#!/usr/bin/env python3
from __future__ import annotations

import hashlib
from pathlib import Path
import shutil
import subprocess
import tarfile
import tempfile
import unittest

HERE = Path(__file__).resolve().parent


class PackagingTests(unittest.TestCase):
    def fixture(self, base: Path) -> Path:
        root = base / "repo"
        (root / "packaging").mkdir(parents=True)
        shutil.copytree(HERE, root / "packaging/arch", symlinks=True)
        (root / "cmd/local-vpn-kde").mkdir(parents=True)
        (root / "internal/example").mkdir(parents=True)
        (root / "scripts/vpnkit/tui/node_modules/private").mkdir(parents=True)
        (root / "config/openvpn").mkdir(parents=True)
        (root / "go.mod").write_text("module github.com/blockedby/local-vpn-kde\n\ngo 1.22\n")
        (root / "go.sum").write_text("")
        (root / "cmd/local-vpn-kde/main.go").write_text("package main\nfunc main() {}\n")
        (root / "internal/example/code.go").write_text("package example\n")
        (root / "config/openvpn/public.template").write_text("public\n")
        (root / "scripts/vpnkit/tui/index.ts").write_text("console.log('public')\n")
        (root / "scripts/vpnkit/tui/node_modules/private/value").write_text("ignored\n")
        (root / "config/openvpn/local.env.example").write_text("not-a-real-secret\n")
        (root / "config/openvpn/private-test.keys").write_text("fixture-only\n")
        (root / "config/openvpn/link").symlink_to("public.template")
        (root / ".gitignore").write_text("node_modules/\n")
        subprocess.run(["git", "init", "-q", root], check=True)
        subprocess.run(["git", "-C", root, "config", "user.name", "Packaging Test"], check=True)
        subprocess.run(["git", "-C", root, "config", "user.email", "test@example.invalid"], check=True)
        subprocess.run(["git", "-C", root, "add", "."], check=True)
        subprocess.run(
            ["git", "-C", root, "add", "-f", "config/openvpn/local.env.example"],
            check=True,
        )
        subprocess.run(["git", "-C", root, "commit", "-qm", "fixture"], check=True)
        return root

    def test_snapshot_is_deterministic_and_excludes_sensitive_classes(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            base = Path(temporary)
            root = self.fixture(base)
            outputs = [base / "out-one", base / "out-two"]
            for output in outputs:
                subprocess.run(
                    ["python3", root / "packaging/arch/prepare-local-source.py", output],
                    check=True,
                    stdout=subprocess.PIPE,
                    text=True,
                )
            archives = [next(output.glob("*.tar.gz")) for output in outputs]
            self.assertEqual(archives[0].name, archives[1].name)
            self.assertEqual(archives[0].read_bytes(), archives[1].read_bytes())

            checksum = hashlib.sha256(archives[0].read_bytes()).hexdigest()
            pkgbuild = (outputs[0] / "PKGBUILD").read_text()
            self.assertIn(f"sha256sums=('{checksum}')", pkgbuild)
            self.assertNotIn("@PKGVER@", pkgbuild)
            with tarfile.open(archives[0], "r:gz") as archive:
                members = archive.getmembers()
            names = [member.name.lower() for member in members]
            self.assertTrue(all(member.isfile() or member.isdir() for member in members))
            self.assertFalse(any("node_modules" in name for name in names))
            self.assertFalse(any("/.git" in name for name in names))
            self.assertFalse(any(".env" in name for name in names))
            self.assertFalse(any("private-test.keys" in name for name in names))
            self.assertFalse(any(name.endswith("/link") for name in names))

    def test_existing_output_is_refused(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            base = Path(temporary)
            root = self.fixture(base)
            output = base / "existing"
            output.mkdir()
            result = subprocess.run(
                ["python3", root / "packaging/arch/prepare-local-source.py", output],
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                text=True,
            )
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("already exists", result.stderr)

    def test_template_has_bounded_runtime_contract(self) -> None:
        template = (HERE / "PKGBUILD.template").read_text()
        self.assertIn("arch=('x86_64')", template)
        self.assertIn("'sing-box>=1.13'", template)
        self.assertIn("'sing-box<1.15'", template)
        self.assertNotIn("'docker'", template.lower())
        self.assertNotIn("run.sh", template)
        self.assertIn("go run ./packaging/arch/generate-units", template)
        self.assertIn("bun install --frozen-lockfile --production", template)
        hook = (HERE / "local-vpn-kde-native.install").read_text()
        transaction = (HERE / "local-vpn-kde-native.hook").read_text()
        self.assertIn("When = PreTransaction", transaction)
        self.assertIn("AbortOnFail", transaction)
        self.assertIn("native-package --action pre-remove", transaction)
        self.assertIn("Operation = Remove", transaction)
        self.assertNotIn("Operation = Upgrade", transaction)
        upgrade = (HERE / "local-vpn-kde-native-upgrade.hook").read_text()
        self.assertIn("Operation = Upgrade", upgrade)
        self.assertIn("AbortOnFail", upgrade)
        self.assertIn("--action pre-upgrade", upgrade)
        self.assertIn("usr/share/libalpm/hooks", template)
        self.assertNotIn("systemctl enable", hook)
        self.assertNotIn("systemctl start", hook)
        self.assertNotIn("systemctl restart", hook)


if __name__ == "__main__":
    unittest.main()
