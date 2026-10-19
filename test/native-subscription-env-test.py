#!/usr/bin/env python3
import importlib.util
import os
from pathlib import Path
import tempfile
import unittest

spec = importlib.util.spec_from_file_location("native_subscription_env", Path(__file__).with_name("native-subscription-env.py"))
env = importlib.util.module_from_spec(spec)
spec.loader.exec_module(env)


class SubscriptionEnvTests(unittest.TestCase):
    def test_literal_formats(self):
        for value in ["https://example.invalid/sub?token=fixture", "'https://example.invalid/sub?token=fixture' # ignored", '"https://example.invalid/sub?token=fixture"']:
            self.assertEqual(env.extract((env.KEY + "=" + value).encode()), "https://example.invalid/sub?token=fixture")
        self.assertEqual(env.extract(b"export VPNKIT_LAB_SUBSCRIPTION_URL=https://example.invalid/$(id)"), "https://example.invalid/$(id)")

    def test_invalid_inputs(self):
        for data in [b"", b"VPNKIT_LAB_SUBSCRIPTION_URL=http://example.invalid", b"VPNKIT_LAB_SUBSCRIPTION_URL=https://user:secret@example.invalid", b"VPNKIT_LAB_SUBSCRIPTION_URL=https://example.invalid/#secret", b"VPNKIT_LAB_SUBSCRIPTION_URL='unterminated", b"VPNKIT_LAB_SUBSCRIPTION_URL=https://example.invalid\nVPNKIT_LAB_SUBSCRIPTION_URL=https://example.invalid", b"VPNKIT_LAB_SUBSCRIPTION_URL=https://example.invalid/a b", b"VPNKIT_LAB_SUBSCRIPTION_URL=https://example.invalid:bad"]:
            with self.subTest(data=data), self.assertRaises(ValueError):
                env.extract(data)

    def test_secure_copy_and_no_overwrite(self):
        with tempfile.TemporaryDirectory() as directory:
            source = Path(directory) / ".env"
            destination = Path(directory) / "sub_url"
            source.write_text("OTHER=$(false)\nVPNKIT_LAB_SUBSCRIPTION_URL=https://example.invalid/fixture\n")
            source.chmod(0o600)
            env.copy_subscription(str(source), str(destination))
            self.assertEqual(destination.read_text(), "https://example.invalid/fixture\n")
            self.assertEqual(destination.stat().st_mode & 0o777, 0o600)
            with self.assertRaises(OSError):
                env.copy_subscription(str(source), str(destination))

    def test_unsafe_sources(self):
        with tempfile.TemporaryDirectory() as directory:
            source = Path(directory) / ".env"
            destination = Path(directory) / "out"
            source.write_text("VPNKIT_LAB_SUBSCRIPTION_URL=https://example.invalid/fixture\n")
            source.chmod(0o644)
            with self.assertRaises(ValueError):
                env.copy_subscription(str(source), str(destination))
            source.chmod(0o600)
            linked = Path(directory) / "link"
            linked.symlink_to(source)
            with self.assertRaises(OSError):
                env.copy_subscription(str(linked), str(destination))
            linked.unlink()
            os.link(source, linked)
            with self.assertRaises(ValueError):
                env.copy_subscription(str(source), str(destination))
            self.assertFalse(destination.exists())

    def test_fifo_rejected_without_blocking(self):
        with tempfile.TemporaryDirectory() as directory:
            source = Path(directory) / "fifo"
            os.mkfifo(source, 0o600)
            with self.assertRaises(ValueError):
                env.copy_subscription(str(source), str(Path(directory) / "out"))


if __name__ == "__main__":
    unittest.main()
