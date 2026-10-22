#!/usr/bin/env python3
"""Verify matrix provenance/checksums before collecting publishable assets."""
import hashlib
import json
import re
import shutil
import sys
import tarfile
from pathlib import Path
from plan import SHA, version_tuple


def digest(path):
    with path.open('rb') as source:
        return hashlib.file_digest(source, 'sha256').hexdigest()


def verify_manifest(directory, expected, sidecars=()):
    actual = {path.name for path in directory.iterdir()}
    if not expected <= actual or actual - expected - {'SHA256SUMS'} - set(sidecars):
        raise ValueError('Missing or unexpected matrix artifact files')
    if any(not path.is_file() or path.is_symlink() for path in directory.iterdir()):
        raise ValueError('Non-regular artifact refused')
    found = {}
    for line in (directory / 'SHA256SUMS').read_text().splitlines():
        match = re.fullmatch(r'([0-9a-f]{64})  (?:\./)?([^/]+)', line)
        if not match or match[2] not in expected or match[2] in found:
            raise ValueError('Unsafe or duplicate checksum entry')
        found[match[2]] = match[1]
    if found.keys() != expected:
        raise ValueError('Checksum manifest does not cover expected payloads')
    for name, checksum in found.items():
        if digest(directory / name) != checksum:
            raise ValueError('Artifact checksum mismatch')
    return found


def assemble(source, output, version, commit):
    version_tuple(version)
    if not SHA.fullmatch(commit):
        raise ValueError('Expected full source SHA')
    copies = []
    for distro, package in (
        ('arch', f'local-vpn-kde-native-{version}-1-x86_64.pkg.tar.zst'),
        ('debian', f'local-vpn-kde-native_{version}_amd64.deb'),
    ):
        directory = source / f'release-{distro}'
        archive = f'local-vpn-kde-native-{version}.tar.gz'
        expected = {package, archive, 'SOURCE.json', 'BUILDER.txt'}
        hashes = verify_manifest(directory, expected, [package + '.sha256'])
        provenance = json.loads((directory / 'SOURCE.json').read_text())
        if provenance.get('commit') != commit or provenance.get('version') != version:
            raise ValueError('Package source revision/version mismatch')
        if provenance.get('source_sha256') != hashes[archive]:
            raise ValueError('Package source archive mismatch')
        for name in sorted(expected):
            copies.append((directory / name, name if name == package else f'{distro}-{name}'))
    directory = source / 'release-docker'
    archive = f'local-vpn-kde-v{version}-linux-amd64.tar.gz'
    verify_manifest(directory, {archive})
    with tarfile.open(directory / archive) as bundle:
        info = bundle.getmember(f'local-vpn-kde-v{version}-linux-amd64/RELEASE.txt')
        if not info.isfile() or info.size > 8192:
            raise ValueError('Invalid Docker installer provenance')
        content = bundle.extractfile(info).read().decode()
        if f'Commit: {commit}' not in content.splitlines() or f'Tag: v{version}' not in content.splitlines():
            raise ValueError('Docker installer revision/version mismatch')
    copies.append((directory / archive, archive))
    output.mkdir(parents=True, exist_ok=False)
    for path, name in copies:
        shutil.copyfile(path, output / name)
    (output / 'RELEASE.json').write_text(json.dumps({'version':version, 'tag':f'v{version}', 'commit':commit}, indent=2) + '\n')
    (output / 'SHA256SUMS').write_text(''.join(f'{digest(path)}  {path.name}\n' for path in sorted(output.iterdir())))

if __name__ == '__main__':
    assemble(Path(sys.argv[1]), Path(sys.argv[2]), sys.argv[3], sys.argv[4])
