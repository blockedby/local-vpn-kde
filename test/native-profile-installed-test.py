#!/usr/bin/env python3
"""Exercise the installed unprivileged native profile transaction endpoint."""

import os
import pwd
import select
import stat
import struct
import subprocess
import sys
import time
from pathlib import Path


LAB_OPT_IN = "VPNKIT_NATIVE_PACKAGE_LAB"
FIXTURE_DIRECTORY = "/work/native-profile-fixtures"
STATE_PATH = "/home/builder/.local/state/local-vpn-kde"
PROFILE_NAME = "vpnkit-local.ovpn"
INSTALLED_CLI = "/usr/libexec/local-vpn-kde/local-vpn-kde.bin"
SETPRIV = "/usr/bin/setpriv"
MAX_PROFILE_SIZE = 1 << 20
READY = b"READY\n"
COMMITTED = b"COMMITTED\n"
ROLLED_BACK = b"ROLLED_BACK\n"
READY_TIMEOUT = 20.0
ACTION_TIMEOUT = 20.0
KILL_TIMEOUT = 3.0


class LabError(RuntimeError):
    pass


def require_disposable_lab() -> None:
    if os.environ.get(LAB_OPT_IN) != "1":
        raise LabError("lab opt-in is required")
    if not os.path.exists("/.dockerenv"):
        raise LabError("Docker container marker is absent")
    if os.geteuid() != 0:
        raise LabError("lab driver requires root")


def read_private_fixture(name: str) -> bytes:
    directory_fd = os.open(
        FIXTURE_DIRECTORY,
        os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW | os.O_CLOEXEC,
    )
    try:
        directory_info = os.fstat(directory_fd)
        if (
            not stat.S_ISDIR(directory_info.st_mode)
            or directory_info.st_uid != 0
            or directory_info.st_mode & 0o777 != 0o700
        ):
            raise LabError("fixture directory is not private")
        file_fd = os.open(
            name,
            os.O_RDONLY | os.O_NOFOLLOW | os.O_CLOEXEC,
            dir_fd=directory_fd,
        )
        try:
            file_info = os.fstat(file_fd)
            if (
                not stat.S_ISREG(file_info.st_mode)
                or file_info.st_uid != 0
                or file_info.st_nlink != 1
                or file_info.st_mode & 0o777 != 0o600
                or file_info.st_size <= 0
                or file_info.st_size > MAX_PROFILE_SIZE
            ):
                raise LabError("fixture profile is not private")
            with os.fdopen(file_fd, "rb") as stream:
                file_fd = -1
                data = stream.read(MAX_PROFILE_SIZE + 1)
            if len(data) == 0 or len(data) > MAX_PROFILE_SIZE:
                raise LabError("fixture profile size is invalid")
            return data
        finally:
            if file_fd >= 0:
                os.close(file_fd)
    finally:
        os.close(directory_fd)


def read_installed_profile() -> bytes:
    try:
        with open(
            os.path.join(STATE_PATH, "openvpn", "client", PROFILE_NAME),
            "rb",
        ) as stream:
            data = stream.read(MAX_PROFILE_SIZE + 1)
    except OSError as error:
        raise LabError("installed profile could not be read") from error
    if not data or len(data) > MAX_PROFILE_SIZE:
        raise LabError("installed profile size is invalid")
    return data


def lookup_builder() -> tuple[int, int]:
    try:
        account = pwd.getpwnam("builder")
    except KeyError as error:
        raise LabError("desktop builder account is unavailable") from error
    if account.pw_uid <= 0 or account.pw_gid < 0:
        raise LabError("desktop builder identity is invalid")
    return account.pw_uid, account.pw_gid


def request_bytes(profile: bytes) -> bytes:
    state = STATE_PATH.encode("utf-8")
    if not state or len(state) > 4096 or len(profile) == 0 or len(profile) > MAX_PROFILE_SIZE:
        raise LabError("transaction request is invalid")
    return struct.pack(">I", len(state)) + state + struct.pack(">I", len(profile)) + profile


def write_pipe(pipe, data: bytes, deadline: float) -> None:
    fd = pipe.fileno()
    os.set_blocking(fd, False)
    offset = 0
    while offset < len(data):
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            raise LabError("transaction input timed out")
        _, writable, _ = select.select([], [fd], [], remaining)
        if not writable:
            raise LabError("transaction input timed out")
        try:
            written = os.write(fd, data[offset:])
        except BlockingIOError:
            continue
        except OSError as error:
            raise LabError("transaction input failed") from error
        if written <= 0:
            raise LabError("transaction input failed")
        offset += written


def read_line_bounded(stream, deadline: float) -> bytes:
    fd = stream.fileno()
    line = bytearray()
    while len(line) < 64:
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            raise LabError("transaction acknowledgment timed out")
        readable, _, _ = select.select([fd], [], [], remaining)
        if not readable:
            raise LabError("transaction acknowledgment timed out")
        try:
            byte = os.read(fd, 1)
        except OSError as error:
            raise LabError("transaction acknowledgment failed") from error
        if not byte:
            raise LabError("transaction acknowledgment ended early")
        line.extend(byte)
        if byte == b"\n":
            return bytes(line)
    raise LabError("transaction acknowledgment is too long")


def read_until_eof_bounded(stream, deadline: float) -> None:
    fd = stream.fileno()
    while True:
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            raise LabError("transaction output did not close")
        readable, _, _ = select.select([fd], [], [], remaining)
        if not readable:
            raise LabError("transaction output did not close")
        try:
            chunk = os.read(fd, 4096)
        except OSError as error:
            raise LabError("transaction output could not be read") from error
        if not chunk:
            return
        raise LabError("transaction emitted an unexpected token")


def child_status_is_restricted(pid: int, uid: int, gid: int) -> bool:
    try:
        status = Path(f"/proc/{pid}/status").read_text(encoding="ascii")
    except (OSError, UnicodeError):
        return False
    fields = {}
    for line in status.splitlines():
        if ":" not in line:
            continue
        name, value = line.split(":", 1)
        fields[name] = value.split()
    if fields.get("Uid") != [str(uid)] * 4 or fields.get("Gid") != [str(gid)] * 4:
        return False
    for name in ("CapInh", "CapPrm", "CapEff", "CapBnd", "CapAmb"):
        values = fields.get(name)
        if values is None or len(values) != 1:
            return False
        try:
            if int(values[0], 16) != 0:
                return False
        except ValueError:
            return False
    return fields.get("NoNewPrivs") == ["1"]


def stop_child(process: subprocess.Popen) -> None:
    try:
        if process.stdin is not None:
            process.stdin.close()
    except OSError:
        pass
    if process.poll() is None:
        # Never address a process group: this is the child owned by this driver.
        process.kill()
    try:
        process.wait(timeout=KILL_TIMEOUT)
    except subprocess.TimeoutExpired as error:
        raise LabError("transaction child could not be stopped") from error


def start_transaction(uid: int, gid: int, profile: bytes) -> subprocess.Popen:
    command = [
        SETPRIV,
        "--no-new-privs",
        "--bounding-set=-all",
        "--inh-caps=-all",
        "--ambient-caps=-all",
        "--reuid",
        str(uid),
        "--regid",
        str(gid),
        "--clear-groups",
        "--",
        INSTALLED_CLI,
        "native-profile-transaction",
    ]
    try:
        process = subprocess.Popen(
            command,
            cwd="/",
            env={"PATH": "/usr/bin:/bin", "HOME": "/", "LANG": "C.UTF-8"},
            stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,
            stderr=subprocess.DEVNULL,
            close_fds=True,
        )
    except OSError as error:
        raise LabError("transaction child could not start") from error
    try:
        if process.stdin is None or process.stdout is None:
            raise LabError("transaction pipes are unavailable")
        write_pipe(process.stdin, request_bytes(profile), time.monotonic() + READY_TIMEOUT)
        if read_line_bounded(process.stdout, time.monotonic() + READY_TIMEOUT) != READY:
            raise LabError("transaction child did not become ready")
        if process.poll() is not None or not child_status_is_restricted(process.pid, uid, gid):
            raise LabError("transaction child identity is not restricted")
        return process
    except BaseException:
        stop_child(process)
        raise


def finish_transaction(
    uid: int,
    gid: int,
    profile: bytes,
    action: bytes,
    expected: bytes,
) -> None:
    process = start_transaction(uid, gid, profile)
    try:
        if process.stdin is None or process.stdout is None:
            raise LabError("transaction pipes are unavailable")
        write_pipe(process.stdin, action, time.monotonic() + ACTION_TIMEOUT)
        process.stdin.close()
        if read_line_bounded(process.stdout, time.monotonic() + ACTION_TIMEOUT) != expected:
            raise LabError("transaction child returned an unexpected result")
        try:
            exit_code = process.wait(timeout=ACTION_TIMEOUT)
        except subprocess.TimeoutExpired as error:
            process.kill()
            raise LabError("transaction child did not exit") from error
        if exit_code != 0:
            raise LabError("transaction child failed")
    finally:
        if process.poll() is None:
            stop_child(process)
        if process.stdout is not None:
            process.stdout.close()


def exercise_eof(uid: int, gid: int, profile: bytes) -> None:
    process = start_transaction(uid, gid, profile)
    try:
        if process.stdin is None or process.stdout is None:
            raise LabError("transaction pipes are unavailable")
        process.stdin.close()
        try:
            exit_code = process.wait(timeout=ACTION_TIMEOUT)
        except subprocess.TimeoutExpired as error:
            process.kill()
            raise LabError("transaction child did not exit after EOF") from error
        if exit_code == 0:
            raise LabError("transaction child accepted EOF")
        read_until_eof_bounded(process.stdout, time.monotonic() + ACTION_TIMEOUT)
    finally:
        if process.poll() is None:
            stop_child(process)
        if process.stdout is not None:
            process.stdout.close()


def assert_installed_profile(expected: bytes) -> None:
    if read_installed_profile() != expected:
        raise LabError("installed profile bytes did not match")


def run() -> None:
    require_disposable_lab()
    uid, gid = lookup_builder()
    first = read_private_fixture("first.ovpn")
    second = read_private_fixture("second.ovpn")

    finish_transaction(uid, gid, first, b"C", COMMITTED)
    assert_installed_profile(first)

    finish_transaction(uid, gid, second, b"R", ROLLED_BACK)
    assert_installed_profile(first)

    exercise_eof(uid, gid, second)
    assert_installed_profile(first)


if __name__ == "__main__":
    try:
        run()
    except Exception:
        print("native_profile_installed_transaction_lab=error", file=sys.stderr)
        sys.exit(1)
    print("native_profile_installed_transaction_lab=pass")
