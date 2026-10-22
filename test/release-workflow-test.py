#!/usr/bin/env python3
"""Release control-plane regression tests; GitHub writes are always mocked."""
import copy
import hashlib
import io
import json
from pathlib import Path
import shutil
import sys
import tarfile
import tempfile
import unittest
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / 'scripts/release'))
import plan
import assemble
import publish

COMMIT = 'a' * 40
REPO = 'blockedby/local-vpn-kde'
EVENT = {'action':'closed','repository':{'full_name':REPO},'pull_request':{
    'number':18, 'merged':True, 'labels':[{'name':'release'}],
    'base':{'ref':'main','repo':{'full_name':REPO}}, 'merge_commit_sha':COMMIT,
}}
FILES = [{'filename':'VERSION','status':'modified'}]


def checksums(directory):
    (directory/'SHA256SUMS').write_text(''.join(f'{assemble.digest(p)}  {p.name}\n' for p in sorted(directory.iterdir()) if p.name != 'SHA256SUMS'))


def fixtures(root):
    for distro, package in [('arch','local-vpn-kde-native-0.2.0-1-x86_64.pkg.tar.zst'),('debian','local-vpn-kde-native_0.2.0_amd64.deb')]:
        directory = root/f'release-{distro}'
        directory.mkdir()
        (directory/package).write_bytes(b'package fixture')
        source = directory/'local-vpn-kde-native-0.2.0.tar.gz'
        source.write_bytes(b'curated source fixture')
        (directory/'SOURCE.json').write_text(json.dumps({'version':'0.2.0','commit':COMMIT,'source_sha256':assemble.digest(source)}))
        (directory/'BUILDER.txt').write_text('sha256:'+'b'*64+'\n')
        checksums(directory)
    directory = root/'release-docker'; directory.mkdir()
    with tarfile.open(directory/'local-vpn-kde-v0.2.0-linux-amd64.tar.gz','w:gz') as archive:
        content=f'Tag: v0.2.0\nCommit: {COMMIT}\n'.encode()
        info=tarfile.TarInfo('local-vpn-kde-v0.2.0-linux-amd64/RELEASE.txt'); info.size=len(content)
        archive.addfile(info,io.BytesIO(content))
    checksums(directory)


class ReleasePlanTest(unittest.TestCase):
    def validate(self,event=EVENT,files=FILES,version='0.2.0\n',tags=('v0.1.0',),head=COMMIT):
        return plan.validate(event,files,version,tags,head,REPO)

    def test_merged_labelled_pr_uses_exact_revision(self):
        self.assertEqual(self.validate(),{'version':'0.2.0','tag':'v0.2.0','commit':COMMIT,'pr':18})

    def test_events_without_release_authorization_are_rejected(self):
        for field,value in [('merged',False),('labels',[]),('merge_commit_sha','b'*40),('number','18')]:
            with self.subTest(field=field):
                event=copy.deepcopy(EVENT); event['pull_request'][field]=value
                with self.assertRaises(ValueError): self.validate(event=event)
        for key,value in [('ref','dev'),('repo',{'full_name':'other/repo'})]:
            event=copy.deepcopy(EVENT); event['pull_request']['base'][key]=value
            with self.assertRaises(ValueError): self.validate(event=event)
        event=copy.deepcopy(EVENT); event['action']='labeled'
        with self.assertRaises(ValueError): self.validate(event=event)

    def test_version_must_change_and_be_new_stable_semver(self):
        with self.assertRaises(ValueError): self.validate(files=[])
        for version in ['v0.2.0','0.2','0.02.0','0.2.0-rc1','0.2.0\n\n','0.2.0\ncommit=evil',' 0.2.0']:
            with self.subTest(version=version),self.assertRaises(ValueError): self.validate(version=version)
        for tags in [('v0.2.0',),('v0.3.0',),('v1.0.0',)]:
            with self.assertRaises(ValueError): self.validate(tags=tags)


class ReleaseArtifactsTest(unittest.TestCase):
    def setUp(self):
        self.tmp=tempfile.TemporaryDirectory(); self.addCleanup(self.tmp.cleanup)
        self.root=Path(self.tmp.name); self.inputs=self.root/'inputs'; self.inputs.mkdir()
        fixtures(self.inputs)
        self.output=self.root/'output'

    def assemble(self):
        assemble.assemble(self.inputs,self.output,'0.2.0',COMMIT)

    def test_complete_matrix_preserves_unique_packages_and_provenance(self):
        self.assemble()
        self.assertTrue((self.output/'arch-SOURCE.json').exists())
        self.assertTrue((self.output/'debian-SOURCE.json').exists())
        assemble.verify_manifest(self.output,{p.name for p in self.output.iterdir() if p.name!='SHA256SUMS'})
        with self.assertRaises(FileExistsError): self.assemble()

    def test_changed_bytes_are_rejected_before_output_creation(self):
        (self.inputs/'release-arch/BUILDER.txt').write_text('tampered')
        with self.assertRaises(ValueError): self.assemble()
        self.assertFalse(self.output.exists())

    def test_valid_checksums_cannot_hide_wrong_source_commit(self):
        directory=self.inputs/'release-debian'
        p=directory/'SOURCE.json'; data=json.loads(p.read_text()); data['commit']='c'*40; p.write_text(json.dumps(data)); checksums(directory)
        with self.assertRaises(ValueError): self.assemble()
        self.assertFalse(self.output.exists())

    def test_source_digest_must_match_archive(self):
        directory=self.inputs/'release-debian'
        p=directory/'SOURCE.json'; data=json.loads(p.read_text()); data['source_sha256']='c'*64; p.write_text(json.dumps(data)); checksums(directory)
        with self.assertRaises(ValueError): self.assemble()

    def test_manifest_traversal_and_symlinks_are_rejected(self):
        directory=self.inputs/'release-arch'
        p=directory/'SHA256SUMS'; p.write_text(p.read_text().replace('  BUILDER.txt','  ../BUILDER.txt'))
        with self.assertRaises(ValueError): self.assemble()
        checksums(directory)
        (directory/'BUILDER.txt').unlink(); (directory/'BUILDER.txt').symlink_to('/etc/passwd')
        with self.assertRaises(ValueError): self.assemble()

    def test_publisher_stops_before_write_on_existing_tag_release_or_api_error(self):
        self.assemble()
        for existing in ['tag','release','api-error']:
            calls=[]
            def gh(*args,**kwargs):
                calls.append((args,kwargs))
                if existing=='api-error': raise RuntimeError('network unavailable')
                if 'releases' in args[-1]: return json.dumps([[{'tag_name':'v0.2.0'}]] if existing=='release' else [[]])
                return json.dumps([{'ref':'refs/tags/v0.2.0'}])
            with self.subTest(existing=existing),patch.object(publish,'gh',gh),self.assertRaises((ValueError,RuntimeError)):
                publish.publish(self.output,'0.2.0',COMMIT,'18',REPO)
            self.assertFalse(any('POST' in args or args[:2]==('release','create') for args,_ in calls))

    def test_publish_downloads_and_verifies_before_making_draft_public(self):
        self.assemble()
        for corrupt in [False,True]:
            calls=[]
            def gh(*args,**kwargs):
                calls.append(args)
                if args[0]=='api':
                    if args[-1].endswith('/releases'): return '[[]]'
                    if 'matching-refs' in args[-1]: return '[]'
                    if args[-1].endswith('/git/tags'): return json.dumps({'sha':'d'*40})
                    if '/commits/' in args[-1]: return json.dumps({'sha':COMMIT})
                    return '{}'
                if args[:2]==('release','download'):
                    destination=Path(args[args.index('--dir')+1]); shutil.copytree(self.output,destination)
                    if corrupt: (destination/'RELEASE.json').write_text('{}')
                return ''
            with self.subTest(corrupt=corrupt),patch.object(publish,'gh',gh):
                if corrupt:
                    with self.assertRaises(ValueError): publish.publish(self.output,'0.2.0',COMMIT,'18',REPO)
                else: publish.publish(self.output,'0.2.0',COMMIT,'18',REPO)
            edits=[args for args in calls if args[:2]==('release','edit')]
            self.assertEqual(len(edits),0 if corrupt else 1)
            creates=[args for args in calls if args[:2]==('release','create')]
            self.assertIn('--draft',creates[0]); self.assertIn('--verify-tag',creates[0])
            self.assertFalse(any('--clobber' in args or 'PATCH' in args for args in calls))

if __name__ == '__main__':
    unittest.main()
