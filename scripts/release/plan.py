#!/usr/bin/env python3
"""Validate an approved merged PR and emit a release plan. No remote writes."""
import json
import re
import subprocess
import sys
from pathlib import Path

VERSION = re.compile(r"(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)")
SHA = re.compile(r"[0-9a-f]{40}")


def version_tuple(value):
    if not VERSION.fullmatch(value):
        raise ValueError("VERSION must contain MAJOR.MINOR.PATCH without v or suffix")
    return tuple(map(int, value.split('.')))


def validate(event, files, version_text, tags, head, repository):
    pr = event.get('pull_request') or {}
    if event.get('action') != 'closed' or pr.get('merged') is not True:
        raise ValueError('Only a merged PR can authorize a release')
    if not any(label.get('name') == 'release' for label in pr.get('labels', [])):
        raise ValueError('PR has no release label')
    if pr.get('base', {}).get('ref') != 'main' or pr.get('base', {}).get('repo', {}).get('full_name') != repository:
        raise ValueError('PR must target main in this repository')
    if event.get('repository', {}).get('full_name') != repository:
        raise ValueError('Event repository mismatch')
    commit = pr.get('merge_commit_sha', '')
    if not SHA.fullmatch(commit) or commit != head:
        raise ValueError('Checkout is not the exact merged PR revision')
    number = pr.get('number')
    if type(number) is not int or number < 1:
        raise ValueError('Invalid PR number')
    if not any(item.get('filename') == 'VERSION' and item.get('status') in ('added', 'modified') for item in files):
        raise ValueError('Release PR must add or change VERSION')
    # Require exactly one line, optionally terminated by a newline.
    version = version_text.removesuffix('\n')
    current = version_tuple(version)
    for tag in tags:
        if tag == f'v{version}':
            raise ValueError('Tag already exists; versions are immutable')
        if tag.startswith('v') and VERSION.fullmatch(tag[1:]) and version_tuple(tag[1:]) >= current:
            raise ValueError('VERSION must exceed every existing stable version')
    return {'version': version, 'tag': f'v{version}', 'commit': commit, 'pr': number}


def main():
    event_path, files_path, repository, output = sys.argv[1:]
    # gh api --paginate --slurp returns an array of pages.
    pages = json.loads(Path(files_path).read_text())
    files = [item for page in pages for item in page]
    git = lambda *args: subprocess.check_output(['git', *args], text=True).strip()
    plan = validate(json.loads(Path(event_path).read_text()), files,
                    subprocess.check_output(['git', 'show', 'HEAD:VERSION'], text=True),
                    git('tag', '--list').splitlines(), git('rev-parse', 'HEAD'), repository)
    Path(output).write_text(json.dumps(plan, indent=2) + '\n')
    print('\n'.join(f'{key}={value}' for key, value in plan.items()))

if __name__ == '__main__':
    main()
