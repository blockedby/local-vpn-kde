#!/usr/bin/env python3
"""Ensure dpkg cannot bypass a failed safety guard via failed-upgrade."""
from pathlib import Path
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[1]

class DebianHooksTest(unittest.TestCase):
    def test_upgrade_and_dpkg_fallback_preserve_guard_failure(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            backend = root / "backend"
            backend.write_text('#!/bin/sh\nprintf "%s\\n" "$*" >> "$CALL_LOG"\nexit 23\n')
            backend.chmod(0o755)
            script = root / "prerm"
            script.write_text((ROOT / 'packaging/debian/prerm').read_text().replace('/usr/libexec/local-vpn-kde/local-vpn-kde.bin',str(backend)))
            for action, expected in [('upgrade','pre-upgrade'),('failed-upgrade','pre-upgrade'),('remove','pre-remove'),('deconfigure','pre-remove')]:
                with self.subTest(action=action):
                    log = root / 'calls'
                    log.write_text('')
                    result = subprocess.run(['/bin/sh', str(script), action, '0.2.0'], env={'CALL_LOG':str(log)})
                    self.assertEqual(result.returncode,23)
                    self.assertEqual(log.read_text(),f'native-package --action {expected}\n')

if __name__ == '__main__':
    unittest.main()
