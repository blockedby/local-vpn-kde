#!/usr/bin/env python3
"""Local KDE VPN backend, JSON transport, for OpenTUI.

This module deliberately stops at an integration boundary.  It does not know
how to operate Docker, NetworkManager, sudo, OpenVPN, or a live VPN.  Lifecycle
buttons either record an action in :class:`MockLifecycleRunner` or invoke one
caller-supplied executable with one of the fixed argv suffixes below:

* ``start``
* ``stop``
* ``retest select``
* ``toggle mode``
* ``diagnostics``

The executable is never invoked through a shell. Interactive runs retain its
output only in bounded private attempt logs; the UI receives allowlisted
reasons and an attempt ID, never raw output. By default the UI uses the adjacent tracked
``vpnkit-local.sh`` adapter and the canonical gitignored local subscription
path; an explicit subscription path must still be that BASE's ``vibe-vpn/sub_url``.
``--status-json`` and ``--test`` are deliberately non-interactive and expose only
redacted state.
"""
from __future__ import annotations

import argparse
import fcntl
import json
import math
import os
import re
import secrets
import selectors
import signal
import stat
import threading
import subprocess
import sys
import time
from dataclasses import dataclass, field
from enum import Enum
from pathlib import Path
from typing import Mapping, Protocol, Sequence


LIFECYCLE_EXECUTABLE_ENV = "VPNKIT_TUI_LIFECYCLE_EXECUTABLE"
TUI_SUPERVISED_ENV = "VPNKIT_TUI_SUPERVISED"
REPO_ROOT = Path(__file__).resolve().parents[2]
DEFAULT_LIFECYCLE_EXECUTABLE = Path(__file__).with_name("vpnkit-local.sh")
DEFAULT_SECRET_ROOT = REPO_ROOT / "secrets" / "vpnkit-local"
DEFAULT_SUBSCRIPTION_PATH = DEFAULT_SECRET_ROOT / "vibe-vpn" / "sub_url"
SUBSCRIPTION_LOCK_NAME = ".vibe-vpn.lock"
LOCAL_SECRETS_ENV = "VPNKIT_LOCAL_SECRETS_DIR"
MAX_SUBSCRIPTION_LENGTH = 16_384
MAX_STATUS_BYTES = 16_384
STATUS_TIMEOUT = 10.0
PROCESS_TERMINATION_GRACE = 0.25
DEFAULT_LIFECYCLE_COMPENSATION_SECONDS = 30.0


class LifecycleAction(str, Enum):
    """The only lifecycle operations this UI may request."""

    BACKEND_START = "backend/start"
    DISCONNECT = "disconnect"
    START = "start"
    STOP = "stop"
    RETEST_SELECT = "retest/select"
    TOGGLE_MODE = "toggle-mode"
    DIAGNOSTICS = "diagnostics"


# Values are fixed, not assembled from user input.  The executable path is the
# sole caller-supplied argv element and is kept separate from these verbs.
LIFECYCLE_ARGV_SUFFIXES: Mapping[LifecycleAction, tuple[str, ...]] = {
    LifecycleAction.BACKEND_START: ("backend", "start"),
    LifecycleAction.DISCONNECT: ("disconnect",),
    LifecycleAction.START: ("start",),
    LifecycleAction.STOP: ("stop",),
    LifecycleAction.RETEST_SELECT: ("retest", "select"),
    LifecycleAction.TOGGLE_MODE: ("toggle", "mode"),
    LifecycleAction.DIAGNOSTICS: ("diagnostics",),
}

BLOCKED_DIRECT_EXECUTABLES = frozenset(
    {
        "docker",
        "docker-compose",
        "podman",
        "podman-compose",
        "nmcli",
        "networkmanager",
        "sudo",
        "openvpn",
        "systemctl",
        "ip",
        "iptables",
        "nft",
    }
)


@dataclass(frozen=True)
class CommandResult:
    """Safe, output-free result returned by a lifecycle runner."""

    action: LifecycleAction
    returncode: int | None
    reason: str = "ok"
    attempt_id: str = ""

    @property
    def ok(self) -> bool:
        return self.returncode == 0 and self.reason == "ok"


class SafeCommandRunner(Protocol):
    """Interface for fixed lifecycle actions.

    Implementations must accept an action, construct an allowlisted argv array,
    and return a result without exposing command output.  The UI never accepts
    arbitrary command arguments.
    """

    def run(self, action: LifecycleAction) -> CommandResult:
        """Run one allowlisted action without a shell."""

    def query_status(self) -> Mapping[str, object]:
        """Return validated, redacted lifecycle status."""



def normalize_action(action: LifecycleAction | str) -> LifecycleAction:
    """Convert an action to the enum while rejecting unknown operations."""

    if isinstance(action, LifecycleAction):
        return action
    try:
        return LifecycleAction(action)
    except ValueError as exc:
        raise ValueError("unsupported lifecycle action") from exc


def _process_group_alive(pgid: int) -> bool:
    """Return whether a supervised process group still has a member."""

    try:
        os.killpg(pgid, 0)
    except ProcessLookupError:
        return False
    except OSError:
        # Permission or transient lookup errors are fail-closed: do not report
        # a timeout while an unverified child could still be mutating state.
        return True
    return True


def _wait_for_process_group_exit(
    process: subprocess.Popen[bytes], pgid: int, timeout: float | None
) -> bool:
    """Wait for both the leader and every supervised group member to exit."""

    deadline = None if timeout is None else time.monotonic() + max(0.0, timeout)
    while True:
        leader_done = process.poll() is not None
        if leader_done and not _process_group_alive(pgid):
            return True
        if deadline is not None:
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                return False
            wait_for = min(0.05, remaining)
        else:
            wait_for = 0.05
        try:
            process.wait(timeout=wait_for)
        except (subprocess.TimeoutExpired, OSError, subprocess.SubprocessError):
            pass


def _terminate_process_group(
    process: subprocess.Popen[bytes], *, grace: float = PROCESS_TERMINATION_GRACE
) -> None:
    """Terminate a lifecycle process and wait for its complete process group."""

    pgid = process.pid
    try:
        os.killpg(pgid, signal.SIGTERM)
    except (OSError, ProcessLookupError):
        pass
    if _wait_for_process_group_exit(process, pgid, grace):
        return

    # Do this even when the process leader exited during the TERM grace period:
    # the lifecycle shell, lock child, and external descendants all share this
    # supervised group when invoked by the TUI.
    try:
        os.killpg(pgid, signal.SIGKILL)
    except (OSError, ProcessLookupError):
        pass
    # SIGKILL cannot be ignored. Keep reaping until the group is gone so
    # run() never returns "timeout" while a mutation child is still alive.
    # This is intentionally unbounded for an OS-level kill race; returning
    # early would violate the lifecycle closure contract.
    _wait_for_process_group_exit(process, pgid, None)


class _RunnerCancelled(Exception):
    """Internal signal used after the supervised lifecycle tree is terminated."""



def _read_bounded_output(
    process: subprocess.Popen[bytes], *, limit: int, timeout: float
) -> tuple[bytes, str | None]:
    """Read at most ``limit`` bytes before returning an error reason.

    ``communicate()`` is deliberately not used here: it buffers all child
    output before a caller can inspect its size.  A nonblocking pipe and
    selector keep the retained status payload bounded while preserving the
    fixed-argv subprocess boundary.
    """

    if process.stdout is None:
        return b"", "unavailable"
    selector = selectors.DefaultSelector()
    fd = process.stdout.fileno()
    os.set_blocking(fd, False)
    selector.register(fd, selectors.EVENT_READ)
    captured = bytearray()
    deadline = time.monotonic() + timeout
    stdout_open = True
    try:
        while True:
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                return bytes(captured), "timeout"
            if stdout_open:
                events = selector.select(remaining)
                if not events:
                    return bytes(captured), "timeout"
                for key, _ in events:
                    del key
                    read_size = min(4096, limit - len(captured) + 1)
                    try:
                        chunk = os.read(fd, read_size)
                    except BlockingIOError:
                        continue
                    if not chunk:
                        stdout_open = False
                        selector.unregister(fd)
                        break
                    if len(captured) + len(chunk) > limit:
                        return bytes(captured), "oversized"
                    captured.extend(chunk)
            else:
                if process.poll() is not None:
                    return bytes(captured), None
                time.sleep(min(0.01, max(0.0, deadline - time.monotonic())))
    finally:
        try:
            selector.unregister(fd)
        except (KeyError, ValueError):
            pass
        selector.close()


_REDACTED_STATUS_VALUES = frozenset(
    {
        "yes",
        "no",
        "configured",
        "missing",
        "active",
        "inactive",
        "unknown",
        "not-configured",
        "not configured",
        "not-managed",
        "not-changed",
        "connected",
        "disconnected",
    }
)
_REDACTED_CONTAINER_VALUES = frozenset(
    {"absent", "stopped", "starting", "healthy", "unhealthy", "running", "inactive", "unknown"}
)
_REDACTED_POLICY_VALUES = frozenset({"strict", "smart"})
_REDACTED_SUBSCRIPTION_VALUES = frozenset({"configured", "missing"})


def _redacted_nm_value(value: object) -> str | None:
    if isinstance(value, bool):
        return "yes" if value else "no"
    if not isinstance(value, str):
        return None
    normalized = value.strip().lower()
    return normalized if normalized in _REDACTED_STATUS_VALUES else None


_NM_BINARY_ALIASES = {
    "yes": "yes",
    "no": "no",
    "configured": "yes",
    "missing": "no",
    "active": "yes",
    "inactive": "no",
    "connected": "yes",
    "disconnected": "no",
    "not-configured": "no",
    "not configured": "no",
    "not-managed": "not-managed",
}


def _redacted_nm_binary(value: object) -> str | None:
    if isinstance(value, bool):
        return "yes" if value else "no"
    if not isinstance(value, str):
        return None
    return _NM_BINARY_ALIASES.get(value.strip().lower())


def _sanitize_lifecycle_status(raw: object) -> dict[str, object]:
    if not isinstance(raw, dict):
        return {}
    result: dict[str, object] = {}
    scalar_keys = {
        "container",
        "routing_policy",
        "subscription",
        "networkmanager_configured",
        "networkmanager_active",
        "configured",
        "active",
    }
    if isinstance(raw.get("schema"), int) and not isinstance(raw["schema"], bool):
        result["schema"] = raw["schema"]
    for key in scalar_keys:
        if key == "container":
            value = raw.get(key)
            if isinstance(value, str) and value in _REDACTED_CONTAINER_VALUES:
                result[key] = value
        elif key == "routing_policy":
            value = raw.get(key)
            if isinstance(value, str) and value in _REDACTED_POLICY_VALUES:
                result[key] = value
        elif key == "subscription":
            value = raw.get(key)
            if isinstance(value, str) and value in _REDACTED_SUBSCRIPTION_VALUES:
                result[key] = value
        else:
            value = _redacted_nm_binary(raw.get(key))
            if value is not None:
                result[key] = value
    networkmanager = raw.get("networkmanager")
    if isinstance(networkmanager, dict):
        safe_networkmanager: dict[str, str] = {}
        for key in ("configured", "active", "state"):
            if key in {"configured", "active"}:
                value = _redacted_nm_binary(networkmanager.get(key))
            else:
                value = _redacted_nm_value(networkmanager.get(key))
            if value is not None:
                safe_networkmanager[key] = value
        if safe_networkmanager:
            result["networkmanager"] = safe_networkmanager
    else:
        value = _redacted_nm_value(networkmanager)
        if value is not None:
            result["networkmanager"] = value
    return result


DIAGNOSTIC_REASONS = {
    "lifecycle recovery preflight failed": "recovery_required",
    "lifecycle committed post-state is unverifiable": "recovery_required",
    "lifecycle recovery required before backend-only start": "recovery_required",
    "failed to resolve source metadata": "docker-registry-failed",
    "failed to solve:": "gateway-build-failed",
    "IPv4 route did not use the exact local VPN device": "route-conflict",
    "IPv4 route lookup returned malformed output": "route-invalid",
    "DNS hostname smoke failed": "dns-failed",
    "DNS hostname smoke returned no addresses": "dns-failed",
    "literal-IP HTTPS smoke failed": "ip-https-failed",
    "hostname HTTPS smoke failed": "hostname-https-failed",
    "IPv4 ping smoke failed": "ping-failed",
    "IPv6 route is unexpectedly available": "ipv6-leak",
    "IPv6 ping unexpectedly succeeded": "ipv6-leak",
    "same-host local VPN smoke failed": "host-smoke-failed",
    "NetworkManager activation failed": "nm-activation-failed",
    "local vpnkit stack failed to start": "gateway-start-failed",
    "local vpnkit failed closed before readiness": "gateway-unhealthy",
    "install and verify the vpnkit local underlay helper": "underlay-not-ready",
}
MAX_ATTEMPT_LOG_BYTES = 8 * 1024 * 1024


class AttemptLog:
    """Private bounded output capture; only allowlisted reasons reach the UI."""

    def __init__(self, action: LifecycleAction) -> None:
        self.action = action
        self.attempt_id = time.strftime("%Y%m%dT%H%M%SZ", time.gmtime()) + "-" + secrets.token_hex(3)
        self.reason = ""
        self.progress = None
        self.started = time.time()
        self.bytes_written = 0
        self.truncated = False
        self.capture_error = False
        self.tail = bytearray()
        self.stop = threading.Event()
        self.thread: threading.Thread | None = None
        self.directory = -1
        self.log = -1
        self.metadata = -1
        try:
            # Reuse the descriptor-based private-root creation/validation.
            base = _canonical_secret_root()
            parent = _open_subscription_parent(base / "vibe-vpn" / "sub_url")
            os.close(parent)
            root = _open_directory_no_symlinks(base)
            try:
                _assert_private_directory(root)
                try:
                    os.mkdir("diagnostics", 0o700, dir_fd=root)
                except FileExistsError:
                    pass
                self.directory = os.open("diagnostics", os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=root)
                _assert_private_directory(self.directory)
            finally:
                os.close(root)
            flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW
            self.log = os.open(self.attempt_id + ".log", flags, 0o600, dir_fd=self.directory)
            self.metadata = os.open(self.attempt_id + ".json", flags, 0o600, dir_fd=self.directory)
            self.write_metadata("running", None, "")
            self.write(f"attempt={self.attempt_id} action={action.value} started_utc={time.strftime('%Y-%m-%dT%H:%M:%SZ', time.gmtime())}\n".encode())
        except BaseException:
            self.close_files()
            raise

    def write(self, data: bytes) -> None:
        # Reserve half for the latest output: readiness and rollback evidence
        # must survive a verbose build that exhausts the initial capture.
        remaining = MAX_ATTEMPT_LOG_BYTES // 2 - self.bytes_written
        kept = data[:max(0, remaining)]
        if kept:
            _write_all(self.log, kept)
            self.bytes_written += len(kept)
        if len(data) > len(kept):
            self.tail.extend(data[len(kept):])
            del self.tail[:-MAX_ATTEMPT_LOG_BYTES // 2]
            self.truncated = True

    def start(self, pipe: object) -> None:
        def pump() -> None:
            selector = selectors.DefaultSelector()
            carry = ""
            stopping_at: float | None = None
            try:
                selector.register(pipe, selectors.EVENT_READ)
                while True:
                    if self.stop.is_set():
                        stopping_at = stopping_at or time.monotonic()
                        if time.monotonic() - stopping_at > 0.25:
                            break
                    ready = selector.select(0.05)
                    if not ready:
                        if self.stop.is_set():
                            break
                        continue
                    data = os.read(pipe.fileno(), 65536)  # type: ignore[attr-defined]
                    if not data:
                        break
                    # Classification continues after the log cap, so a late
                    # smoke failure is still reported after verbose builds.
                    text = carry + data.decode("utf-8", errors="replace")
                    for message, reason in DIAGNOSTIC_REASONS.items():
                        if message in text and not self.reason:
                            self.reason = reason
                    if self.progress:
                        for phase in re.findall(r"vpnkit_phase=([a-z-]+)", text):
                            if phase in {"preparing", "prepared", "render", "compose-up", "compose-up-done", "nm-work", "host-smoke", "nm-disconnect", "compose-down", "runtime-wait", "committing", "committed", "compensating"}:
                                self.progress(phase)
                    carry = text[-256:]
                    try:
                        self.write(data)
                    except OSError:
                        self.capture_error = True
            except (OSError, ValueError):
                self.capture_error = True
            finally:
                selector.close()
                pipe.close()  # type: ignore[attr-defined]
        self.thread = threading.Thread(target=pump, daemon=True)
        self.thread.start()

    def write_metadata(self, state: str, code: int | None, reason: str) -> None:
        payload = json.dumps({
            "schema": 1, "attempt_id": self.attempt_id, "action": self.action.value,
            "started_unix": self.started, "elapsed_seconds": round(time.time() - self.started, 3),
            "state": state, "returncode": code, "reason": reason,
            "truncated": self.truncated, "capture_error": self.capture_error,
        }).encode() + b"\n"
        os.lseek(self.metadata, 0, os.SEEK_SET)
        _write_all(self.metadata, payload)
        os.ftruncate(self.metadata, len(payload))
        os.fsync(self.metadata)

    def finish(self, result: CommandResult) -> CommandResult:
        self.stop.set()
        if self.thread:
            self.thread.join()
        reason = self.reason if result.reason == "failed" and self.reason else result.reason
        try:
            if self.truncated:
                _write_all(self.log, b"\n[bounded capture: latest output follows]\n")
                _write_all(self.log, self.tail)
            os.fsync(self.log)
            self.write_metadata("finished", result.returncode, reason)
        except OSError:
            self.capture_error = True
        finally:
            self.close_files()
        return CommandResult(result.action, result.returncode, reason, self.attempt_id)

    def close_files(self) -> None:
        for name in ("log", "metadata", "directory"):
            fd = getattr(self, name)
            if fd >= 0:
                os.close(fd)
                setattr(self, name, -1)


class AllowlistedSubprocessRunner:
    """Run a configured lifecycle executable with fixed argv arrays only."""

    def __init__(self, executable: str | os.PathLike[str], *, timeout: float = 1800.0, record_attempts: bool = False) -> None:
        value = os.fspath(executable)
        if not isinstance(value, str) or not value or "\x00" in value:
            raise ValueError("lifecycle executable must be a non-empty path")
        if any(character.isspace() for character in value):
            raise ValueError("lifecycle executable must be one argv token")
        if Path(value).name.lower() in BLOCKED_DIRECT_EXECUTABLES:
            raise ValueError("direct system and VPN executables are not lifecycle adapters")
        if timeout <= 0 or not math.isfinite(timeout):
            raise ValueError("lifecycle timeout must be finite and positive")
        self.executable = value
        self.timeout = timeout
        self.record_attempts = record_attempts
        self.progress = None

    @staticmethod
    def _cancellation_grace() -> float:
        """Allow lifecycle compensation to acknowledge TERM before KILL."""

        raw = os.environ.get("VPNKIT_LOCAL_COMPENSATION_TIMEOUT_SECONDS", "")
        try:
            configured = float(raw)
        except (TypeError, ValueError):
            configured = DEFAULT_LIFECYCLE_COMPENSATION_SECONDS
        if not math.isfinite(configured) or configured < 1 or configured > 3600:
            configured = DEFAULT_LIFECYCLE_COMPENSATION_SECONDS
        return max(PROCESS_TERMINATION_GRACE, configured + 1.0)

    def argv_for(self, action: LifecycleAction | str) -> list[str]:
        """Return a fresh, fixed argv list for an allowlisted action."""

        normalized = normalize_action(action)
        try:
            suffix = LIFECYCLE_ARGV_SUFFIXES[normalized]
        except KeyError as exc:  # defensive if the enum and map drift
            raise ValueError("unsupported lifecycle action") from exc
        return [self.executable, *suffix]

    def run(self, action: LifecycleAction | str) -> CommandResult:
        normalized = normalize_action(action)
        if not self.record_attempts:
            return self._run(normalized)
        try:
            capture = AttemptLog(normalized)
            capture.progress = self.progress
        except (OSError, ValueError):
            return CommandResult(normalized, None, "diagnostics-unavailable")
        result = CommandResult(normalized, None, "unavailable")
        try:
            result = self._run(normalized, capture)
        finally:
            result = capture.finish(result)
        return result

    def _run(self, action: LifecycleAction | str, capture: AttemptLog | None = None) -> CommandResult:
        normalized = normalize_action(action)
        argv = self.argv_for(normalized)
        process: subprocess.Popen[bytes] | None = None
        previous_handlers: dict[int, object] = {}

        def cancel(signum: int, _frame: object) -> None:
            signal.signal(signal.SIGUSR1, signal.SIG_IGN)
            if process is not None and process.poll() is None:
                _terminate_process_group(process, grace=self._cancellation_grace())
            raise _RunnerCancelled(signum)

        # The child has its own process group so the TUI can terminate it
        # without killing its UI process. The lifecycle adapter receives a
        # supervision marker and therefore does not create a second detached
        # session for its lock child.
        try:
            for signal_number in (signal.SIGINT, signal.SIGTERM, signal.SIGHUP, signal.SIGUSR1):
                try:
                    previous_handlers[signal_number] = signal.getsignal(signal_number)
                    signal.signal(signal_number, cancel)
                except (OSError, ValueError):
                    pass
            env = os.environ.copy()
            env[TUI_SUPERVISED_ENV] = "1"
            if capture:
                env["VPNKIT_TUI_DIAGNOSTICS"] = "1"
            process = subprocess.Popen(
                argv,
                shell=False,
                stdin=subprocess.DEVNULL,
                stdout=subprocess.PIPE if capture else subprocess.DEVNULL,
                stderr=subprocess.STDOUT if capture else subprocess.DEVNULL,
                start_new_session=True,
                env=env,
            )
            if capture:
                capture.start(process.stdout)
            try:
                returncode = process.wait(timeout=self.timeout)
            except subprocess.TimeoutExpired:
                _terminate_process_group(process, grace=self._cancellation_grace())
                return CommandResult(normalized, None, "timeout")
            except _RunnerCancelled:
                return CommandResult(normalized, None, "cancelled")
        except _RunnerCancelled:
            return CommandResult(normalized, None, "cancelled")
        except (OSError, subprocess.SubprocessError):
            if process is not None and process.poll() is None:
                _terminate_process_group(process, grace=self._cancellation_grace())
            # Do not expose exception text: a path or child output must never
            # become a UI/logging channel for private operator data.
            return CommandResult(normalized, None, "unavailable")
        finally:
            for signal_number, handler in previous_handlers.items():
                try:
                    signal.signal(signal_number, handler)  # type: ignore[arg-type]
                except (OSError, ValueError):
                    pass
        return CommandResult(normalized, returncode, "ok" if returncode == 0 else "failed")

    def query_status(self) -> Mapping[str, object]:
        process: subprocess.Popen[bytes] | None = None
        failure: str | None = None
        try:
            process = subprocess.Popen(
                [self.executable, "status", "--json"],
                shell=False,
                stdin=subprocess.DEVNULL,
                stdout=subprocess.PIPE,
                stderr=subprocess.DEVNULL,
                start_new_session=True,
            )
            raw_bytes, failure = _read_bounded_output(process, limit=MAX_STATUS_BYTES, timeout=STATUS_TIMEOUT)
            if failure is not None:
                _terminate_process_group(process)
                return {}
            if process.returncode is None:
                try:
                    process.wait(timeout=PROCESS_TERMINATION_GRACE)
                except subprocess.TimeoutExpired:
                    _terminate_process_group(process)
                    return {}
            if process.returncode != 0:
                return {}
            raw = json.loads(raw_bytes.decode("utf-8"))
        except (OSError, subprocess.SubprocessError, UnicodeError, json.JSONDecodeError):
            if process is not None and process.poll() is None:
                _terminate_process_group(process)
            return {}
        finally:
            if process is not None and process.stdout is not None:
                process.stdout.close()
        return _sanitize_lifecycle_status(raw)


@dataclass
class MockLifecycleRunner:
    """Deterministic runner for tests and the non-interactive test mode."""

    returncode: int = 0
    actions: list[LifecycleAction] = field(default_factory=list)

    def run(self, action: LifecycleAction | str) -> CommandResult:
        normalized = normalize_action(action)
        self.actions.append(normalized)
        return CommandResult(normalized, self.returncode, "ok" if self.returncode == 0 else "failed")

    def query_status(self) -> Mapping[str, object]:
        return {}


class SubscriptionError(ValueError):
    """Raised when a subscription destination is outside the local secret root."""



def _reject_symlink_components(path: Path) -> None:
    """Reject symlinks in every existing component without resolving them."""

    if not path.is_absolute():
        raise SubscriptionError("local path must be absolute")
    current = Path(path.anchor or os.sep)
    for part in path.parts[1:]:
        if part in ("", "."):
            continue
        if part == "..":
            current = current.parent
            continue
        current /= part
        try:
            entry = os.lstat(current)
        except FileNotFoundError:
            # Keep walking: a later ``..`` component can return to an
            # existing path, matching the shell path guard's lexical check.
            continue
        except OSError as exc:
            raise SubscriptionError("local path could not be inspected") from exc
        if stat.S_ISLNK(entry.st_mode):
            raise SubscriptionError("local path contains a symlink component")



def _path_is_within(path: Path, root: Path, *, include_root: bool = True) -> bool:
    if include_root:
        return path == root or root in path.parents
    return root in path.parents



def _validate_existing_parent(path: Path) -> None:
    """Match the guard's missing-root parent check without creating anything."""

    current = path
    while True:
        try:
            entry = os.lstat(current)
        except FileNotFoundError:
            if current == current.parent:
                raise SubscriptionError("local secret root parent is unavailable")
            current = current.parent
            continue
        except OSError as exc:
            raise SubscriptionError("local secret root parent could not be inspected") from exc
        if not stat.S_ISDIR(entry.st_mode) or stat.S_ISLNK(entry.st_mode):
            raise SubscriptionError("local secret root parent is not a directory")
        return



def _is_private_mode(mode: int) -> bool:
    """Return whether group/other have no access to a secret inode."""

    return stat.S_IMODE(mode) & 0o077 == 0



def _canonical_secret_root() -> Path:
    """Return a canonical root bounded like vpnkit-local-path-guard.sh.

    The environment variable is a selector for the repository's local tree,
    an isolated lab child, or an explicitly marked temporary test fixture. It
    is never permission to inspect or write an arbitrary absolute directory.
    Validation is deliberately performed before status construction so an
    unsafe environment cannot be silently reported as an empty configuration.
    """

    raw = os.environ.get(LOCAL_SECRETS_ENV)
    if raw is None or raw == "":
        requested = DEFAULT_SECRET_ROOT
    else:
        requested = Path(os.path.expanduser(raw))
        if not requested.is_absolute():
            requested = REPO_ROOT / requested

    _reject_symlink_components(requested)
    try:
        # Keep canonicalization lexical. realpath() would turn a symlink that
        # appears between the lexical check and resolution into an accepted
        # destination; the held O_NOFOLLOW descriptors below are the authority.
        base = Path(os.path.abspath(os.path.normpath(os.fspath(requested))))
    except (OSError, ValueError) as exc:
        raise SubscriptionError("local secret root could not be canonicalized") from exc
    _reject_symlink_components(base)

    if any(
        token in part.lower()
        for part in base.parts
        if part not in ("", os.sep)
        for token in ("production", "prod", "live")
    ):
        raise SubscriptionError("refusing production-like local secret root")

    local_root = REPO_ROOT / "secrets" / "vpnkit-local"
    labs_root = REPO_ROOT / "secrets" / "vpnkit-labs"
    temp_root: Path | None = None
    if _path_is_within(base, local_root) or _path_is_within(base, labs_root, include_root=False):
        pass
    else:
        for candidate in (Path("/tmp"), Path("/var/tmp")):
            if candidate in base.parents:
                temp_root = candidate
                break
        if temp_root is None:
            raise SubscriptionError(
                "VPNKIT_LOCAL_SECRETS_DIR must stay in the local, isolated lab, or test temporary tree"
            )
        if base == temp_root:
            raise SubscriptionError("VPNKIT_LOCAL_SECRETS_DIR is too broad")
        relative = base.relative_to(temp_root)
        first = relative.parts[0] if relative.parts else ""
        if base == temp_root / "vpnkit-local-":
            raise SubscriptionError("VPNKIT_LOCAL_SECRETS_DIR is too broad")
        if not first.startswith("vpnkit-local-") and os.environ.get("VPNKIT_LOCAL_TEST_FIXTURE") != "1":
            raise SubscriptionError("unprefixed temporary secret roots require VPNKIT_LOCAL_TEST_FIXTURE=1")

    try:
        entry = os.lstat(base)
    except FileNotFoundError:
        _validate_existing_parent(base)
    except OSError as exc:
        raise SubscriptionError("local secret root could not be inspected") from exc
    else:
        if (
            not stat.S_ISDIR(entry.st_mode)
            or stat.S_ISLNK(entry.st_mode)
            or entry.st_uid != os.geteuid()
            or not _is_private_mode(entry.st_mode)
        ):
            raise SubscriptionError("VPNKIT_LOCAL_SECRETS_DIR must be an owned private directory")
    return base



def _open_directory_no_symlinks(path: Path) -> int:
    """Open a directory by component, refusing symlink traversal."""

    if not path.is_absolute() or any(part == ".." for part in path.parts):
        raise SubscriptionError("subscription directory path is unsafe")
    if not hasattr(os, "O_DIRECTORY") or not hasattr(os, "O_NOFOLLOW"):
        raise SubscriptionError("subscription directory safety features are unavailable")
    directory_flags = os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW
    directory_flags |= getattr(os, "O_CLOEXEC", 0)
    try:
        fd = os.open(os.sep, directory_flags)
    except OSError as exc:
        raise SubscriptionError("subscription directory is unavailable") from exc
    try:
        for part in path.parts[1:]:
            try:
                next_fd = os.open(part, directory_flags, dir_fd=fd)
            except OSError as exc:
                os.close(fd)
                raise SubscriptionError("subscription directory is unavailable") from exc
            os.close(fd)
            fd = next_fd
        return fd
    except SubscriptionError:
        raise



def _assert_private_directory(fd: int) -> os.stat_result:
    try:
        entry = os.fstat(fd)
    except OSError as exc:
        raise SubscriptionError("subscription directory could not be inspected") from exc
    if not stat.S_ISDIR(entry.st_mode) or entry.st_uid != os.geteuid() or not _is_private_mode(entry.st_mode):
        raise SubscriptionError("subscription directory must be an owned private directory")
    return entry



def _assert_private_final(entry: os.stat_result) -> os.stat_result:
    if (
        not stat.S_ISREG(entry.st_mode)
        or entry.st_uid != os.geteuid()
        or entry.st_nlink != 1
        or not _is_private_mode(entry.st_mode)
    ):
        raise SubscriptionError("subscription destination must be an owned private regular file")
    return entry



def _inspect_final_inode(parent_fd: int, name: str) -> os.stat_result | None:
    """Inspect the final inode through the trusted parent descriptor only."""

    if not name or "/" in name or name in (".", ".."):
        raise SubscriptionError("subscription destination name is unsafe")
    if not hasattr(os, "O_NOFOLLOW"):
        raise SubscriptionError("subscription destination safety features are unavailable")
    flags = os.O_RDONLY | os.O_NOFOLLOW
    flags |= getattr(os, "O_CLOEXEC", 0) | getattr(os, "O_NONBLOCK", 0)
    fd = -1
    try:
        try:
            fd = os.open(name, flags, dir_fd=parent_fd)
        except FileNotFoundError:
            return None
        except OSError as exc:
            raise SubscriptionError("subscription destination could not be inspected") from exc
        try:
            return _assert_private_final(os.fstat(fd))
        except OSError as exc:
            raise SubscriptionError("subscription destination could not be inspected") from exc
    finally:
        if fd != -1:
            os.close(fd)



def _selected_path(
    path: str | os.PathLike[str] | None, *, inspect_final: bool = True
) -> Path:
    if path is None:
        raise SubscriptionError("an explicit subscription path is required")
    try:
        raw = os.fspath(path)
    except (TypeError, ValueError) as exc:
        raise SubscriptionError("subscription path is invalid") from exc
    if not isinstance(raw, str) or raw == "":
        raise SubscriptionError("an explicit subscription path is required")
    selected = Path(os.path.expanduser(raw))
    if not selected.is_absolute():
        raise SubscriptionError("subscription path must be absolute")
    if any(part == ".." for part in selected.parts):
        raise SubscriptionError("subscription path must not contain parent traversal")
    root = _canonical_secret_root()
    expected = root / "vibe-vpn" / "sub_url"
    if selected != expected:
        raise SubscriptionError("subscription path must be the canonical local sub_url")
    _reject_symlink_components(selected)
    if inspect_final:
        parent_fd = _open_subscription_parent(selected, root=root)
        try:
            _inspect_final_inode(parent_fd, selected.name)
        finally:
            os.close(parent_fd)
    return selected



def _open_subscription_parent(selected: Path, *, root: Path | None = None) -> int:
    """Return a held, trusted ``BASE/vibe-vpn`` directory descriptor."""

    trusted_root = root if root is not None else _canonical_secret_root()
    expected = trusted_root / "vibe-vpn" / "sub_url"
    if selected != expected:
        raise SubscriptionError("subscription path must be the canonical local sub_url")
    base_fd = -1
    try:
        base_fd = _open_directory_no_symlinks(trusted_root)
        _assert_private_directory(base_fd)
        if not hasattr(os, "O_DIRECTORY") or not hasattr(os, "O_NOFOLLOW"):
            raise SubscriptionError("subscription directory safety features are unavailable")
        flags = os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW
        flags |= getattr(os, "O_CLOEXEC", 0)
        try:
            parent_fd = os.open("vibe-vpn", flags, dir_fd=base_fd)
        except OSError as exc:
            raise SubscriptionError("subscription directory is unavailable") from exc
        try:
            _assert_private_directory(parent_fd)
        except BaseException:
            os.close(parent_fd)
            raise
        return parent_fd
    finally:
        if base_fd != -1:
            os.close(base_fd)



def _assert_private_lock(fd: int) -> os.stat_result:
    """Validate the persistent lock inode through its already-open fd."""

    try:
        entry = os.fstat(fd)
    except OSError:
        raise SubscriptionError("subscription lock could not be inspected") from None
    if (
        not stat.S_ISREG(entry.st_mode)
        or entry.st_uid != os.geteuid()
        or entry.st_nlink != 1
        or not _is_private_mode(entry.st_mode)
    ):
        raise SubscriptionError("subscription lock must be an owned private regular file")
    return entry



def _acquire_subscription_lock(parent_fd: int) -> int:
    """Open and exclusively lock the persistent sibling of ``sub_url``.

    The parent descriptor was opened and validated by ``_open_subscription_parent``.
    The lock pathname is therefore never resolved through a process-global path,
    and the inode is retained rather than unlinked after the transaction.
    """

    if not hasattr(os, "O_NOFOLLOW") or not hasattr(os, "O_CLOEXEC"):
        raise SubscriptionError("subscription lock safety features are unavailable")
    flags = os.O_RDWR | os.O_CREAT | os.O_NOFOLLOW | os.O_CLOEXEC
    flags |= getattr(os, "O_NONBLOCK", 0)
    lock_fd = -1
    try:
        try:
            lock_fd = os.open(SUBSCRIPTION_LOCK_NAME, flags, 0o600, dir_fd=parent_fd)
        except OSError:
            raise SubscriptionError("subscription lock could not be opened") from None
        _assert_private_lock(lock_fd)
        try:
            fcntl.flock(lock_fd, fcntl.LOCK_EX)
        except OSError:
            raise SubscriptionError("subscription lock could not be acquired") from None
        # Recheck both the descriptor and the descriptor-relative pathname
        # after acquisition. A same-user actor that swaps the persistent
        # pathname while this fd waits must not create a second lock domain.
        opened_entry = _assert_private_lock(lock_fd)
        try:
            path_entry = os.stat(
                SUBSCRIPTION_LOCK_NAME,
                dir_fd=parent_fd,
                follow_symlinks=False,
            )
        except OSError:
            raise SubscriptionError("subscription lock path could not be inspected") from None
        if (
            not stat.S_ISREG(path_entry.st_mode)
            or path_entry.st_uid != os.geteuid()
            or path_entry.st_nlink != 1
            or not _is_private_mode(path_entry.st_mode)
            or (path_entry.st_dev, path_entry.st_ino)
            != (opened_entry.st_dev, opened_entry.st_ino)
        ):
            raise SubscriptionError("subscription lock path changed")
        return lock_fd
    except BaseException:
        if lock_fd != -1:
            try:
                fcntl.flock(lock_fd, fcntl.LOCK_UN)
            except OSError:
                pass
            try:
                os.close(lock_fd)
            except OSError:
                pass
        raise



def _write_all(fd: int, data: bytes) -> None:
    view = memoryview(data)
    while view:
        written = os.write(fd, view)
        if written <= 0:
            raise SubscriptionError("subscription staging write made no progress")
        view = view[written:]



def _copy_fd(source_fd: int, destination_fd: int) -> None:
    """Copy a private snapshot without materializing secret bytes in logs."""

    while True:
        chunk = os.read(source_fd, 64 * 1024)
        if not chunk:
            return
        _write_all(destination_fd, chunk)



def _assert_private_stage(fd: int, *, expected_mode: int = 0o600) -> os.stat_result:
    try:
        entry = os.fstat(fd)
    except OSError as exc:
        raise SubscriptionError("subscription staging file could not be inspected") from exc
    if (
        not stat.S_ISREG(entry.st_mode)
        or entry.st_uid != os.geteuid()
        or entry.st_nlink != 1
        or stat.S_IMODE(entry.st_mode) != stat.S_IMODE(expected_mode)
        or not _is_private_mode(expected_mode)
    ):
        raise SubscriptionError("subscription staging file is not private")
    return entry



def _create_subscription_stage(parent_fd: int, *, label: str = "sub_url") -> tuple[str, int]:
    if not hasattr(os, "O_NOFOLLOW"):
        raise SubscriptionError("subscription staging safety features are unavailable")
    flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW
    flags |= getattr(os, "O_CLOEXEC", 0)
    for _ in range(8):
        stage = f".{label}.{os.getpid()}.{secrets.token_hex(16)}.tmp"
        try:
            fd = os.open(stage, flags, 0o600, dir_fd=parent_fd)
        except FileExistsError:
            continue
        except OSError as exc:
            raise SubscriptionError("subscription staging file could not be created") from exc
        return stage, fd
    raise SubscriptionError("subscription staging file could not be created")



def _open_final_inode(parent_fd: int, name: str) -> tuple[int, os.stat_result | None]:
    """Open the final inode for a descriptor-relative, immutable snapshot."""

    if not name or "/" in name or name in (".", ".."):
        raise SubscriptionError("subscription destination name is unsafe")
    if not hasattr(os, "O_NOFOLLOW"):
        raise SubscriptionError("subscription destination safety features are unavailable")
    flags = os.O_RDONLY | os.O_NOFOLLOW
    flags |= getattr(os, "O_CLOEXEC", 0) | getattr(os, "O_NONBLOCK", 0)
    try:
        fd = os.open(name, flags, dir_fd=parent_fd)
    except FileNotFoundError:
        return -1, None
    except OSError as exc:
        raise SubscriptionError("subscription destination could not be inspected") from exc
    try:
        return fd, _assert_private_final(os.fstat(fd))
    except BaseException:
        try:
            os.close(fd)
        except OSError:
            pass
        raise



def _inspect_inode_identity(entry: os.stat_result | None) -> tuple[int, int] | None:
    if entry is None:
        return None
    return entry.st_dev, entry.st_ino



def _unlink_stage_if_owned(parent_fd: int, name: str, stage_fd: int) -> None:
    """Remove a stage only while its pathname still names our inode."""

    probe_fd = -1
    try:
        flags = os.O_RDONLY | os.O_NOFOLLOW | getattr(os, "O_CLOEXEC", 0)
        try:
            probe_fd = os.open(name, flags, dir_fd=parent_fd)
        except FileNotFoundError:
            return
        except OSError:
            return
        if _inspect_inode_identity(os.fstat(probe_fd)) != _inspect_inode_identity(os.fstat(stage_fd)):
            return
        os.unlink(name, dir_fd=parent_fd)
    except (OSError, ValueError):
        # Cleanup is deliberately silent; neither a private stage name nor an
        # operating-system error belongs in UI output.
        pass
    finally:
        if probe_fd != -1:
            try:
                os.close(probe_fd)
            except OSError:
                pass



def _rollback_subscription(
    *,
    parent_fd: int,
    name: str,
    original_identity: tuple[int, int] | None,
    candidate_identity: tuple[int, int] | None,
    rollback_name: str | None,
    rollback_fd: int,
    rollback_is_absent: bool,
    rollback_mode: int | None,
) -> bool:
    """Compensate a post-rename failure without overwriting a raced inode."""

    rollback_ok = True
    try:
        current = _inspect_final_inode(parent_fd, name)
    except BaseException:
        current = None
        rollback_ok = False

    current_identity = _inspect_inode_identity(current)
    already_restored = (
        current_identity == original_identity
        if not rollback_is_absent
        else current is None
    )
    if not already_restored:
        # Never restore over an inode that is not the one installed by this
        # transaction. This is the race barrier for a newer writer.
        if current_identity != candidate_identity or rollback_name is None:
            rollback_ok = False
        else:
            try:
                if rollback_is_absent:
                    os.unlink(name, dir_fd=parent_fd)
                else:
                    os.replace(rollback_name, name, src_dir_fd=parent_fd, dst_dir_fd=parent_fd)
            except BaseException:
                rollback_ok = False

    # Verify the best state we can establish before the directory sync. A
    # failed sync is still reported as incomplete even when bytes/mode are
    # already back in place.
    try:
        restored = _inspect_final_inode(parent_fd, name)
        if rollback_is_absent:
            state_matches = restored is None
        else:
            rollback_entry = _assert_private_stage(rollback_fd, expected_mode=rollback_mode)  # type: ignore[arg-type]
            state_matches = (
                restored is not None
                and _inspect_inode_identity(restored) == _inspect_inode_identity(rollback_entry)
                and stat.S_IMODE(restored.st_mode) == stat.S_IMODE(rollback_mode)  # type: ignore[arg-type]
            )
        # An injected/underlying rename can report an error after moving the
        # stage. The final descriptor-relative state, not that ambiguous
        # return status, decides whether compensation completed.
        rollback_ok = state_matches
    except BaseException:
        rollback_ok = False

    try:
        os.fsync(parent_fd)
    except BaseException:
        rollback_ok = False
    return rollback_ok



def _verify_replaced_inode(stage_fd: int, parent_fd: int, name: str) -> None:
    staged = _assert_private_stage(stage_fd)
    final = _inspect_final_inode(parent_fd, name)
    if final is None or _inspect_inode_identity(final) != _inspect_inode_identity(staged):
        raise SubscriptionError("subscription destination changed during replacement")



def write_subscription(path: str | os.PathLike[str] | None, value: str) -> None:
    """Atomically replace the canonical local subscription file.

    Existing destination inodes are never opened for writing or truncated. A
    new private inode is written and fsynced, then installed with a relative
    rename while the trusted ``BASE/vibe-vpn`` descriptor remains held. Any
    failure after that rename is compensated from a private rollback stage.
    A persistent sibling lock serializes the complete descriptor-relative
    transaction across processes and remains until stage cleanup finishes.
    """

    # Path/root validation may happen before the lock, but no final inode is
    # trusted until the persistent sibling lock has been acquired.
    selected = _selected_path(path, inspect_final=False)
    if not isinstance(value, str) or not value or not value.strip():
        raise SubscriptionError("subscription value must not be empty")
    if len(value) > MAX_SUBSCRIPTION_LENGTH:
        raise SubscriptionError("subscription value is too long")
    if "\x00" in value or "\n" in value or "\r" in value:
        raise SubscriptionError("subscription value must be one line")
    try:
        encoded = value.encode("utf-8")
    except UnicodeEncodeError:
        raise SubscriptionError("subscription value is not valid UTF-8") from None
    if len(encoded) > MAX_SUBSCRIPTION_LENGTH:
        raise SubscriptionError("subscription value is too long")

    parent_fd = -1
    lock_fd = -1
    original_fd = -1
    stage_fd = -1
    rollback_fd = -1
    stage_name: str | None = None
    rollback_name: str | None = None
    rollback_is_absent = False
    original_identity: tuple[int, int] | None = None
    candidate_identity: tuple[int, int] | None = None
    rollback_mode: int | None = None

    try:
        parent_fd = _open_subscription_parent(selected)
        lock_fd = _acquire_subscription_lock(parent_fd)

        # This is the write transaction's final-file preflight. It is
        # deliberately after lock acquisition so a second process cannot
        # snapshot, stage, replace, verify, or roll back concurrently.
        original_fd, original_entry = _open_final_inode(parent_fd, selected.name)
        if original_entry is not None:
            original_identity = _inspect_inode_identity(original_entry)
            rollback_mode = stat.S_IMODE(original_entry.st_mode)
        else:
            rollback_is_absent = True

        # This is a private, O_EXCL, descriptor-relative copy of the exact
        # pre-call state. It is deliberately not a hard link: rollback must
        # remain independent of the inode being replaced.
        rollback_name, rollback_fd = _create_subscription_stage(
            parent_fd, label="sub_url-rollback"
        )
        if original_fd != -1:
            _copy_fd(original_fd, rollback_fd)
            current_original = os.fstat(original_fd)
            if (
                _inspect_inode_identity(current_original) != original_identity
                or stat.S_IMODE(current_original.st_mode) != rollback_mode
                or current_original.st_size != original_entry.st_size  # type: ignore[union-attr]
            ):
                raise SubscriptionError("subscription destination changed during staging")
            os.fchmod(rollback_fd, rollback_mode)  # type: ignore[arg-type]
            _assert_private_stage(rollback_fd, expected_mode=rollback_mode)  # type: ignore[arg-type]
        else:
            # An empty private stage plus this in-memory flag records that the
            # original final entry was absent. It is never renamed to target.
            _assert_private_stage(rollback_fd)
        os.fsync(rollback_fd)
        if rollback_is_absent:
            _assert_private_stage(rollback_fd)
        else:
            _assert_private_stage(rollback_fd, expected_mode=rollback_mode)  # type: ignore[arg-type]

        stage_name, stage_fd = _create_subscription_stage(parent_fd)
        os.fchmod(stage_fd, 0o600)
        _assert_private_stage(stage_fd)
        _write_all(stage_fd, encoded)
        os.fchmod(stage_fd, 0o600)
        _assert_private_stage(stage_fd)
        os.fsync(stage_fd)
        candidate_entry = _assert_private_stage(stage_fd)
        candidate_identity = _inspect_inode_identity(candidate_entry)

        # This is intentionally the last observation before renameat: the
        # original final inode must still be the one captured before staging.
        # A changed inode is rejected without touching either final state.
        before_replace = _inspect_final_inode(parent_fd, selected.name)
        if _inspect_inode_identity(before_replace) != original_identity:
            raise SubscriptionError("subscription destination changed during staging")

        try:
            os.replace(stage_name, selected.name, src_dir_fd=parent_fd, dst_dir_fd=parent_fd)
        except BaseException:
            # An adapter/test double can perform the rename and then raise. Run
            # the same identity-checked compensation path; if no rename took
            # place it observes the unchanged original and performs no write.
            rollback_ok = _rollback_subscription(
                parent_fd=parent_fd,
                name=selected.name,
                original_identity=original_identity,
                candidate_identity=candidate_identity,
                rollback_name=rollback_name,
                rollback_fd=rollback_fd,
                rollback_is_absent=rollback_is_absent,
                rollback_mode=rollback_mode,
            )
            raise SubscriptionError(
                "subscription update failed; prior state restored"
                if rollback_ok
                else "subscription update failed; rollback incomplete"
            ) from None

        try:
            # The directory sync and final-inode verification are both inside
            # the transaction: any failure after os.replace must restore the
            # exact pre-call bytes/mode before returning.
            os.fsync(parent_fd)
            _verify_replaced_inode(stage_fd, parent_fd, selected.name)
            # Keep the final directory sync inside the same compensation
            # window. This second sync is intentionally not allowed to turn a
            # post-rename durability failure into a committed in-memory state.
            os.fsync(parent_fd)
        except BaseException:
            rollback_ok = _rollback_subscription(
                parent_fd=parent_fd,
                name=selected.name,
                original_identity=original_identity,
                candidate_identity=candidate_identity,
                rollback_name=rollback_name,
                rollback_fd=rollback_fd,
                rollback_is_absent=rollback_is_absent,
                rollback_mode=rollback_mode,
            )
            raise SubscriptionError(
                "subscription update failed; prior state restored"
                if rollback_ok
                else "subscription update failed; rollback incomplete"
            ) from None
    except SubscriptionError:
        raise
    except BaseException:
        # Keep all filesystem and encoding failures free of paths, bytes, and
        # exception text that could contain a subscription URL.
        raise SubscriptionError("subscription destination could not be written") from None
    finally:
        if original_fd != -1:
            try:
                os.close(original_fd)
            except OSError:
                pass
        if parent_fd != -1:
            if stage_name is not None and stage_fd != -1:
                _unlink_stage_if_owned(parent_fd, stage_name, stage_fd)
            if rollback_name is not None and rollback_fd != -1:
                _unlink_stage_if_owned(parent_fd, rollback_name, rollback_fd)
        if stage_fd != -1:
            try:
                os.close(stage_fd)
            except OSError:
                pass
        if rollback_fd != -1:
            try:
                os.close(rollback_fd)
            except OSError:
                pass
        if parent_fd != -1:
            try:
                os.close(parent_fd)
            except OSError:
                pass
        # Unlock last: all rollback, stage removal, and descriptor cleanup
        # above still happens while the process owns the exclusive lock.
        if lock_fd != -1:
            try:
                fcntl.flock(lock_fd, fcntl.LOCK_UN)
            except OSError:
                pass
            try:
                os.close(lock_fd)
            except OSError:
                pass



def subscription_configured(path: str | os.PathLike[str] | None) -> bool:
    """Check only canonical-file metadata; never follow links or read content."""

    if path is None:
        return False
    try:
        selected = _selected_path(path)
        parent_fd = _open_subscription_parent(selected)
        try:
            entry = _inspect_final_inode(parent_fd, selected.name)
        finally:
            os.close(parent_fd)
    except (OSError, SubscriptionError, TypeError, ValueError):
        return False
    return entry is not None and entry.st_size > 0


@dataclass
class TuiState:
    """In-memory state intentionally limited to redacted, display-safe values."""

    subscription_path: str | os.PathLike[str] | None = None
    runner: SafeCommandRunner = field(default_factory=MockLifecycleRunner)
    mode: str = "strict"
    vpn_state: str = "unknown"
    gateway_state: str = "unknown"
    diagnostics: str = "not-run"
    selection: str = "redacted"
    last_action: str = "none"
    last_result: str = "not-run"
    last_attempt: str = ""
    networkmanager_configured: str = "unknown"
    networkmanager_active: str = "unknown"

    def __post_init__(self) -> None:
        # Fail closed before even the initial metadata/status probe. The helper
        # still returns False for malformed explicit paths, but an arbitrary
        # environment root must not become a silent "missing" status.
        _canonical_secret_root()
        if self.subscription_path is not None:
            _validate_subscription_argument(self.subscription_path)
        self.subscription_ready = subscription_configured(self.subscription_path)

    @property
    def runner_kind(self) -> str:
        return "mock" if isinstance(self.runner, MockLifecycleRunner) else "configured"

    def refresh(self) -> None:
        raw = self.runner.query_status()
        # A failed/partial probe must not preserve a previously active VPN.
        self.vpn_state = "unknown"
        self.networkmanager_configured = "unknown"
        self.networkmanager_active = "unknown"
        container = raw.get("container")
        policy = raw.get("routing_policy")
        subscription = raw.get("subscription")
        container_is_valid = container in {
            "absent",
            "stopped",
            "starting",
            "healthy",
            "unhealthy",
            "running",
            "inactive",
            "unknown",
        }
        self.gateway_state = str(container) if container_is_valid else "unknown"
        if policy in {"strict", "smart"}:
            self.mode = str(policy)
        if subscription in {"configured", "missing"}:
            self.subscription_ready = subscription == "configured"

        # The structured NetworkManager object has precedence over legacy
        # top-level fields.  This prevents an untrusted/stale fallback from
        # turning an explicitly managed active=no result into active=yes.
        networkmanager = raw.get("networkmanager")
        configured: object = None
        active: object = None
        if isinstance(networkmanager, Mapping):
            configured = networkmanager.get("configured")
            active = networkmanager.get("active")
        if configured is None:
            configured = raw.get("networkmanager_configured", raw.get("configured"))
        if active is None:
            active = raw.get("networkmanager_active", raw.get("active"))
        safe_configured = _redacted_nm_binary(configured)
        safe_active = _redacted_nm_binary(active)
        if safe_configured is not None:
            self.networkmanager_configured = safe_configured
        if safe_active is not None:
            self.networkmanager_active = safe_active

        # A healthy container is only container evidence.  When the owned NM
        # profile is configured but inactive, reporting healthy/running would
        # falsely claim a host VPN path.  Explicit not-managed mode retains
        # container-only behavior.
        if self.networkmanager_active == "no" and self.networkmanager_configured in {"yes", "no"}:
            self.vpn_state = "inactive"
        elif container_is_valid:
            self.vpn_state = str(container)

    def save_subscription(self, value: str) -> None:
        write_subscription(self.subscription_path, value)
        self.subscription_ready = True
        self.last_action = "configure subscription"
        self.last_result = "ok"

    def dispatch(self, action: LifecycleAction | str) -> CommandResult:
        normalized = normalize_action(action)
        result = self.runner.run(normalized)
        self.last_action = normalized.value
        self.last_result = "ok" if result.ok else result.reason
        self.last_attempt = result.attempt_id
        if not result.ok:
            return result

        if normalized is LifecycleAction.START:
            self.vpn_state = "inactive" if (
                self.networkmanager_configured == "yes" and self.networkmanager_active == "no"
            ) else "running"
        elif normalized is LifecycleAction.STOP:
            self.vpn_state = "stopped"
        elif normalized is LifecycleAction.RETEST_SELECT:
            self.selection = "redacted"
        elif normalized is LifecycleAction.TOGGLE_MODE:
            self.mode = "strict" if self.mode == "smart" else "smart"
        elif normalized is LifecycleAction.DIAGNOSTICS:
            self.diagnostics = "available"
        return result

    def status(self) -> dict[str, str | int]:
        """Return stable JSON-safe status with no paths, endpoints, or values."""

        return {
            "schema": 1,
            "vpn_state": self.vpn_state,
            "gateway_state": self.gateway_state,
            "subscription": "configured" if self.subscription_ready else "not configured",
            "endpoint": "redacted",
            "routing_mode": self.mode,
            "selection": self.selection,
            "diagnostics": self.diagnostics,
            "last_action": self.last_action,
            "last_result": self.last_result,
            "last_attempt": self.last_attempt,
            "networkmanager_configured": self.networkmanager_configured,
            "networkmanager_active": self.networkmanager_active,
            "runner": self.runner_kind,
        }


def _validate_subscription_argument(path: str | os.PathLike[str]) -> None:
    """Reject an explicit CLI path before status mode does any filesystem work."""

    _selected_path(path)



def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--subscription-path",
        default=None,
        help="canonical local subscription file (default: BASE/vibe-vpn/sub_url)",
    )
    parser.add_argument(
        "--lifecycle-executable",
        help=f"one executable token for the fixed lifecycle adapter (or ${LIFECYCLE_EXECUTABLE_ENV})",
    )
    parser.add_argument(
        "--status-json",
        action="store_true",
        help="print redacted status JSON without opening the interface",
    )
    parser.add_argument(
        "--test",
        action="store_true",
        help="non-interactive mock status (no lifecycle operations)",
    )
    parser.add_argument("--mode", choices=("smart", "strict"), default="strict")
    parser.add_argument("--bridge", action="store_true", help=argparse.SUPPRESS)
    return parser


def read_subscription_for_editor(path):
    # Explicit editor response only; status and diagnostic metadata stay redacted.
    selected = _selected_path(path)
    parent = _open_subscription_parent(selected)
    fd = -1
    try:
        try:
            fd = os.open(selected.name, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK, dir_fd=parent)
        except FileNotFoundError:
            return ""
        _assert_private_final(os.fstat(fd))
        raw = os.read(fd, MAX_SUBSCRIPTION_LENGTH + 1)
        if len(raw) > MAX_SUBSCRIPTION_LENGTH:
            raise ValueError("invalid subscription")
        return raw.decode("utf-8").strip()
    finally:
        if fd >= 0: os.close(fd)
        os.close(parent)


def run_server_request(state, action, value):
    command = action.removeprefix("servers/")
    if command not in {"list", "refresh", "ping", "speed", "availability", "check-batch", "select", "current"}:
        raise ValueError("invalid server command")
    suffix = ["servers", command]
    if command == "check-batch":
        fields = json.loads(value) if isinstance(value, str) else {}
        if not isinstance(fields, dict):
            raise ValueError("invalid batch")
        ids = fields.get("ids", [])
        target = fields.get("url")
        if (not isinstance(ids, list) or not 1 <= len(ids) <= 5
                or any(not isinstance(i, str) or re.fullmatch(r"srv_[A-Za-z0-9_-]{27}", i) is None for i in ids)
                or len(set(ids)) != len(ids)):
            raise ValueError("invalid batch")
        if (not isinstance(target, str) or not target.startswith("https://")
                or len(target) > 2048 or any(ord(c) < 32 for c in target)):
            raise ValueError("invalid target")
        suffix.extend([",".join(ids), target])
    if command in {"ping", "speed", "availability", "select"}:
        fields = json.loads(value) if command == "availability" and isinstance(value, str) else {"id": value}
        server_id = fields.get("id")
        if not isinstance(server_id, str) or re.fullmatch(r"srv_[A-Za-z0-9_-]{27}", server_id) is None:
            raise ValueError("invalid server id")
        suffix.append(server_id)
        if command == "availability":
            target = fields.get("url")
            if not isinstance(target, str) or not target.startswith("https://") or len(target)>2048 or any(ord(c)<32 for c in target):
                raise ValueError("invalid target")
            suffix.append(target)
    if isinstance(state.runner, MockLifecycleRunner):
        return {"schema":"vibe-vpn.server-browser.v2", "status":"ok", "generation":1, "servers":[]}
    process = None
    previous = {}
    def cancel(signum, frame):
        signal.signal(signal.SIGUSR1, signal.SIG_IGN)
        if process is not None:
            _terminate_process_group(process, grace=state.runner._cancellation_grace())
        raise _RunnerCancelled(signum)
    try:
        for sig in (signal.SIGINT,signal.SIGTERM,signal.SIGHUP,signal.SIGUSR1):
            previous[sig] = signal.getsignal(sig)
            signal.signal(sig,cancel)
        process = subprocess.Popen([state.runner.executable, *suffix], stdin=subprocess.DEVNULL,
            stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, start_new_session=True,
            env={**os.environ, TUI_SUPERVISED_ENV:"1"})
        raw, failure = _read_bounded_output(process, limit=1048576, timeout=state.runner.timeout)
        if failure:
            _terminate_process_group(process, grace=state.runner._cancellation_grace())
            return {"status": "timeout" if failure == "timeout" else "unavailable"}
        data = json.loads(raw)
        if not isinstance(data, dict): raise ValueError("invalid response")
        if data.get("schema") != "vibe-vpn.server-browser.v2": return {"status":"backend-outdated"}
        # Fixed fields only; arbitrary stdout never reaches the renderer.
        def server(row):
            if not isinstance(row, dict) or not re.fullmatch(r"srv_[A-Za-z0-9_-]{27}",str(row.get("server_id",""))):
                raise ValueError("invalid server")
            safe = {"server_id":row["server_id"], "display_name":str(row.get("display_name","Server"))[:160],
                    "selected":row.get("selected") is True}
            safe["display_name"] = re.sub(r"[\x00-\x1f\x7f-\x9f]", "", safe["display_name"])
            for field in ("status", "ping_status", "availability"):
                safe[field] = "ready" if field == "status" and row.get(field) == "selected" else row.get(field) if row.get(field) in {"ready","failed","untested"} else "untested"
            for field in ("latency_ms","download_mbps","download_seconds","downloaded_bytes"):
                n=row.get(field)
                if isinstance(n,(int,float)) and not isinstance(n,bool) and math.isfinite(n) and n>=0: safe[field]=n
            return safe
        result = {"status":data.get("status") if data.get("status") in {"ok","failed","stale","canceled","unavailable","recovery_required","backend-outdated"} else "unavailable"}
        if command == "check-batch" and result["status"] == "ok" and "servers" not in data:
            raise ValueError("missing batch response")
        if "servers" in data:
            if not isinstance(data["servers"],list) or len(data["servers"])>1000: raise ValueError("invalid catalog")
            result["servers"]=[server(row) for row in data["servers"]]
            if command == "check-batch":
                returned = [row["server_id"] for row in result["servers"]]
                if len(returned) > 5 or len(set(returned)) != len(returned) or set(returned) != set(ids):
                    raise ValueError("invalid batch response")
        if "server" in data: result["server"]=server(data["server"])
        return result
    except _RunnerCancelled:
        return {"status":"canceled"}
    finally:
        if process is not None:
            if process.poll() is None: _terminate_process_group(process, grace=state.runner._cancellation_grace())
            if process.stdout: process.stdout.close()
        for sig,handler in previous.items(): signal.signal(sig,handler)


def run_bridge(state: TuiState) -> int:
    """Serialized JSON-line transport for OpenTUI; secrets only enter on stdin.

    Keep lifecycle supervision and private-file validation in this process.
    Never echo requests, child output, exceptions, or subscription values.
    """
    signal.signal(signal.SIGUSR1, signal.SIG_IGN)
    while True:
        line = sys.stdin.buffer.readline(MAX_SUBSCRIPTION_LENGTH * 6 + 1024)
        if not line:
            return 0
        if not line.endswith(b"\n"):
            return 2
        ok, reason, code = True, "ok", None
        extra = {}
        try:
            request = json.loads(line)
            if not isinstance(request, dict):
                raise ValueError("invalid request")
            action = request.get("action")
            if isinstance(state.runner, AllowlistedSubprocessRunner):
                state.runner.progress = (lambda phase: print(json.dumps({"event":"progress", "phase":phase}), flush=True)) if request.get("progress") is True else None
            if action == "subscription/read":
                extra["value"] = read_subscription_for_editor(state.subscription_path)
            elif isinstance(action, str) and action.startswith("servers/"):
                data = run_server_request(state, action, request.get("value"))
                extra["catalog"] = data
                ok = data["status"] == "ok"
                reason = "cancelled" if data["status"] == "canceled" else data["status"]
            elif action == "status":
                state.refresh()
            elif action == "subscription":
                value = request.get("value")
                if not isinstance(value, str):
                    raise ValueError("invalid subscription")
                state.save_subscription(value)
            else:
                normalized = normalize_action(action)
                # The lifecycle adapter can initialize its Docker backend
                # without connecting the host VPN before testing nodes.
                result = state.dispatch(normalized)
                ok, reason, code = result.ok, result.reason, result.returncode
                state.refresh()
        except (ValueError, TypeError, OSError, UnicodeError):
            ok, reason = False, "invalid-request"
        print(json.dumps({"ok": ok, "reason": reason, "code": code, "status": state.status(), **extra}), flush=True)



def main(argv: Sequence[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    executable = args.lifecycle_executable or os.environ.get(LIFECYCLE_EXECUTABLE_ENV) or str(DEFAULT_LIFECYCLE_EXECUTABLE)
    try:
        if args.test:
            runner: SafeCommandRunner = MockLifecycleRunner()
        else:
            runner = AllowlistedSubprocessRunner(executable, record_attempts=not args.status_json)
        if args.subscription_path is not None:
            _validate_subscription_argument(args.subscription_path)
        subscription_path = args.subscription_path or (None if args.test else str(_canonical_secret_root() / "vibe-vpn" / "sub_url"))
        state = TuiState(subscription_path, runner=runner, mode=args.mode)
    except ValueError as exc:
        parser.error(str(exc))

    if args.bridge:
        return run_bridge(state)

    if args.status_json or args.test:
        # json.dumps is the only output path in non-interactive mode.  The
        # state object contains no subscription value or selected path.
        if not args.test:
            state.refresh()
        print(json.dumps(state.status(), sort_keys=True))
        return 0

    parser.error("use ./run.sh for the interactive interface")



if __name__ == "__main__":
    sys.exit(main())
