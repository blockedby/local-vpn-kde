#!/usr/bin/env python3
"""Create a deterministic, local-only Arch source snapshot and PKGBUILD."""

from __future__ import annotations

import gzip
import hashlib
import os
from pathlib import Path, PurePosixPath
import shutil
import stat
import subprocess
import sys
import tarfile

PACKAGE = "local-vpn-kde-native"
ALLOWLIST = (
    "go.mod",
    "go.sum",
    "cmd/local-vpn-kde",
    "cmd/vibe-vpn",
    "cmd/vibe-vpn-server-op-supervisor",
    "internal",
    "config/openvpn",
    "config/sing-box",
    "scripts/vpnkit/tui",
    "scripts/vpnkit/vpnkit-native.sh",
    "packaging/arch",
)
BANNED_COMPONENTS = {
    ".git",
    ".build",
    "build",
    "generated",
    "log",
    "logs",
    "node_modules",
    "secret",
    "secrets",
}


def fail(message: str) -> "NoReturn":
    raise SystemExit(f"prepare-local-source: {message}")


def git(root: Path, *args: str) -> bytes:
    try:
        return subprocess.check_output(
            ["git", "-C", os.fspath(root), *args], stderr=subprocess.DEVNULL
        )
    except (OSError, subprocess.CalledProcessError):
        fail("Git metadata is unavailable")


def excluded_name(path: PurePosixPath) -> bool:
    for part in path.parts:
        lower = part.lower()
        if lower in BANNED_COMPONENTS or lower.startswith(".git") or ".env" in lower:
            return True
        if lower.endswith(".log"):
            return True
        if lower.startswith("private") and lower.endswith(".keys"):
            return True
    return False


def safe_regular_file(root: Path, relative: PurePosixPath) -> Path | None:
    if relative.is_absolute() or ".." in relative.parts or excluded_name(relative):
        return None
    current = root
    for part in relative.parts:
        current = current / part
        try:
            info = current.lstat()
        except FileNotFoundError:
            fail(f"Git-listed path is missing: {relative}")
        if stat.S_ISLNK(info.st_mode):
            return None
    info = current.lstat()
    if not stat.S_ISREG(info.st_mode):
        return None
    return current


def inventory(root: Path) -> list[tuple[PurePosixPath, Path]]:
    raw = git(
        root,
        "ls-files",
        "--cached",
        "--others",
        "--exclude-standard",
        "-z",
        "--",
        *ALLOWLIST,
    )
    selected: list[tuple[PurePosixPath, Path]] = []
    for item in raw.split(b"\0"):
        if not item:
            continue
        try:
            relative = PurePosixPath(item.decode("utf-8", "strict"))
        except UnicodeDecodeError:
            fail("a selected source path is not UTF-8")
        source = safe_regular_file(root, relative)
        if source is not None:
            selected.append((relative, source))
    selected.sort(key=lambda value: value[0].as_posix())
    required = {
        PurePosixPath("go.mod"),
        PurePosixPath("go.sum"),
        PurePosixPath("packaging/arch/PKGBUILD.template"),
        PurePosixPath("packaging/arch/local-vpn-kde-native.install"),
    }
    present = {relative for relative, _ in selected}
    missing = sorted(path.as_posix() for path in required - present)
    if missing:
        fail("required public sources are absent: " + ", ".join(missing))
    return selected


def content_identity(files: list[tuple[PurePosixPath, Path]]) -> str:
    digest = hashlib.sha256()
    for relative, source in files:
        digest.update(relative.as_posix().encode("utf-8") + b"\0")
        with source.open("rb") as stream:
            while block := stream.read(1024 * 1024):
                digest.update(block)
        digest.update(b"\0")
    return digest.hexdigest()[:12]


def tar_info(name: str, mode: int, epoch: int, directory: bool = False) -> tarfile.TarInfo:
    info = tarfile.TarInfo(name + ("/" if directory and not name.endswith("/") else ""))
    info.type = tarfile.DIRTYPE if directory else tarfile.REGTYPE
    info.mode = mode
    info.uid = 0
    info.gid = 0
    info.uname = "root"
    info.gname = "root"
    info.mtime = epoch
    return info


def write_archive(
    archive: Path,
    prefix: str,
    files: list[tuple[PurePosixPath, Path]],
    epoch: int,
) -> str:
    directories = {prefix}
    for relative, _ in files:
        parent = PurePosixPath(prefix) / relative.parent
        while parent.as_posix() != ".":
            directories.add(parent.as_posix())
            if parent.as_posix() == prefix:
                break
            parent = parent.parent

    with archive.open("xb") as raw:
        with gzip.GzipFile(filename="", mode="wb", fileobj=raw, mtime=epoch) as zipped:
            with tarfile.open(fileobj=zipped, mode="w", format=tarfile.GNU_FORMAT) as tar:
                for directory in sorted(directories):
                    tar.addfile(tar_info(directory, 0o755, epoch, directory=True))
                for relative, source in files:
                    executable = bool(source.stat().st_mode & stat.S_IXUSR)
                    info = tar_info(
                        f"{prefix}/{relative.as_posix()}",
                        0o755 if executable else 0o644,
                        epoch,
                    )
                    info.size = source.stat().st_size
                    with source.open("rb") as stream:
                        tar.addfile(info, stream)
    return hashlib.sha256(archive.read_bytes()).hexdigest()


def render(template: str, pkgver: str, archive: str, checksum: str) -> str:
    replacements = {
        "@PKGVER@": pkgver,
        "@ARCHIVE@": archive,
        "@SHA256@": checksum,
    }
    for marker, value in replacements.items():
        if template.count(marker) != 1:
            fail(f"template marker must occur exactly once: {marker}")
        template = template.replace(marker, value)
    return template


def main() -> None:
    if len(sys.argv) != 2:
        fail("usage: prepare-local-source.py NEW_OUTPUT_DIRECTORY")
    root = Path(__file__).resolve().parents[2]
    output = Path(sys.argv[1]).expanduser()
    if output.exists() or output.is_symlink():
        fail("output path already exists")
    resolved_parent = output.parent.resolve(strict=True)
    resolved_output = resolved_parent / output.name
    try:
        resolved_output.relative_to(root)
    except ValueError:
        pass
    else:
        fail("output directory must be outside the source checkout")

    files = inventory(root)
    commit_count = git(root, "rev-list", "--count", "HEAD").decode().strip()
    short_commit = git(root, "rev-parse", "--short=12", "HEAD").decode().strip()
    epoch_text = git(root, "show", "-s", "--format=%ct", "HEAD").decode().strip()
    if not (commit_count.isdigit() and epoch_text.isdigit()):
        fail("invalid Git revision metadata")
    pkgver = (
        f"0.0.0.r{commit_count}.g{short_commit}.snapshot."
        f"{content_identity(files)}"
    )
    archive_name = f"{PACKAGE}-{pkgver}.tar.gz"
    prefix = f"{PACKAGE}-{pkgver}"

    output.mkdir(mode=0o755)
    archive = output / archive_name
    checksum = write_archive(archive, prefix, files, int(epoch_text))
    template_path = root / "packaging/arch/PKGBUILD.template"
    template = template_path.read_text(encoding="utf-8")
    (output / "PKGBUILD").write_text(
        render(template, pkgver, archive_name, checksum), encoding="utf-8"
    )
    shutil.copyfile(
        root / "packaging/arch/local-vpn-kde-native.install",
        output / "local-vpn-kde-native.install",
    )
    print(f"output={output}")
    print(f"pkgver={pkgver}")
    print(f"source={archive_name}")
    print(f"sha256={checksum}")


if __name__ == "__main__":
    main()
