#!/usr/bin/env python3
"""Curated public snapshot shared by Arch and Debian candidate builds."""
import importlib.util
import re
import shutil
import sys
from pathlib import Path

root = Path(__file__).resolve().parents[2]
spec = importlib.util.spec_from_file_location("arch_source", root / "packaging/arch/prepare-local-source.py")
source = importlib.util.module_from_spec(spec)
spec.loader.exec_module(source)
if len(sys.argv) != 3 or not re.fullmatch(r"[0-9]+\.[0-9]+\.[0-9]+(?:~?rc[1-9][0-9]*)?", sys.argv[1]):
    raise SystemExit("Usage: prepare-source.py VERSION NEW_OUTPUT_DIRECTORY")
version, output = sys.argv[1], Path(sys.argv[2]).resolve()
if output.exists() or output.is_relative_to(root):
    raise SystemExit("Output must be a new directory outside the checkout")
source.ALLOWLIST += ("packaging/debian", "packaging/portable", "cmd/vpnkit-portable", "install-native.sh", "test/compiled-tui-installed-test.py", "test/native-debian-package-installed-test.py")
files = source.inventory(root)
output.mkdir(parents=True)
epoch = int(source.git(root, "show", "-s", "--format=%ct", "HEAD"))
archive = f"local-vpn-kde-native-{version}.tar.gz"
checksum = source.write_archive(output / archive, f"local-vpn-kde-native-{version}", files, epoch)
(output / "PKGBUILD").write_text(source.render((root / "packaging/arch/PKGBUILD.template").read_text(), version, archive, checksum))
shutil.copyfile(root / "packaging/arch/local-vpn-kde-native.install", output / "local-vpn-kde-native.install")
(output / "SOURCE.json").write_text(__import__('json').dumps({"commit":source.git(root,"rev-parse","HEAD").decode().strip(),"version":version,"source_sha256":checksum},indent=2)+'\n')
