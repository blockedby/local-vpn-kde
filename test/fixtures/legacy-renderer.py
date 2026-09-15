#!/usr/bin/env python3
"""Descriptor-relative, atomic writer for the local KDE renderer.

The shell adapter validates the local-only policy boundary.  This helper owns
all destination mutations: it never reopens a destination by pathname after
validation. Destination directories are held as O_DIRECTORY|O_NOFOLLOW
file descriptors, and every regular output is staged and replaced relative to
its held directory descriptor.
"""
from __future__ import annotations

import json
import os
import secrets
import stat
import subprocess
import sys
from contextlib import ExitStack
from typing import Optional


CURRENT_UID = os.getuid()


def _require_descriptor_apis() -> None:
    required = ("O_DIRECTORY", "O_NOFOLLOW")
    if any(not hasattr(os, name) for name in required):
        raise RuntimeError("descriptor-relative directory APIs are unavailable")
    if os.open not in os.supports_dir_fd or os.mkdir not in os.supports_dir_fd:
        raise RuntimeError("descriptor-relative directory operations are unavailable")
    # CPython exposes replace() as a wrapper around rename(); the capability
    # set names the underlying rename primitive rather than the wrapper.
    if os.rename not in os.supports_dir_fd or os.unlink not in os.supports_dir_fd:
        raise RuntimeError("descriptor-relative rename operations are unavailable")


_require_descriptor_apis()

DIR_FLAGS = (
    os.O_RDONLY
    | os.O_DIRECTORY
    | os.O_NOFOLLOW
    | getattr(os, "O_CLOEXEC", 0)
)
READ_FLAGS = os.O_RDONLY | os.O_NOFOLLOW | getattr(os, "O_CLOEXEC", 0)
STAGE_FLAGS = (
    os.O_WRONLY
    | os.O_CREAT
    | os.O_EXCL
    | os.O_NOFOLLOW
    | getattr(os, "O_CLOEXEC", 0)
)


class RenderError(RuntimeError):
    """A renderer failure that must leave the external tree untouched."""


def _components(path: str) -> list[str]:
    if not path or not os.path.isabs(path):
        raise RenderError("renderer paths must be absolute")
    components = []
    for component in path.split("/"):
        if not component:
            continue
        if component in (".", ".."):
            raise RenderError("renderer paths must be canonical")
        components.append(component)
    return components


def _assert_directory(fd: int, *, owned: bool) -> os.stat_result:
    result = os.fstat(fd)
    if not stat.S_ISDIR(result.st_mode):
        raise RenderError("renderer destination component is not a directory")
    if owned and result.st_uid != CURRENT_UID:
        raise RenderError("renderer destination directory is not user-owned")
    return result


def _open_rooted_dir(path: str, *, create: bool, owned: bool) -> int:
    """Open an absolute directory one component at a time from the root fd."""

    fd = os.open("/", DIR_FLAGS)
    try:
        for component in _components(path):
            try:
                child = os.open(component, DIR_FLAGS, dir_fd=fd)
            except FileNotFoundError:
                if not create:
                    raise
                try:
                    os.mkdir(component, 0o700, dir_fd=fd)
                except FileExistsError:
                    # Re-open below with O_NOFOLLOW.  A concurrent symlink or
                    # non-directory therefore fails closed rather than being
                    # accepted by the mkdir race.
                    pass
                child = os.open(component, DIR_FLAGS, dir_fd=fd)
            try:
                _assert_directory(child, owned=False)
            except BaseException:
                os.close(child)
                raise
            os.close(fd)
            fd = child
        _assert_directory(fd, owned=owned)
        return fd
    except BaseException:
        os.close(fd)
        raise


def _open_child_dir(parent_fd: int, name: str, *, owned: bool = True) -> int:
    if not name or "/" in name or name in (".", ".."):
        raise RenderError("invalid renderer directory component")
    try:
        child = os.open(name, DIR_FLAGS, dir_fd=parent_fd)
    except FileNotFoundError:
        try:
            os.mkdir(name, 0o700, dir_fd=parent_fd)
        except FileExistsError:
            pass
        child = os.open(name, DIR_FLAGS, dir_fd=parent_fd)
    try:
        _assert_directory(child, owned=owned)
    except BaseException:
        os.close(child)
        raise
    return child


def _prepare_directory(fd: int) -> None:
    """Apply directory permissions through the held fd and make them durable."""

    _assert_directory(fd, owned=True)
    os.fchmod(fd, 0o700)
    os.fsync(fd)


def _open_rooted_regular(path: str) -> int:
    """Open a source regular file without following any path component."""

    if "/" not in path:
        raise RenderError("source path has no parent directory")
    parent_path, name = path.rsplit("/", 1)
    if not name or name in (".", ".."):
        raise RenderError("invalid source file name")
    parent_fd = _open_rooted_dir(parent_path or "/", create=False, owned=False)
    try:
        return _open_regular_at(parent_fd, name)
    finally:
        os.close(parent_fd)


def _open_regular_at(parent_fd: int, name: str) -> int:
    if not name or "/" in name or name in (".", ".."):
        raise RenderError("invalid source file name")
    fd = os.open(name, READ_FLAGS, dir_fd=parent_fd)
    result = os.fstat(fd)
    if not stat.S_ISREG(result.st_mode):
        os.close(fd)
        raise RenderError("renderer source is not a regular file")
    return fd


def _read_fd(fd: int) -> bytes:
    chunks: list[bytes] = []
    while True:
        chunk = os.read(fd, 1024 * 1024)
        if not chunk:
            return b"".join(chunks)
        chunks.append(chunk)


def _read_rooted_regular(path: str) -> bytes:
    fd = _open_rooted_regular(path)
    try:
        return _read_fd(fd)
    finally:
        os.close(fd)


def _read_optional_at(parent_fd: int, name: str) -> Optional[bytes]:
    try:
        fd = _open_regular_at(parent_fd, name)
    except FileNotFoundError:
        return None
    try:
        return _read_fd(fd)
    finally:
        os.close(fd)


def _assert_private_stage(fd: int) -> None:
    result = os.fstat(fd)
    if (
        not stat.S_ISREG(result.st_mode)
        or result.st_nlink != 1
        or result.st_uid != CURRENT_UID
    ):
        raise RenderError("renderer staging inode is not a private single-link regular file")


def _write_all(fd: int, data: bytes) -> None:
    view = memoryview(data)
    while view:
        written = os.write(fd, view)
        if written <= 0:
            raise RenderError("renderer staging write made no progress")
        view = view[written:]


def _verify_replaced_inode(staged_fd: int, dir_fd: int, name: str) -> None:
    """Ensure renameat installed the inode that was staged, not a swapped name."""

    staged = os.fstat(staged_fd)
    final_fd = os.open(name, READ_FLAGS, dir_fd=dir_fd)
    try:
        final = os.fstat(final_fd)
        if (
            not stat.S_ISREG(final.st_mode)
            or final.st_uid != CURRENT_UID
            or final.st_nlink != 1
            or (final.st_dev, final.st_ino) != (staged.st_dev, staged.st_ino)
        ):
            raise RenderError("renderer final inode changed during replacement")
    finally:
        os.close(final_fd)


def _atomic_write(dir_fd: int, name: str, data: bytes, mode: int = 0o600) -> None:
    """Write one output through a same-directory exclusive staging inode."""

    if not name or "/" in name or name in (".", ".."):
        raise RenderError("invalid renderer output name")
    stage = f".vpnkit-render-{os.getpid()}-{secrets.token_hex(12)}.tmp"
    renamed = False
    fd: Optional[int] = None
    try:
        # O_EXCL makes the stage creation independent of any pathname check;
        # O_NOFOLLOW also protects filesystems that report an unusual race.
        fd = os.open(stage, STAGE_FLAGS, mode, dir_fd=dir_fd)
        _assert_private_stage(fd)
        _write_all(fd, data)
        os.fchmod(fd, mode)
        os.fsync(fd)
        # Check after the complete write and immediately before renameat.  A
        # hardlink inserted by a same-UID observer is therefore rejected.
        _assert_private_stage(fd)

        # Both names are relative to the held directory fd.  renameat replaces
        # a final symlink entry itself; it never follows that symlink. Keep the
        # staged descriptor open through the replacement and directory fsync.
        os.replace(stage, name, src_dir_fd=dir_fd, dst_dir_fd=dir_fd)
        renamed = True
        os.fsync(dir_fd)
        _assert_private_stage(fd)
        _verify_replaced_inode(fd, dir_fd, name)
    finally:
        if fd is not None:
            os.close(fd)
        if not renamed:
            try:
                os.unlink(stage, dir_fd=dir_fd)
            except FileNotFoundError:
                pass


def _read_rule_sources(rule_source_path: str) -> list[tuple[str, bytes]]:
    source_fd = _open_rooted_dir(rule_source_path, create=False, owned=False)
    try:
        names = sorted(name for name in os.listdir(source_fd) if name.endswith(".json"))
        result: list[tuple[str, bytes]] = []
        for name in names:
            fd = _open_regular_at(source_fd, name)
            try:
                result.append((name, _read_fd(fd)))
            finally:
                os.close(fd)
        return result
    finally:
        os.close(source_fd)


def _run_race_hook(base: str) -> None:
    hook = os.environ.get("VPNKIT_LOCAL_RENDER_RACE_HOOK", "")
    if not hook:
        return
    if os.environ.get("VPNKIT_LOCAL_TEST_FIXTURE") != "1":
        raise RenderError("renderer race hook requires an explicit test fixture")
    if not os.path.isabs(hook):
        raise RenderError("renderer race hook must be an absolute executable")
    try:
        hook_stat = os.stat(hook, follow_symlinks=False)
    except OSError as exc:
        raise RenderError("renderer race hook is unavailable") from exc
    if not stat.S_ISREG(hook_stat.st_mode) or not os.access(hook, os.X_OK):
        raise RenderError("renderer race hook is not an executable regular file")
    environment = os.environ.copy()
    environment["VPNKIT_LOCAL_SECRETS_DIR"] = base
    try:
        subprocess.run([hook], check=True, env=environment, close_fds=True)
    except (OSError, subprocess.CalledProcessError) as exc:
        raise RenderError("renderer race hook failed") from exc


def _build_config(template_path: str, policy: str, ruleset_mode: str, selected_outbound_mode: str) -> bytes:
    # This placeholder is validated but never started in local real mode:
    # entrypoint must replace it with a benchmarked subscription node before
    # sing-box/OpenVPN. Disposable acceptance explicitly requests a direct
    # outbound so it can test the server data path without private subscription
    # material.
    if selected_outbound_mode == "direct-fixture":
        bootstrap = {"type": "direct", "tag": "selected-native-out"}
    else:
        bootstrap = {
            "type": "vless",
            "tag": "selected-native-out",
            "server": "192.0.2.1",
            "server_port": 443,
            "uuid": "00000000-0000-4000-8000-000000000000",
            "tls": {"enabled": True, "server_name": "bootstrap.example.invalid"},
        }
    remote_sets = [
        {
            "type": "remote",
            "tag": "geoip-ru",
            "format": "binary",
            "url": "https://raw.githubusercontent.com/runetfreedom/russia-v2ray-rules-dat/release/sing-box/rule-set-geoip/geoip-ru.srs",
            "download_detour": "direct-out",
        },
        {
            "type": "remote",
            "tag": "geosite-category-ru",
            "format": "binary",
            "url": "https://raw.githubusercontent.com/runetfreedom/russia-v2ray-rules-dat/release/sing-box/rule-set-geosite/geosite-category-ru.srs",
            "download_detour": "direct-out",
        },
    ]
    local_sets = [
        {"type": "local", "tag": "geoip-ru", "format": "source", "path": "/etc/sing-box/rule-sets/geoip-ru.json"},
        {"type": "local", "tag": "geosite-category-ru", "format": "source", "path": "/etc/sing-box/rule-sets/geosite-category-ru.json"},
    ]
    selected_sets = local_sets if ruleset_mode == "local-fixture" else remote_sets

    template = _read_rooted_regular(template_path).decode("utf-8")
    text = template.replace("{{SELECTED_NATIVE_OUT_JSON}}", json.dumps(bootstrap, indent=4))
    text = text.replace("{{RU_RULE_SETS_JSON}}", ",\n".join(json.dumps(item, indent=8) for item in selected_sets))
    config = json.loads(text)

    # sing-box 1.13 does not provide a DNS-server health selector. The local
    # Compose watchdog probes these two tagged providers through the local
    # SOCKS inbound and changes only dns.final in the persisted runtime config.
    dns_servers = {server.get("tag"): server for server in config.get("dns", {}).get("servers", [])}
    for tag in ("remote-dns", "remote-dns-fallback"):
        if tag not in dns_servers:
            raise RenderError(f"sing-box template is missing required DNS server tag: {tag}")
    dns_servers["remote-dns"].update(
        {
            "type": "https",
            "server": "1.1.1.1",
            "server_port": 443,
            "path": "/dns-query",
            "tls": {"enabled": True, "server_name": "cloudflare-dns.com"},
        }
    )
    dns_servers["remote-dns-fallback"].update(
        {
            "type": "https",
            "server": "8.8.8.8",
            "server_port": 443,
            "path": "/dns-query",
            "tls": {"enabled": True, "server_name": "dns.google"},
        }
    )
    if selected_outbound_mode == "direct-fixture":
        # sing-box rejects a DNS detour to an empty direct outbound as
        # nonsensical. Omitting detour has the same direct behavior in the
        # disposable fixture; real local mode keeps the selected subscription
        # detour.
        dns_servers["remote-dns"].pop("detour", None)
        dns_servers["remote-dns-fallback"].pop("detour", None)
    config["dns"]["final"] = "remote-dns"
    if policy == "strict":
        route = config["route"]
        route["rules"] = [
            rule for rule in route.get("rules", []) if rule.get("protocol") == "dns" or rule.get("action") == "sniff"
        ]
        route["rule_set"] = []
    return (json.dumps(config, indent=2) + "\n").encode("utf-8")


VIBE_CONFIG = b"""subscription_file: /etc/vibe-vpn-input/sub_url
runtime: singbox
sing_box_bin: /usr/local/bin/sing-box
sing_box_config: /var/lib/vpnkit/sing-box/config.json
sing_box_service: vpnkit-supervised-sing-box
sing_box_restart_mode: request-file
sing_box_restart_file: /run/vpnkit/restart-sing-box
# Daemon/manual apply must wait until the entrypoint consumes this exact
# request token, completes the full runtime health predicate, advances this
# generation marker, and publishes the matching health acknowledgement.
sing_box_restart_ack_generation_file: /run/vpnkit/sing-box-generation
sing_box_restart_ack_file: /run/vpnkit/sing-box-generation.ack
sing_box_restart_ack_timeout: 60s
state_dir: /var/lib/vibe-vpn
production_socks: 127.0.0.1:2080
test_socks: 127.0.0.1:18080
test_url: https://proof.ovh.net/files/10Mb.dat
test_limit_kib: 64
timeout_seconds: 12
"""


def _render(
    template_path: str,
    base: str,
    policy: str,
    ruleset_mode: str,
    selected_outbound_mode: str,
    allow_missing_subscription: str,
) -> None:
    allow_missing = allow_missing_subscription.lower() in {"1", "true", "yes", "on"}
    rule_source_path = os.path.join(os.path.dirname(template_path), "rule-sets")

    with ExitStack() as stack:
        # The root fd and each generated destination directory are held before
        # the optional deterministic race hook. A later same-UID rename or
        # symlink swap therefore either causes an O_NOFOLLOW open to fail or
        # leaves these fds pointing at the already-intended directories.
        base_fd = _open_rooted_dir(base, create=True, owned=True)
        stack.callback(os.close, base_fd)
        source_vibe_fd = _open_child_dir(base_fd, "vibe-vpn", owned=True)
        stack.callback(os.close, source_vibe_fd)
        rendered_fd = _open_child_dir(base_fd, "rendered", owned=True)
        stack.callback(os.close, rendered_fd)
        singbox_fd = _open_child_dir(rendered_fd, "sing-box", owned=True)
        stack.callback(os.close, singbox_fd)
        ruleset_fd = _open_child_dir(singbox_fd, "rule-sets", owned=True)
        stack.callback(os.close, ruleset_fd)
        output_vibe_fd = _open_child_dir(rendered_fd, "vibe-vpn", owned=True)
        stack.callback(os.close, output_vibe_fd)

        # Read every input through an O_NOFOLLOW regular-file descriptor before
        # the race hook. The tracked tree is trusted, but never reopen a source
        # pathname after its initial descriptor check.
        template_config = _build_config(template_path, policy, ruleset_mode, selected_outbound_mode)
        rule_sources = _read_rule_sources(rule_source_path)
        subscription = _read_optional_at(source_vibe_fd, "sub_url")
        if subscription is None:
            if not allow_missing:
                raise RenderError("local subscription file is missing or empty")
        elif not subscription and not allow_missing:
            raise RenderError("local subscription file is missing or empty")

        _run_race_hook(base)

        # Directory permissions are also applied through the held descriptors
        # after the race point; a swapped path entry cannot redirect chmod.
        for directory_fd in (
            base_fd,
            source_vibe_fd,
            rendered_fd,
            singbox_fd,
            ruleset_fd,
            output_vibe_fd,
        ):
            _prepare_directory(directory_fd)

        _atomic_write(singbox_fd, "config.json", template_config)
        for name, content in rule_sources:
            _atomic_write(ruleset_fd, name, content)
        if ruleset_mode == "local-fixture":
            _atomic_write(
                ruleset_fd,
                "geoip-ru.json",
                b'{"version":1,"rules":[{"ip_cidr":["5.0.0.0/8"]}]}\n',
            )
            _atomic_write(
                ruleset_fd,
                "geosite-category-ru.json",
                b'{"version":1,"rules":[{"domain_suffix":["ru"]}]}\n',
            )
        _atomic_write(output_vibe_fd, "config.yaml", VIBE_CONFIG)
        if subscription:
            _atomic_write(output_vibe_fd, "sub_url", subscription)


def main(argv: list[str]) -> int:
    if len(argv) != 6:
        print("renderer writer received invalid arguments", file=sys.stderr)
        return 20
    try:
        _render(*argv)
    except (OSError, RenderError, UnicodeError, ValueError, json.JSONDecodeError):
        # Keep errors secret-free.  The shell adapter's stdout remains the
        # stable, redacted status surface even when a race is rejected.
        print("vpnkit local render failed closed", file=sys.stderr)
        return 20
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
