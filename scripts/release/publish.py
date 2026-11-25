#!/usr/bin/env python3
"""Publish only verified assets. No tag updates, asset replacement or retries."""
import json
import os
import re
import subprocess
import sys
import tempfile
from pathlib import Path
from assemble import verify_manifest
from plan import SHA, VERSION, check_notes, notes_path, version_tuple


def gh(*args, payload=None):
    command = ['gh', *args]
    if payload is not None:
        command += ['--input', '-']
    return subprocess.check_output(command, input=None if payload is None else json.dumps(payload).encode()).decode()


def publish(assets, version, commit, pr, repository, notes_text):
    current = version_tuple(version)
    notes_text = check_notes(notes_text)
    if not SHA.fullmatch(commit) or not re.fullmatch(r'[1-9][0-9]*', pr):
        raise ValueError('Invalid release commit/PR')
    if not re.fullmatch(r'[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+', repository):
        raise ValueError('Invalid repository')
    tag = f'v{version}'
    expected = {
        f'local-vpn-kde-native-{version}-1-x86_64.pkg.tar.zst',
        f'local-vpn-kde-native_{version}_amd64.deb',
        f'local-vpn-kde-v{version}-linux-amd64.tar.gz',
        'RELEASE.json',
        *[f'{distro}-{name}' for distro in ('arch', 'debian') for name in
          ('SOURCE.json', 'BUILDER.txt', f'local-vpn-kde-native-{version}.tar.gz')],
    }
    verify_manifest(assets, expected)
    if json.loads((assets/'RELEASE.json').read_text()) != {'version':version,'tag':tag,'commit':commit}:
        raise ValueError('Release metadata mismatch')
    # API errors abort. Absence is established from successful enumerations,
    # never inferred from an arbitrary failed gh release view call.
    releases = json.loads(gh('api', '--paginate', '--slurp', f'repos/{repository}/releases'))
    if any(item['tag_name'] == tag for page in releases for item in page):
        raise ValueError('Release already exists; refusing replacement')
    refs = json.loads(gh('api', f'repos/{repository}/git/matching-refs/tags/'))
    for ref in refs:
        name = ref['ref'].removeprefix('refs/tags/')
        if name == tag or (name.startswith('v') and VERSION.fullmatch(name[1:]) and version_tuple(name[1:]) >= current):
            raise ValueError('Version is already used or superseded')
    # Atomic create: racing publishers receive an error instead of moving refs.
    annotated = json.loads(gh('api', '--method', 'POST', f'repos/{repository}/git/tags', payload={
        'tag':tag, 'message':f'{tag} approved by merged release PR #{pr}', 'object':commit, 'type':'commit',
    }))
    gh('api', '--method', 'POST', f'repos/{repository}/git/refs', payload={'ref':f'refs/tags/{tag}', 'sha':annotated['sha']})
    resolved = json.loads(gh('api', f'repos/{repository}/commits/{tag}'))
    if resolved['sha'] != commit:
        raise ValueError('Created tag does not resolve to approved revision')
    with tempfile.TemporaryDirectory(prefix='vpnkit-publish-') as temporary:
        work = Path(temporary)
        notes = work / 'notes.md'
        notes.write_text(notes_text + f'''
---
Revision `{commit}` · approved release PR #{pr} · verify downloads with `SHA256SUMS`.
''')
        gh('release', 'create', tag, '--repo', repository, '--verify-tag', '--draft', '--title', tag,
           '--notes-file', str(notes), *[str(path) for path in sorted(assets.iterdir())])
        download = work / 'download'
        gh('release', 'download', tag, '--repo', repository, '--dir', str(download))
        # Check downloaded bytes against our original manifest, not a remote
        # manifest that could have been changed along with the uploaded data.
        if (download/'SHA256SUMS').read_bytes() != (assets/'SHA256SUMS').read_bytes():
            raise ValueError('Uploaded checksum manifest mismatch')
        verify_manifest(download, expected)
        gh('release', 'edit', tag, '--repo', repository, '--draft=false', '--latest')

if __name__ == '__main__':
    notes_file = Path(notes_path(sys.argv[2]))
    publish(Path(sys.argv[1]), sys.argv[2], sys.argv[3], sys.argv[4], os.environ['GH_REPO'],
            notes_file.read_text() if notes_file.is_file() else None)
