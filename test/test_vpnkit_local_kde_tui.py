#!/usr/bin/env python3
"""Focused stdlib regressions for the issue #40 local KDE TUI."""
from __future__ import annotations

import contextlib
import importlib.util
import io
import json
import os
import stat
import subprocess
import sys
import tempfile
import time
import unittest
from pathlib import Path
from unittest import mock


ROOT = Path(__file__).resolve().parents[1]
MODULE_PATH = ROOT / "test" / "fixtures" / "legacy-tui.py"
SPEC = importlib.util.spec_from_file_location("vpnkit_local_kde_tui_issue40", MODULE_PATH)
assert SPEC is not None and SPEC.loader is not None
TUI = importlib.util.module_from_spec(SPEC)
sys.modules["vpnkit_local_kde_tui_issue40"] = TUI
SPEC.loader.exec_module(TUI)


@contextlib.contextmanager
def local_secret_tree(temp_dir: str):
    root = Path(temp_dir) / "secrets" / "vpnkit-local"
    subscription_dir = root / "vibe-vpn"
    subscription_dir.mkdir(parents=True, exist_ok=True)
    root.chmod(0o700)
    subscription_dir.chmod(0o700)
    with mock.patch.dict(
        os.environ,
        {TUI.LOCAL_SECRETS_ENV: str(root), "VPNKIT_LOCAL_TEST_FIXTURE": "1"},
        clear=False,
    ):
        yield root, subscription_dir / "sub_url"


def wait_for_file(path: Path, timeout: float = 10.0) -> None:
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if path.exists():
            return
        time.sleep(0.01)
    raise AssertionError(f"timed out waiting for test event: {path.name}")


MULTIPROCESS_WRITER = r'''
import importlib.util
import os
import sys
import time
from pathlib import Path

module_path = os.environ["TUI_MODULE_PATH"]
spec = importlib.util.spec_from_file_location("vpnkit_local_kde_tui_child", module_path)
assert spec is not None and spec.loader is not None
module = importlib.util.module_from_spec(spec)
sys.modules["vpnkit_local_kde_tui_child"] = module
spec.loader.exec_module(module)

role = os.environ["WRITER_ROLE"]
if role == "a":
    real_verify = module._verify_replaced_inode

    def pause_then_fail(stage_fd, parent_fd, name):
        real_verify(stage_fd, parent_fd, name)
        Path(os.environ["CANDIDATE_CHECKED"]).touch()
        deadline = time.monotonic() + 15.0
        while not Path(os.environ["RELEASE_A"]).exists():
            if time.monotonic() >= deadline:
                raise RuntimeError("test release timeout")
            time.sleep(0.01)
        raise module.SubscriptionError("forced post-rename failure")

    module._verify_replaced_inode = pause_then_fail
elif role == "b":
    Path(os.environ["WRITER_B_STARTED"]).touch()
else:
    raise RuntimeError("unknown writer role")

try:
    module.write_subscription(os.environ["SUBSCRIPTION_PATH"], os.environ["WRITER_VALUE"])
except module.SubscriptionError:
    Path(os.environ["WRITER_RESULT"]).write_text("error", encoding="ascii")
    sys.exit(0)
except BaseException:
    Path(os.environ["WRITER_RESULT"]).write_text("unexpected", encoding="ascii")
    sys.exit(2)
else:
    Path(os.environ["WRITER_RESULT"]).write_text("ok", encoding="ascii")
'''


class DockerFailureDiagnosticTests(unittest.TestCase):
    def test_build_errors_keep_specific_reason_and_private_log(self):
        cases = [
            ("lifecycle recovery preflight failed; journal retained and mutation refused", "recovery_required"),
            ("lifecycle recovery required before backend-only start", "recovery_required"),
            ("failed to solve: DeadlineExceeded: failed to resolve source metadata: private.example/token", "docker-registry-failed"),
            ("failed to solve: build command failed", "gateway-build-failed"),
        ]
        for message, reason in cases:
            with self.subTest(reason=reason), tempfile.TemporaryDirectory() as tmp, local_secret_tree(tmp) as (root, _):
                capture = TUI.AttemptLog(TUI.LifecycleAction.START)
                read_fd, write_fd = os.pipe()
                capture.start(os.fdopen(read_fd, "rb"))
                os.write(write_fd, (message + "\nlocal vpnkit stack failed to start\n").encode())
                os.close(write_fd)
                result = capture.finish(TUI.CommandResult(TUI.LifecycleAction.START, 1, "failed"))
                self.assertEqual(result.reason, reason)
                self.assertNotIn("private.example", repr(result))
                log = root / "diagnostics" / (result.attempt_id + ".log")
                self.assertIn(message, log.read_text())
                self.assertEqual(stat.S_IMODE(log.stat().st_mode), 0o600)


class SubscriptionTests(unittest.TestCase):
    def test_writes_only_canonical_file_with_0600(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir, local_secret_tree(temp_dir) as (_, path):
            path.write_text("old", encoding="utf-8")
            path.chmod(0o600)

            TUI.write_subscription(path, "hidden-subscription-value")

            self.assertEqual(path.read_text(encoding="utf-8"), "hidden-subscription-value")
            self.assertEqual(stat.S_IMODE(path.stat().st_mode), 0o600)
            self.assertEqual(sorted(p.name for p in path.parent.iterdir()), [".vibe-vpn.lock", "sub_url"])

    def test_requires_canonical_absolute_destination_and_rejects_multiline_value(self) -> None:
        with self.assertRaises(TUI.SubscriptionError):
            TUI.write_subscription(None, "value")
        with self.assertRaises(TUI.SubscriptionError):
            TUI.write_subscription("relative-sub-url", "value")

        with tempfile.TemporaryDirectory() as temp_dir, local_secret_tree(temp_dir) as (root, path):
            with self.assertRaises(TUI.SubscriptionError):
                TUI.write_subscription(root / "other", "value")
            outside = Path(temp_dir) / "outside" / "vibe-vpn" / "sub_url"
            with self.assertRaises(TUI.SubscriptionError):
                TUI.write_subscription(outside, "value")
            with self.assertRaises(TUI.SubscriptionError):
                TUI.TuiState(outside)
            with self.assertRaises(TUI.SubscriptionError):
                TUI.write_subscription(path, "line-one\nline-two")

    def test_rejects_final_parent_and_root_symlinks_without_touching_target(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir:
            root = Path(temp_dir) / "secrets" / "vpnkit-local"
            subscription_dir = root / "vibe-vpn"
            subscription_dir.mkdir(parents=True)
            target = Path(temp_dir) / "outside"
            target.write_text("unchanged", encoding="utf-8")

            with local_secret_tree(temp_dir):
                link = subscription_dir / "sub_url"
                link.symlink_to(target)
                with self.assertRaises(TUI.SubscriptionError):
                    TUI.write_subscription(link, "hidden-value")
                self.assertFalse(TUI.subscription_configured(link))
                link.unlink()

                subscription_dir.rmdir()
                subscription_dir.symlink_to(target.parent, target_is_directory=True)
                with self.assertRaises(TUI.SubscriptionError):
                    TUI.write_subscription(root / "vibe-vpn" / "sub_url", "hidden-value")
                self.assertFalse(TUI.subscription_configured(root / "vibe-vpn" / "sub_url"))

            target_dir = Path(temp_dir) / "root-target" / "vibe-vpn"
            target_dir.mkdir(parents=True)
            symlink_root = Path(temp_dir) / "secrets-link"
            symlink_root.symlink_to(root, target_is_directory=True)
            with mock.patch.dict(os.environ, {TUI.LOCAL_SECRETS_ENV: str(symlink_root)}, clear=False):
                selected = symlink_root / "vibe-vpn" / "sub_url"
                with self.assertRaises(TUI.SubscriptionError):
                    TUI.write_subscription(selected, "hidden-value")
                self.assertFalse(TUI.subscription_configured(selected))
            self.assertEqual(target.read_text(encoding="utf-8"), "unchanged")

    def test_rejects_hardlink_before_truncation(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir, local_secret_tree(temp_dir) as (_, path):
            outside = Path(temp_dir) / "outside"
            outside.write_text("unchanged", encoding="utf-8")
            outside.chmod(0o600)
            path.hardlink_to(outside)
            with self.assertRaises(TUI.SubscriptionError):
                TUI.write_subscription(path, "replacement")
            self.assertEqual(outside.read_text(encoding="utf-8"), "unchanged")

    def test_lock_is_persistent_private_and_single_link(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir, local_secret_tree(temp_dir) as (_, path):
            TUI.write_subscription(path, "https://old.example.invalid/persistent-lock")
            lock = path.parent / TUI.SUBSCRIPTION_LOCK_NAME
            before = lock.stat()
            self.assertTrue(stat.S_ISREG(before.st_mode))
            self.assertEqual(before.st_uid, os.geteuid())
            self.assertEqual(before.st_nlink, 1)
            self.assertEqual(stat.S_IMODE(before.st_mode), 0o600)

            TUI.write_subscription(path, "https://new.example.invalid/persistent-lock")
            after = lock.stat()
            self.assertEqual((after.st_dev, after.st_ino), (before.st_dev, before.st_ino))
            self.assertEqual(after.st_nlink, 1)
            self.assertEqual(stat.S_IMODE(after.st_mode), 0o600)
            self.assertFalse(lock.is_symlink())

    def test_rejects_hostile_persistent_lock_inodes(self) -> None:
        # A symlink must not be followed or removed.
        with tempfile.TemporaryDirectory() as temp_dir, local_secret_tree(temp_dir) as (_, path):
            path.write_text("https://old.example.invalid/lock-symlink", encoding="utf-8")
            path.chmod(0o600)
            lock = path.parent / TUI.SUBSCRIPTION_LOCK_NAME
            target = Path(temp_dir) / "lock-target"
            target.write_text("lock-target-sentinel", encoding="utf-8")
            target.chmod(0o600)
            lock.symlink_to(target)
            with self.assertRaises(TUI.SubscriptionError):
                TUI.write_subscription(path, "https://new.example.invalid/lock-symlink")
            self.assertTrue(lock.is_symlink())
            self.assertEqual(target.read_text(encoding="utf-8"), "lock-target-sentinel")
            self.assertEqual(path.read_text(encoding="utf-8"), "https://old.example.invalid/lock-symlink")

        # A hardlinked lock would make the lock pathname an alias for another
        # inode, so it must be rejected without changing the sentinel.
        with tempfile.TemporaryDirectory() as temp_dir, local_secret_tree(temp_dir) as (_, path):
            path.write_text("https://old.example.invalid/lock-hardlink", encoding="utf-8")
            path.chmod(0o600)
            lock = path.parent / TUI.SUBSCRIPTION_LOCK_NAME
            target = Path(temp_dir) / "lock-hardlink-target"
            target.write_text("hardlink-sentinel", encoding="utf-8")
            target.chmod(0o600)
            lock.hardlink_to(target)
            link_count = target.stat().st_nlink
            with self.assertRaises(TUI.SubscriptionError):
                TUI.write_subscription(path, "https://new.example.invalid/lock-hardlink")
            self.assertEqual(target.read_text(encoding="utf-8"), "hardlink-sentinel")
            self.assertEqual(lock.stat().st_nlink, link_count)
            self.assertEqual(path.read_text(encoding="utf-8"), "https://old.example.invalid/lock-hardlink")

        # Group/other access is not a private lock, even when the inode is
        # otherwise regular and owned by this process.
        with tempfile.TemporaryDirectory() as temp_dir, local_secret_tree(temp_dir) as (_, path):
            path.write_text("https://old.example.invalid/lock-mode", encoding="utf-8")
            path.chmod(0o600)
            lock = path.parent / TUI.SUBSCRIPTION_LOCK_NAME
            lock.write_text("lock-mode-sentinel", encoding="utf-8")
            lock.chmod(0o640)
            with self.assertRaises(TUI.SubscriptionError):
                TUI.write_subscription(path, "https://new.example.invalid/lock-mode")
            self.assertEqual(stat.S_IMODE(lock.stat().st_mode), 0o640)
            self.assertEqual(lock.read_text(encoding="utf-8"), "lock-mode-sentinel")
            self.assertEqual(path.read_text(encoding="utf-8"), "https://old.example.invalid/lock-mode")

        # A foreign owner is rejected from the descriptor metadata, without
        # requiring chown privileges in the test account.
        with tempfile.TemporaryDirectory() as temp_dir, local_secret_tree(temp_dir) as (_, path):
            path.write_text("https://old.example.invalid/lock-owner", encoding="utf-8")
            path.chmod(0o600)
            lock = path.parent / TUI.SUBSCRIPTION_LOCK_NAME
            lock.write_text("lock-owner-sentinel", encoding="utf-8")
            lock.chmod(0o600)
            lock_inode = lock.stat().st_ino
            real_fstat = os.fstat

            def foreign_lock_owner(fd: int):
                entry = real_fstat(fd)
                if entry.st_ino != lock_inode:
                    return entry
                values = list(entry)
                values[4] = os.geteuid() + 1
                return os.stat_result(values)

            with mock.patch.object(TUI.os, "fstat", side_effect=foreign_lock_owner):
                with self.assertRaises(TUI.SubscriptionError):
                    TUI.write_subscription(path, "https://new.example.invalid/lock-owner")
            self.assertEqual(lock.read_text(encoding="utf-8"), "lock-owner-sentinel")
            self.assertEqual(path.read_text(encoding="utf-8"), "https://old.example.invalid/lock-owner")

    def test_process_lock_serializes_post_rename_rollback_for_existing_and_absent(self) -> None:
        for initially_present in (True, False):
            with self.subTest(initially_present=initially_present):
                with tempfile.TemporaryDirectory() as temp_dir, local_secret_tree(temp_dir) as (_, path):
                    old_value = "https://old.example.invalid/process-lock"
                    candidate_value = "https://candidate.example.invalid/process-lock"
                    committed_value = "https://committed.example.invalid/process-lock"
                    if initially_present:
                        path.write_text(old_value, encoding="utf-8")
                        path.chmod(0o600)

                    events = Path(temp_dir) / "events"
                    events.mkdir()
                    candidate_checked = events / "candidate-checked"
                    writer_b_started = events / "writer-b-started"
                    release_a = events / "release-a"
                    writer_a_result = events / "writer-a-result"
                    writer_b_result = events / "writer-b-result"
                    common_env = os.environ.copy()
                    common_env.update(
                        {
                            "TUI_MODULE_PATH": str(MODULE_PATH),
                            "SUBSCRIPTION_PATH": str(path),
                            "CANDIDATE_CHECKED": str(candidate_checked),
                            "WRITER_B_STARTED": str(writer_b_started),
                            "RELEASE_A": str(release_a),
                        }
                    )
                    env_a = common_env.copy()
                    env_a.update(
                        {
                            "WRITER_ROLE": "a",
                            "WRITER_VALUE": candidate_value,
                            "WRITER_RESULT": str(writer_a_result),
                        }
                    )
                    writer_a = subprocess.Popen(
                        [sys.executable, "-c", MULTIPROCESS_WRITER],
                        env=env_a,
                        stdout=subprocess.PIPE,
                        stderr=subprocess.PIPE,
                        text=True,
                    )
                    writer_b: subprocess.Popen[str] | None = None
                    try:
                        wait_for_file(candidate_checked)
                        env_b = common_env.copy()
                        env_b.update(
                            {
                                "WRITER_ROLE": "b",
                                "WRITER_VALUE": committed_value,
                                "WRITER_RESULT": str(writer_b_result),
                            }
                        )
                        writer_b = subprocess.Popen(
                            [sys.executable, "-c", MULTIPROCESS_WRITER],
                            env=env_b,
                            stdout=subprocess.PIPE,
                            stderr=subprocess.PIPE,
                            text=True,
                        )
                        wait_for_file(writer_b_started)

                        # A has already verified the candidate identity and is
                        # still holding the lock. B must remain blocked with A's
                        # candidate visible until A restores its prior state.
                        time.sleep(0.2)
                        self.assertIsNone(writer_b.poll())
                        self.assertEqual(path.read_text(encoding="utf-8"), candidate_value)
                        self.assertFalse(writer_b_result.exists())

                        release_a.touch()
                        a_stdout, a_stderr = writer_a.communicate(timeout=10)
                        b_stdout, b_stderr = writer_b.communicate(timeout=10)
                        self.assertEqual(writer_a.returncode, 0, a_stderr)
                        self.assertEqual(writer_b.returncode, 0, b_stderr)
                        self.assertEqual(writer_a_result.read_text(encoding="ascii"), "error")
                        self.assertEqual(writer_b_result.read_text(encoding="ascii"), "ok")
                        self.assertEqual(path.read_text(encoding="utf-8"), committed_value)
                        self.assertNotIn(candidate_value, a_stdout + a_stderr + b_stdout + b_stderr)
                        self.assertNotIn(committed_value, a_stdout + a_stderr + b_stdout + b_stderr)
                        lock = path.parent / TUI.SUBSCRIPTION_LOCK_NAME
                        lock_entry = lock.stat()
                        self.assertEqual(stat.S_IMODE(lock_entry.st_mode), 0o600)
                        self.assertEqual(lock_entry.st_nlink, 1)
                    finally:
                        release_a.touch()
                        for process in (writer_a, writer_b):
                            if process is not None and process.poll() is None:
                                process.kill()
                            if process is not None:
                                process.wait(timeout=10)

    def test_rejects_arbitrary_env_root_before_write_or_status_side_effect(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir:
            root = Path(temp_dir) / "arbitrary-secret-root"
            path = root / "vibe-vpn" / "sub_url"
            with mock.patch.dict(
                os.environ,
                {
                    TUI.LOCAL_SECRETS_ENV: str(root),
                    "VPNKIT_LOCAL_TEST_FIXTURE": "0",
                },
                clear=False,
            ):
                with self.assertRaises(TUI.SubscriptionError):
                    TUI.write_subscription(path, "must-not-be-written")
                self.assertFalse(root.exists())

                class SideEffectRunner:
                    def __init__(self) -> None:
                        self.queries = 0

                    def query_status(self):
                        self.queries += 1
                        return {"container": "healthy"}

                    def run(self, action):
                        raise AssertionError("lifecycle action must not run")

                runner = SideEffectRunner()
                with self.assertRaises(TUI.SubscriptionError):
                    TUI.TuiState(None, runner=runner)
                self.assertEqual(runner.queries, 0)

    def test_status_requires_private_file_mode(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir, local_secret_tree(temp_dir) as (_, path):
            path.write_text("configured", encoding="utf-8")
            path.chmod(0o644)
            self.assertFalse(TUI.subscription_configured(path))
            with self.assertRaises(TUI.SubscriptionError):
                TUI.write_subscription(path, "configured-again")
            self.assertEqual(path.read_text(encoding="utf-8"), "configured")

            path.chmod(0o600)
            TUI.write_subscription(path, "configured-again")
            self.assertTrue(TUI.subscription_configured(path))

    def test_rejects_permissive_base_and_subscription_directories(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir, local_secret_tree(temp_dir) as (root, path):
            for directory in (root, path.parent):
                directory.chmod(0o755)
                with self.assertRaises(TUI.SubscriptionError):
                    TUI.write_subscription(path, "replacement")
                self.assertFalse(path.exists())
                directory.chmod(0o700)

    def test_rejects_foreign_owned_existing_inode_before_staging(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir, local_secret_tree(temp_dir) as (_, path):
            path.write_text("old-bytes", encoding="utf-8")
            path.chmod(0o600)
            real_fstat = os.fstat
            replaced = False

            def foreign_fstat(fd: int):
                nonlocal replaced
                entry = real_fstat(fd)
                if not replaced and stat.S_ISREG(entry.st_mode):
                    values = list(entry)
                    values[4] = os.geteuid() + 1
                    replaced = True
                    return os.stat_result(values)
                return entry

            with mock.patch.object(TUI.os, "fstat", side_effect=foreign_fstat):
                with self.assertRaises(TUI.SubscriptionError):
                    TUI.write_subscription(path, "replacement")
            self.assertEqual(path.read_text(encoding="utf-8"), "old-bytes")
            self.assertEqual(sorted(p.name for p in path.parent.iterdir()), [".vibe-vpn.lock", "sub_url"])

    def test_atomic_replace_does_not_follow_raced_final_symlink(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir, local_secret_tree(temp_dir) as (_, path):
            outside = Path(temp_dir) / "outside"
            outside.write_text("outside-bytes", encoding="utf-8")
            path.write_text("old-bytes", encoding="utf-8")
            path.chmod(0o600)
            real_replace = os.replace

            def replace_after_symlink_race(
                source: str,
                destination: str,
                *,
                src_dir_fd: int | None = None,
                dst_dir_fd: int | None = None,
            ) -> None:
                path.unlink()
                path.symlink_to(outside)
                real_replace(
                    source,
                    destination,
                    src_dir_fd=src_dir_fd,
                    dst_dir_fd=dst_dir_fd,
                )

            with mock.patch.object(TUI.os, "replace", side_effect=replace_after_symlink_race):
                TUI.write_subscription(path, "replacement")
            self.assertEqual(outside.read_text(encoding="utf-8"), "outside-bytes")
            self.assertEqual(path.read_text(encoding="utf-8"), "replacement")
            self.assertFalse(path.is_symlink())

    def test_write_failure_preserves_old_bytes_and_cleans_stage(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir, local_secret_tree(temp_dir) as (_, path):
            path.write_text("old-bytes", encoding="utf-8")
            path.chmod(0o600)
            old_inode = path.stat().st_ino
            with mock.patch.object(TUI.os, "write", side_effect=OSError("write failure")):
                with self.assertRaises(TUI.SubscriptionError):
                    TUI.write_subscription(path, "replacement")
            self.assertEqual(path.read_text(encoding="utf-8"), "old-bytes")
            self.assertEqual(path.stat().st_ino, old_inode)
            self.assertEqual(sorted(p.name for p in path.parent.iterdir()), [".vibe-vpn.lock", "sub_url"])

    def test_fsync_failure_preserves_old_bytes_and_cleans_stage(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir, local_secret_tree(temp_dir) as (_, path):
            path.write_text("old-bytes", encoding="utf-8")
            path.chmod(0o600)
            old_inode = path.stat().st_ino
            with mock.patch.object(TUI.os, "fsync", side_effect=OSError("fsync failure")):
                with self.assertRaises(TUI.SubscriptionError):
                    TUI.write_subscription(path, "replacement")
            self.assertEqual(path.read_text(encoding="utf-8"), "old-bytes")
            self.assertEqual(path.stat().st_ino, old_inode)
            self.assertEqual(sorted(p.name for p in path.parent.iterdir()), [".vibe-vpn.lock", "sub_url"])

    def test_post_replace_parent_fsync_restores_exact_old_bytes_and_mode(self) -> None:
        old_value = "https://old.example.invalid/subscription-token"
        new_value = "https://new.example.invalid/replacement-token"
        with tempfile.TemporaryDirectory() as temp_dir, local_secret_tree(temp_dir) as (_, path):
            path.write_text(old_value, encoding="utf-8")
            path.chmod(0o400)
            old_mode = stat.S_IMODE(path.stat().st_mode)
            directory_fsyncs = 0
            real_fsync = TUI.os.fsync

            def fail_first_parent_fsync(fd: int) -> None:
                nonlocal directory_fsyncs
                if stat.S_ISDIR(os.fstat(fd).st_mode):
                    directory_fsyncs += 1
                    if directory_fsyncs == 1:
                        raise OSError("post-replace parent fsync")
                real_fsync(fd)

            with mock.patch.object(TUI.os, "fsync", side_effect=fail_first_parent_fsync):
                with self.assertRaises(TUI.SubscriptionError) as raised:
                    TUI.write_subscription(path, new_value)

            self.assertIn("prior state restored", str(raised.exception))
            self.assertNotIn(old_value, str(raised.exception))
            self.assertNotIn(new_value, str(raised.exception))
            self.assertEqual(path.read_text(encoding="utf-8"), old_value)
            self.assertEqual(stat.S_IMODE(path.stat().st_mode), old_mode)
            self.assertEqual(sorted(p.name for p in path.parent.iterdir()), [".vibe-vpn.lock", "sub_url"])

    def test_second_post_replace_parent_fsync_also_restores_old_state(self) -> None:
        old_value = "https://old.example.invalid/second-fsync-token"
        new_value = "https://new.example.invalid/second-fsync-replacement"
        with tempfile.TemporaryDirectory() as temp_dir, local_secret_tree(temp_dir) as (_, path):
            path.write_text(old_value, encoding="utf-8")
            path.chmod(0o600)
            directory_fsyncs = 0
            real_fsync = TUI.os.fsync

            def fail_second_parent_fsync(fd: int) -> None:
                nonlocal directory_fsyncs
                if stat.S_ISDIR(os.fstat(fd).st_mode):
                    directory_fsyncs += 1
                    if directory_fsyncs == 2:
                        raise OSError("second post-replace parent fsync")
                real_fsync(fd)

            with mock.patch.object(TUI.os, "fsync", side_effect=fail_second_parent_fsync):
                with self.assertRaises(TUI.SubscriptionError) as raised:
                    TUI.write_subscription(path, new_value)

            self.assertIn("prior state restored", str(raised.exception))
            self.assertEqual(path.read_text(encoding="utf-8"), old_value)
            self.assertEqual(sorted(p.name for p in path.parent.iterdir()), [".vibe-vpn.lock", "sub_url"])

    def test_final_inode_verification_failure_restores_old_state(self) -> None:
        old_value = "https://old.example.invalid/final-check-token"
        new_value = "https://new.example.invalid/final-check-replacement"
        with tempfile.TemporaryDirectory() as temp_dir, local_secret_tree(temp_dir) as (_, path):
            path.write_text(old_value, encoding="utf-8")
            path.chmod(0o700)
            old_mode = stat.S_IMODE(path.stat().st_mode)
            with mock.patch.object(
                TUI,
                "_verify_replaced_inode",
                side_effect=TUI.SubscriptionError("forced final verification failure"),
            ):
                with self.assertRaises(TUI.SubscriptionError) as raised:
                    TUI.write_subscription(path, new_value)

            self.assertIn("prior state restored", str(raised.exception))
            self.assertEqual(path.read_text(encoding="utf-8"), old_value)
            self.assertEqual(stat.S_IMODE(path.stat().st_mode), old_mode)
            self.assertEqual(sorted(p.name for p in path.parent.iterdir()), [".vibe-vpn.lock", "sub_url"])

    def test_post_replace_failure_restores_originally_absent_path(self) -> None:
        new_value = "https://new.example.invalid/absent-path-token"
        with tempfile.TemporaryDirectory() as temp_dir, local_secret_tree(temp_dir) as (_, path):
            self.assertFalse(path.exists())
            with mock.patch.object(
                TUI,
                "_verify_replaced_inode",
                side_effect=TUI.SubscriptionError("forced final verification failure"),
            ):
                with self.assertRaises(TUI.SubscriptionError) as raised:
                    TUI.write_subscription(path, new_value)

            self.assertIn("prior state restored", str(raised.exception))
            self.assertFalse(path.exists())
            self.assertFalse(TUI.subscription_configured(path))
            self.assertEqual(sorted(p.name for p in path.parent.iterdir()), [".vibe-vpn.lock"])

    def test_rollback_fsync_failure_reports_incomplete_but_keeps_old_memory_state(self) -> None:
        old_value = "https://old.example.invalid/rollback-fsync-token"
        new_value = "https://new.example.invalid/rollback-fsync-replacement"
        with tempfile.TemporaryDirectory() as temp_dir, local_secret_tree(temp_dir) as (_, path):
            path.write_text(old_value, encoding="utf-8")
            path.chmod(0o400)
            old_mode = stat.S_IMODE(path.stat().st_mode)
            state = TUI.TuiState(path, runner=TUI.MockLifecycleRunner())
            self.assertTrue(state.subscription_ready)
            directory_fsyncs = 0
            real_fsync = TUI.os.fsync

            def fail_rollback_parent_fsync(fd: int) -> None:
                nonlocal directory_fsyncs
                if stat.S_ISDIR(os.fstat(fd).st_mode):
                    directory_fsyncs += 1
                    if directory_fsyncs == 2:
                        raise OSError("rollback parent fsync")
                real_fsync(fd)

            with mock.patch.object(TUI.os, "fsync", side_effect=fail_rollback_parent_fsync):
                with mock.patch.object(
                    TUI,
                    "_verify_replaced_inode",
                    side_effect=TUI.SubscriptionError("forced post-replace failure"),
                ):
                    with self.assertRaises(TUI.SubscriptionError) as raised:
                        state.save_subscription(new_value)

            self.assertIn("rollback incomplete", str(raised.exception))
            self.assertTrue(state.subscription_ready)
            self.assertEqual(path.read_text(encoding="utf-8"), old_value)
            self.assertEqual(stat.S_IMODE(path.stat().st_mode), old_mode)
            self.assertEqual(sorted(p.name for p in path.parent.iterdir()), [".vibe-vpn.lock", "sub_url"])

    def test_pre_replace_inode_race_is_rejected_without_overwriting_newer_inode(self) -> None:
        old_value = "https://old.example.invalid/pre-replace-race-token"
        newer_value = "https://new.example.invalid/pre-replace-newer-token"
        with tempfile.TemporaryDirectory() as temp_dir, local_secret_tree(temp_dir) as (_, path):
            path.write_text(old_value, encoding="utf-8")
            path.chmod(0o600)
            real_inspect = TUI._inspect_final_inode
            inspections = 0

            def race_on_last_check(parent_fd: int, name: str):
                nonlocal inspections
                result = real_inspect(parent_fd, name)
                inspections += 1
                if inspections == 2:
                    path.unlink()
                    path.write_text(newer_value, encoding="utf-8")
                    path.chmod(0o600)
                    return real_inspect(parent_fd, name)
                return result

            with mock.patch.object(TUI, "_inspect_final_inode", side_effect=race_on_last_check):
                with self.assertRaises(TUI.SubscriptionError):
                    TUI.write_subscription(path, "https://candidate.example.invalid/pre-replace-token")

            self.assertEqual(path.read_text(encoding="utf-8"), newer_value)
            self.assertEqual(sorted(p.name for p in path.parent.iterdir()), [".vibe-vpn.lock", "sub_url"])

    def test_rollback_race_does_not_overwrite_newer_inode(self) -> None:
        old_value = "https://old.example.invalid/race-token"
        newer_value = "https://new.example.invalid/newer-writer-token"
        with tempfile.TemporaryDirectory() as temp_dir, local_secret_tree(temp_dir) as (_, path):
            path.write_text(old_value, encoding="utf-8")
            path.chmod(0o600)

            def install_newer_inode(*_: object, **__: object) -> None:
                path.unlink()
                path.write_text(newer_value, encoding="utf-8")
                path.chmod(0o600)
                raise TUI.SubscriptionError("forced verification race")

            with mock.patch.object(TUI, "_verify_replaced_inode", side_effect=install_newer_inode):
                with self.assertRaises(TUI.SubscriptionError) as raised:
                    TUI.write_subscription(path, "https://candidate.example.invalid/candidate-token")

            self.assertIn("rollback incomplete", str(raised.exception))
            self.assertEqual(path.read_text(encoding="utf-8"), newer_value)
            self.assertEqual(sorted(p.name for p in path.parent.iterdir()), [".vibe-vpn.lock", "sub_url"])


class RunnerTests(unittest.TestCase):
    def test_builds_only_fixed_argv_and_uses_a_new_process_group(self) -> None:
        runner = TUI.AllowlistedSubprocessRunner("/tmp/mock-vpnkit-lifecycle")
        process = mock.MagicMock()
        process.pid = 1234
        process.returncode = 0
        process.wait.return_value = 0
        with mock.patch.object(TUI.subprocess, "Popen", return_value=process) as popen:
            result = runner.run(TUI.LifecycleAction.RETEST_SELECT)

        self.assertTrue(result.ok)
        argv, kwargs = popen.call_args
        self.assertEqual(argv[0], ["/tmp/mock-vpnkit-lifecycle", "retest", "select"])
        self.assertFalse(kwargs["shell"])
        self.assertTrue(kwargs["start_new_session"])
        self.assertEqual(kwargs["env"][TUI.TUI_SUPERVISED_ENV], "1")
        self.assertIs(kwargs["stdin"], subprocess.DEVNULL)
        self.assertIs(kwargs["stdout"], subprocess.DEVNULL)
        self.assertIs(kwargs["stderr"], subprocess.DEVNULL)
        process.wait.assert_called_once_with(timeout=1800.0)

    def test_timeout_terminates_descendant_process_group(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir:
            root = Path(temp_dir)
            executable = root / "lifecycle"
            ready = root / "child-ready"
            survivor = root / "child-survived"
            child_code = (
                "import pathlib,time; "
                f"pathlib.Path({str(ready)!r}).write_text('ready'); "
                "time.sleep(0.8); "
                f"pathlib.Path({str(survivor)!r}).write_text('survived'); "
                "time.sleep(5)"
            )
            executable.write_text(
                "#!/usr/bin/env python3\n"
                "import subprocess,sys,time\n"
                f"subprocess.Popen([sys.executable, '-c', {child_code!r}])\n"
                "time.sleep(5)\n",
                encoding="utf-8",
            )
            executable.chmod(0o700)
            runner = TUI.AllowlistedSubprocessRunner(str(executable), timeout=0.2)

            result = runner.run(TUI.LifecycleAction.START)
            self.assertEqual(result.reason, "timeout")
            self.assertTrue(ready.exists())
            time.sleep(1.0)
            self.assertFalse(survivor.exists(), "a timed-out descendant survived the process-group kill")

    def test_term_cancels_the_supervised_group_before_returning(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir:
            root = Path(temp_dir)
            executable = root / "lifecycle"
            ready = root / "runner-ready"
            survivor = root / "survivor"
            result_file = root / "result"
            child_code = (
                "import pathlib,time; "
                f"pathlib.Path({str(ready)!r}).touch(); "
                "time.sleep(5); "
                f"pathlib.Path({str(survivor)!r}).write_text('survived')"
            )
            executable.write_text(
                "#!/usr/bin/env python3\n"
                "import subprocess,sys,time\n"
                f"subprocess.Popen([sys.executable, '-c', {child_code!r}])\n"
                "time.sleep(10)\n",
                encoding="utf-8",
            )
            executable.chmod(0o700)
            wrapper = (
                "import importlib.util, pathlib, sys\n"
                f"spec=importlib.util.spec_from_file_location('tui_cancel', {str(MODULE_PATH)!r})\n"
                "module=importlib.util.module_from_spec(spec); sys.modules['tui_cancel']=module; spec.loader.exec_module(module)\n"
                f"runner=module.AllowlistedSubprocessRunner({str(executable)!r}, timeout=30)\n"
                "result=runner.run(module.LifecycleAction.START)\n"
                f"pathlib.Path({str(result_file)!r}).write_text(result.reason)\n"
            )
            process = subprocess.Popen([sys.executable, "-c", wrapper])
            try:
                wait_for_file(ready)
                process.terminate()
                process.wait(timeout=10)
            finally:
                if process.poll() is None:
                    process.kill()
                    process.wait(timeout=10)
            self.assertEqual(result_file.read_text(encoding="ascii"), "cancelled")
            time.sleep(0.2)
            self.assertFalse(survivor.exists(), "TERM returned while a lifecycle descendant was alive")

    def test_rejects_shell_tokens_and_direct_mutation_tools(self) -> None:
        with self.assertRaises(ValueError):
            TUI.AllowlistedSubprocessRunner("/bin/sh -c")
        with self.assertRaises(ValueError):
            TUI.AllowlistedSubprocessRunner("docker")
        with self.assertRaises(ValueError):
            TUI.AllowlistedSubprocessRunner("/usr/bin/sudo")

    def test_mock_runner_records_only_known_actions(self) -> None:
        runner = TUI.MockLifecycleRunner()
        result = runner.run("start")
        self.assertTrue(result.ok)
        self.assertEqual(runner.actions, [TUI.LifecycleAction.START])
        with self.assertRaises(ValueError):
            runner.run("arbitrary-command")


class StateAndStatusTests(unittest.TestCase):
    def test_attempt_logs_private_output_and_exposes_only_safe_reason(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir, local_secret_tree(temp_dir) as (root, _):
            adapter = Path(temp_dir) / "fake-lifecycle"
            adapter.write_text(
                "#!/usr/bin/env python3\nimport sys\n"
                "print('private-fixture-token-not-for-ui', flush=True)\n"
                "print('vpnkit_phase=host-smoke', file=sys.stderr, flush=True)\n"
                "print('IPv4 route did not use the exact local VPN device', file=sys.stderr, flush=True)\n"
                "print('local stack was rolled back', file=sys.stderr, flush=True)\n"
                "raise SystemExit(1)\n"
            )
            adapter.chmod(0o700)
            runner = TUI.AllowlistedSubprocessRunner(adapter, record_attempts=True)
            state = TUI.TuiState(None, runner=runner)
            result = state.dispatch("start")
            self.assertEqual(result.reason, "route-conflict")
            self.assertTrue(result.attempt_id)
            directory = root / "diagnostics"
            self.assertEqual(stat.S_IMODE(directory.stat().st_mode), 0o700)
            logfile = directory / (result.attempt_id + ".log")
            metadata = directory / (result.attempt_id + ".json")
            self.assertEqual(stat.S_IMODE(logfile.stat().st_mode), 0o600)
            self.assertEqual(stat.S_IMODE(metadata.stat().st_mode), 0o600)
            self.assertIn("private-fixture-token-not-for-ui", logfile.read_text())
            self.assertIn("local stack was rolled back", logfile.read_text())
            data = json.loads(metadata.read_text())
            self.assertEqual(data["state"], "finished")
            self.assertEqual(data["returncode"], 1)
            self.assertEqual(data["reason"], "route-conflict")
            safe_output = json.dumps(state.status()) + metadata.read_text()
            self.assertNotIn("private-fixture-token-not-for-ui", safe_output)
            self.assertNotIn(str(root), safe_output)
            bridge = subprocess.run(
                [sys.executable, str(MODULE_PATH), "--bridge", "--lifecycle-executable", str(adapter)],
                input='{"action":"start"}\n', capture_output=True, text=True, timeout=5,
            )
            self.assertEqual(bridge.returncode, 0)
            reply = json.loads(bridge.stdout)
            self.assertEqual(reply["reason"], "route-conflict")
            self.assertTrue((directory / (reply["status"]["last_attempt"] + ".log")).is_file())
            self.assertNotIn("private-fixture-token-not-for-ui", bridge.stdout + bridge.stderr)
            self.assertNotIn(str(root), bridge.stdout + bridge.stderr)

    def test_attempt_log_cap_retains_late_failure_and_rolls_no_output_into_ui(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir, local_secret_tree(temp_dir) as (root, _):
            adapter = Path(temp_dir) / "fake-lifecycle"
            adapter.write_text(
                "#!/usr/bin/env python3\n"
                "print('x' * 200000)\n"
                "print('hostname HTTPS smoke failed')\n"
                "print('local stack was rolled back')\n"
                "raise SystemExit(1)\n"
            )
            adapter.chmod(0o700)
            with mock.patch.object(TUI, "MAX_ATTEMPT_LOG_BYTES", 1024):
                result = TUI.AllowlistedSubprocessRunner(adapter, record_attempts=True).run("start")
            self.assertEqual(result.reason, "hostname-https-failed")
            logfile = root / "diagnostics" / (result.attempt_id + ".log")
            self.assertLess(logfile.stat().st_size, 1200)
            self.assertIn("local stack was rolled back", logfile.read_text())
            metadata = json.loads((root / "diagnostics" / (result.attempt_id + ".json")).read_text())
            self.assertTrue(metadata["truncated"])

    def test_unsafe_diagnostics_directory_prevents_action(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir, local_secret_tree(temp_dir) as (root, _):
            (root / "diagnostics").symlink_to(Path(temp_dir))
            runner = TUI.AllowlistedSubprocessRunner("/does-not-run", record_attempts=True)
            with mock.patch.object(TUI.subprocess, "Popen") as popen:
                result = runner.run("start")
            self.assertEqual(result.reason, "diagnostics-unavailable")
            popen.assert_not_called()

    def test_timed_out_attempt_records_completion(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir, local_secret_tree(temp_dir) as (root, _):
            adapter = Path(temp_dir) / "fake-lifecycle"
            adapter.write_text("#!/usr/bin/env python3\nimport time\nprint('started', flush=True)\ntime.sleep(10)\n")
            adapter.chmod(0o700)
            result = TUI.AllowlistedSubprocessRunner(adapter, timeout=0.2, record_attempts=True).run("start")
            self.assertEqual(result.reason, "timeout")
            metadata = json.loads((root / "diagnostics" / (result.attempt_id + ".json")).read_text())
            self.assertEqual(metadata["state"], "finished")
            self.assertEqual(metadata["reason"], "timeout")

    def test_bridge_retest_delegates_backend_readiness_without_host_vpn(self) -> None:
        for gateway in ("absent", "stopped", "starting", "unhealthy", "unknown", "healthy"):
            with self.subTest(gateway=gateway):
                runner = TUI.MockLifecycleRunner()
                runner.query_status = lambda: {
                    "container": gateway,
                    "networkmanager": {"configured": "yes", "active": "no"},
                }
                state = TUI.TuiState(None, runner=runner)
                stdin = io.TextIOWrapper(io.BytesIO(b'{"action":"retest/select"}\n'))
                stdout = io.StringIO()
                with mock.patch.object(TUI.sys, "stdin", stdin), mock.patch.object(TUI.sys, "stdout", stdout):
                    self.assertEqual(TUI.run_bridge(state), 0)
                reply = json.loads(stdout.getvalue())
                self.assertEqual(reply["status"]["gateway_state"], gateway)
                self.assertEqual(reply["status"]["vpn_state"], "inactive")
                self.assertTrue(reply["ok"])
                self.assertEqual(runner.actions, [TUI.LifecycleAction.RETEST_SELECT])

    def test_opentui_signals_wait_for_operation_before_destroying_renderer(self) -> None:
        import pty
        import select
        import shutil
        import signal
        import struct
        import termios

        bun = shutil.which("bun")
        if not bun or not (ROOT / "scripts/vpnkit/tui/node_modules/@opentui/core").is_dir():
            self.skipTest("OpenTUI signal acceptance requires Bun and installed TUI dependencies")
        for signum in (signal.SIGTERM, signal.SIGINT, signal.SIGHUP):
            with self.subTest(signal=signum), tempfile.TemporaryDirectory() as temp_dir, local_secret_tree(temp_dir):
                fixture = Path(temp_dir)
                adapter = fixture / "fake-lifecycle"
                adapter.write_text(
                    "#!/usr/bin/env python3\n"
                    "import json, pathlib, sys, time\n"
                    "root = pathlib.Path(__file__).parent\n"
                    "if sys.argv[1] == 'status':\n"
                    " print(json.dumps({'container': 'healthy', 'subscription': 'configured', "
                    "'networkmanager': {'configured': 'yes', 'active': 'no'}}))\n"
                    "else:\n"
                    " (root / 'started').touch()\n"
                    " time.sleep(0.5)\n"
                    " (root / 'completed').touch()\n"
                )
                adapter.chmod(0o700)
                master, slave = pty.openpty()
                TUI.fcntl.ioctl(slave, termios.TIOCSWINSZ, struct.pack("HHHH", 24, 80, 0, 0))
                process = subprocess.Popen(
                    [bun, str(ROOT / "scripts/vpnkit/tui/index.ts"), "--lifecycle-executable", str(adapter)],
                    stdin=slave, stdout=slave, stderr=slave,
                )
                os.close(slave)
                output = bytearray()

                def drain() -> None:
                    if select.select([master], [], [], 0.05)[0]:
                        try:
                            output.extend(os.read(master, 65536))
                        except OSError:
                            pass

                try:
                    deadline = time.monotonic() + 5
                    while "VPN отключён".encode() not in output and time.monotonic() < deadline:
                        drain()
                    self.assertIn("VPN отключён".encode(), output)
                    os.write(master, b"s")
                    while not (fixture / "started").exists() and time.monotonic() < deadline:
                        drain()
                    self.assertTrue((fixture / "started").exists())
                    process.send_signal(signum)
                    deadline = time.monotonic() + 5
                    while process.poll() is None and time.monotonic() < deadline:
                        drain()
                    drain()
                    self.assertEqual(process.poll(), 0)
                    self.assertTrue((fixture / "completed").exists())
                    self.assertNotIn(b"TextBuffer is destroyed", output)
                    self.assertIn(b"\x1b[?1049l", output)
                finally:
                    if process.poll() is None:
                        process.kill()
                    process.wait(timeout=5)
                    os.close(master)

    def test_failed_status_probe_clears_previously_active_connection(self) -> None:
        runner = TUI.MockLifecycleRunner()
        state = TUI.TuiState(None, runner=runner)
        state.vpn_state = "healthy"
        state.networkmanager_configured = "yes"
        state.networkmanager_active = "yes"
        state.refresh()
        self.assertEqual(state.vpn_state, "unknown")
        self.assertEqual(state.networkmanager_active, "unknown")
        self.assertEqual(state.networkmanager_configured, "unknown")

    def test_bridge_rejects_arbitrary_actions_without_echoing_input(self) -> None:
        completed = subprocess.run(
            [sys.executable, str(MODULE_PATH), "--bridge", "--test"],
            input='{"action":"private-value-must-not-appear"}\n{"action":"status"}\n',
            text=True, capture_output=True, timeout=5,
        )
        self.assertEqual(completed.returncode, 0)
        replies = [json.loads(line) for line in completed.stdout.splitlines()]
        self.assertFalse(replies[0]["ok"])
        self.assertTrue(replies[1]["ok"])
        self.assertNotIn("private-value", completed.stdout + completed.stderr)

    def test_status_is_redacted_and_toggle_is_deterministic(self) -> None:
        secret = "subscription-token-must-not-appear"
        with tempfile.TemporaryDirectory() as temp_dir, local_secret_tree(temp_dir) as (_, path):
            TUI.write_subscription(path, secret)
            state = TUI.TuiState(path, runner=TUI.MockLifecycleRunner())

            before = json.dumps(state.status(), sort_keys=True)
            self.assertNotIn(secret, before)
            self.assertNotIn(str(path), before)
            self.assertEqual(state.status()["subscription"], "configured")
            self.assertEqual(state.status()["endpoint"], "redacted")

            self.assertEqual(state.status()["routing_mode"], "strict")
            state.dispatch(TUI.LifecycleAction.TOGGLE_MODE)
            self.assertEqual(state.status()["routing_mode"], "smart")
            state.dispatch(TUI.LifecycleAction.RETEST_SELECT)
            state.dispatch(TUI.LifecycleAction.DIAGNOSTICS)
            self.assertEqual(state.status()["selection"], "redacted")
            self.assertEqual(state.status()["diagnostics"], "available")

    def test_refresh_consumes_redacted_networkmanager_fields(self) -> None:
        class LifecycleStatus:
            def query_status(self):
                return {
                    "container": "healthy",
                    "routing_policy": "strict",
                    "subscription": "configured",
                    "networkmanager": {"configured": "yes", "active": "no"},
                }

            def run(self, action):
                return TUI.CommandResult(TUI.normalize_action(action), 0)

        state = TUI.TuiState(None, runner=LifecycleStatus())
        state.refresh()
        self.assertEqual(state.networkmanager_configured, "yes")
        self.assertEqual(state.networkmanager_active, "no")
        self.assertEqual(state.vpn_state, "inactive")
        self.assertNotIn(state.vpn_state, {"healthy", "running"})
        self.assertEqual(state.status()["networkmanager_configured"], "yes")
        self.assertEqual(state.status()["networkmanager_active"], "no")

        class MissingManagedProfileStatus:
            def query_status(self):
                return {
                    "container": "healthy",
                    "networkmanager": {"configured": "no", "active": "no"},
                }

            def run(self, action):
                return TUI.CommandResult(TUI.normalize_action(action), 0)

        missing_managed = TUI.TuiState(None, runner=MissingManagedProfileStatus())
        missing_managed.refresh()
        self.assertEqual(missing_managed.vpn_state, "inactive")

        class ContainerOnlyStatus:
            def query_status(self):
                return {
                    "container": "healthy",
                    "networkmanager": {"configured": "not-managed", "active": "not-managed"},
                }

            def run(self, action):
                return TUI.CommandResult(TUI.normalize_action(action), 0)

        container_only = TUI.TuiState(None, runner=ContainerOnlyStatus())
        container_only.refresh()
        self.assertEqual(container_only.vpn_state, "healthy")
        self.assertEqual(container_only.networkmanager_configured, "not-managed")

class CommandLineTests(unittest.TestCase):
    def test_status_json_is_noninteractive_and_contains_no_secret(self) -> None:
        secret = "cli-secret-never-printed"
        with tempfile.TemporaryDirectory() as temp_dir, local_secret_tree(temp_dir) as (root, path):
            TUI.write_subscription(path, secret)
            env = os.environ.copy()
            env[TUI.LOCAL_SECRETS_ENV] = str(root)
            completed = subprocess.run(
                [sys.executable, str(MODULE_PATH), "--status-json", "--test", "--subscription-path", str(path)],
                check=False,
                capture_output=True,
                text=True,
                env=env,
            )

        self.assertEqual(completed.returncode, 0, completed.stderr)
        status = json.loads(completed.stdout)
        self.assertEqual(status["subscription"], "configured")
        self.assertNotIn(secret, completed.stdout)
        self.assertNotIn(str(path), completed.stdout)

    def test_status_mode_queries_fixed_action_and_sanitizes_networkmanager(self) -> None:
        process = mock.MagicMock()
        process.pid = 1234
        process.returncode = 0
        process.poll.return_value = 0
        process.stdout = io.BytesIO()
        payload = (
            b'{"schema":1,"container":"healthy","routing_policy":"strict",'
            b'"subscription":"configured","networkmanager":{"configured":"yes","active":"no",'
            b'"private":"must-drop"}}'
        )
        with mock.patch.object(TUI.subprocess, "Popen", return_value=process) as popen:
            with mock.patch.object(TUI, "_read_bounded_output", return_value=(payload, None)):
                runner = TUI.AllowlistedSubprocessRunner("/tmp/mock-vpnkit-lifecycle")
                status = runner.query_status()

        argv, kwargs = popen.call_args
        self.assertEqual(argv[0], ["/tmp/mock-vpnkit-lifecycle", "status", "--json"])
        self.assertFalse(kwargs["shell"])
        self.assertTrue(kwargs["start_new_session"])
        self.assertEqual(status["networkmanager"], {"configured": "yes", "active": "no"})
        self.assertNotIn("private", json.dumps(status))
        self.assertEqual(status["container"], "healthy")

    def test_oversized_status_is_rejected_while_reading(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir:
            root = Path(temp_dir)
            executable = root / "lifecycle"
            survivor = root / "survivor"
            executable.write_text(
                "#!/usr/bin/env python3\n"
                "import pathlib,sys,time\n"
                "if sys.argv[1] == 'status':\n"
                f"    sys.stdout.write('x' * {TUI.MAX_STATUS_BYTES + 4096})\n"
                "    sys.stdout.flush()\n"
                "    time.sleep(5)\n"
                f"    pathlib.Path({str(survivor)!r}).write_text('survived')\n",
                encoding="utf-8",
            )
            executable.chmod(0o700)
            runner = TUI.AllowlistedSubprocessRunner(str(executable))
            started = time.monotonic()
            self.assertEqual(runner.query_status(), {})
            self.assertLess(time.monotonic() - started, 2.0)
            time.sleep(0.2)
            self.assertFalse(survivor.exists())


class OpenTuiBridgeExtensionsTests(unittest.TestCase):
    def test_editor_read_is_explicit_and_rejects_symlinks(self):
        with tempfile.TemporaryDirectory() as temp, local_secret_tree(temp) as (root, path):
            TUI.write_subscription(path, "https://example.invalid/editor-token")
            self.assertEqual(TUI.read_subscription_for_editor(path), "https://example.invalid/editor-token")
            self.assertNotIn("editor-token", json.dumps(TUI.TuiState(path).status()))
            path.unlink()
            path.symlink_to(Path(temp) / "outside")
            with self.assertRaises((ValueError, OSError)):
                TUI.read_subscription_for_editor(path)

    def test_server_transport_preserves_metrics_without_child_diagnostics(self):
        with tempfile.TemporaryDirectory() as temp, local_secret_tree(temp) as (_, path):
            adapter = Path(temp) / "adapter"
            row = {"server_id":"srv_" + "a" * 27, "display_name":"Amsterdam", "status":"ready", "selected":False,
                   "ping_status":"ready", "latency_ms":17, "download_mbps":24, "download_seconds":3,
                   "private_endpoint":"must-not-cross"}
            payload = {"schema":"vibe-vpn.server-browser.v2", "status":"ok", "generation":1, "server":row}
            adapter.write_text("#!/usr/bin/env python3\nimport sys\nprint('private-diagnostic',file=sys.stderr)\nprint(" + repr(json.dumps(payload)) + ")\n")
            adapter.chmod(0o700)
            state = TUI.TuiState(path, runner=TUI.AllowlistedSubprocessRunner(adapter))
            data = TUI.run_server_request(state, "servers/speed", row["server_id"])
            self.assertEqual(data["server"]["latency_ms"],17)
            self.assertEqual(data["server"]["download_mbps"],24)
            self.assertNotIn("must-not-cross",json.dumps(data))
            self.assertNotIn("private-diagnostic",json.dumps(data))
            with self.assertRaises(ValueError):
                TUI.run_server_request(state,"servers/speed","--raw-command")

    def test_batch_transport_bounds_ids_and_uses_fixed_arguments(self):
        with tempfile.TemporaryDirectory() as temp, local_secret_tree(temp) as (_, path):
            adapter = Path(temp) / "adapter"
            ids = ["srv_" + str(i).zfill(27) for i in range(5)]
            target = "https://example.com/watch?a=1&b=2"
            payload = {"schema": "vibe-vpn.server-browser.v2", "status": "ok",
                       "servers": [{"server_id": i} for i in ids]}
            adapter.write_text("#!/usr/bin/env python3\nimport sys\nassert sys.argv[1:] == " +
                repr(["servers", "check-batch", ",".join(ids), target]) + "\nprint(" + repr(json.dumps(payload)) + ")\n")
            adapter.chmod(0o700)
            state = TUI.TuiState(path, runner=TUI.AllowlistedSubprocessRunner(adapter))
            value = json.dumps({"ids": ids, "url": target})
            result = TUI.run_server_request(state, "servers/check-batch", value)
            self.assertEqual(len(result["servers"]), 5)
            for bad in [[], ids + ["srv_" + "x" * 27], [ids[0], ids[0]]]:
                with self.assertRaises(ValueError):
                    TUI.run_server_request(state, "servers/check-batch", json.dumps({"ids": bad, "url": target}))
            payload["servers"] = payload["servers"][:1]
            adapter.write_text("#!/usr/bin/env python3\nprint(" + repr(json.dumps(payload)) + ")\n")
            with self.assertRaises(ValueError):
                TUI.run_server_request(state, "servers/check-batch", value)

    def test_server_cancellation_reaps_children_and_keeps_bridge_usable(self):
        with tempfile.TemporaryDirectory() as temp, local_secret_tree(temp) as (_, path):
            adapter=Path(temp)/"adapter"
            started=Path(temp)/"started"
            survivor=Path(temp)/"survivor"
            child="import time; from pathlib import Path; time.sleep(.3); Path(" + repr(str(survivor)) + ").touch()"
            adapter.write_text("#!/usr/bin/env python3\nimport sys,subprocess,time\nfrom pathlib import Path\n"
                "if 'status' in sys.argv: print('{}')\nelse:\n subprocess.Popen([sys.executable,'-c'," + repr(child) + "])\n Path(" + repr(str(started)) + ").touch()\n time.sleep(30)\n")
            adapter.chmod(0o700)
            bridge=subprocess.Popen([sys.executable,str(MODULE_PATH),"--bridge","--lifecycle-executable",str(adapter)],stdin=subprocess.PIPE,stdout=subprocess.PIPE,stderr=subprocess.PIPE,text=True)
            try:
                bridge.stdin.write('{"action":"servers/list"}\n');bridge.stdin.flush()
                wait_for_file(started)
                bridge.send_signal(__import__('signal').SIGUSR1)
                out,err=bridge.communicate('{"action":"status"}\n',timeout=5)
                replies=[json.loads(line) for line in out.splitlines()]
                self.assertEqual(replies[0]["reason"],"cancelled")
                self.assertTrue(replies[1]["ok"])
                self.assertEqual(bridge.returncode,0)
                time.sleep(.35)
                self.assertFalse(survivor.exists())
            finally:
                if bridge.poll() is None: bridge.kill();bridge.communicate()

    def test_progress_protocol_is_opt_in_and_only_emits_known_phases(self):
        with tempfile.TemporaryDirectory() as temp, local_secret_tree(temp) as (_, path):
            adapter=Path(temp)/"adapter"
            adapter.write_text("#!/usr/bin/env python3\nimport sys\nif 'status' in sys.argv: print('{}')\nelse: print('vpnkit_phase=compose-up\\nvpnkit_phase=private-value')\n")
            adapter.chmod(0o700)
            result=subprocess.run([sys.executable,str(MODULE_PATH),"--bridge","--lifecycle-executable",str(adapter)],
                input='{"action":"backend/start","progress":true}\n',capture_output=True,text=True,timeout=5)
            messages=[json.loads(line) for line in result.stdout.splitlines()]
            self.assertIn({"event":"progress","phase":"compose-up"},messages)
            self.assertTrue(messages[-1]["ok"])
            self.assertNotIn("private-value",result.stdout)



class OpenTuiSetupTerminalTests(unittest.TestCase):
    def test_setup_hands_terminal_to_sudo_and_resumes(self):
        import fcntl
        import pty
        import select
        import shutil
        import signal
        import struct
        import termios

        bun = shutil.which("bun")
        dependencies = ROOT / "scripts/vpnkit/tui/node_modules"
        if not bun or not dependencies.exists():
            self.skipTest("installed OpenTUI dependencies required")
        with tempfile.TemporaryDirectory(prefix="vpnkit-local-setup-") as temp:
            root = Path(temp)
            ui = root / "scripts/vpnkit/tui"
            ui.mkdir(parents=True)
            for name in ("setup-index.ts", "setup.ts", "setup-backend.ts", "model.ts"):
                shutil.copyfile(ROOT / "scripts/vpnkit/tui" / name, ui / name)
            (ui / "node_modules").symlink_to(dependencies, target_is_directory=True)
            (root / ".build").mkdir()
            binary = root / ".build/local-vpn-kde.bin"
            binary.write_text("#!/usr/bin/env python3\n"
                "import json,os,pathlib,sys\n"
                "if sys.argv[1] == 'ui':\n print('app-open-fixture'); raise SystemExit(0)\n"
                "assert 'VPNKIT_LOCAL_SECRETS_DIR' not in os.environ\n"
                "action=sys.argv[sys.argv.index('--action')+1]\n"
                "if action=='install':\n"
                " assert pathlib.Path(os.environ['SETUP_AUTH_MARKER']).read_text()=='ok'\n"
                " print(json.dumps({'event':'progress','phase':'setup-profile'}))\n"
                "print(json.dumps({'event':'result','ok':True,'reason':'ok'}))\n")
            binary.chmod(0o700)
            mock_bin = root / "bin"
            mock_bin.mkdir()
            sudo = mock_bin / "sudo"
            sudo.write_text("#!/usr/bin/env bash\nset -euo pipefail\n"
                "[[ $1 == -v && -t 0 && -t 1 ]]\n"
                "trap 'stty echo' EXIT\nstty -echo\n"
                "printf 'fixture-password: '\nread -r password\n"
                "[[ $password == harmless-fixture-password ]]\n"
                "printf ok >\"$SETUP_AUTH_MARKER\"\nprintf '\\n'\n")
            sudo.chmod(0o700)
            pid, master = pty.fork()
            if pid == 0:
                os.chdir(root)
                env = {**os.environ, "TERM":"xterm-256color", "PATH":str(mock_bin)+":"+os.environ["PATH"],
                       "SETUP_AUTH_MARKER":str(root / "authorized"), "VPNKIT_LOCAL_SECRETS_DIR":"/not-the-current-installation"}
                os.execve(bun, [bun, str(ui / "setup-index.ts")], env)
            fcntl.ioctl(master, termios.TIOCSWINSZ, struct.pack("HHHH",24,80,0,0))
            transcript = bytearray()
            reaped = False

            def until(text):
                wanted = text.encode()
                deadline = time.monotonic() + 10
                while wanted not in transcript and time.monotonic() < deadline:
                    if select.select([master], [], [], .05)[0]:
                        try:
                            data = os.read(master, 65536)
                        except OSError:
                            break
                        if not data:
                            break
                        transcript.extend(data)
                self.assertIn(wanted, transcript, "setup terminal transition did not complete")

            try:
                until("Начать установку")
                os.write(master, b"\r")
                until("fixture-password: ")
                os.write(master, b"harmless-fixture-password\n")
                until("Открыть программу")
                self.assertNotIn(b"harmless-fixture-password", transcript)
                self.assertNotIn(b"TextBuffer is destroyed", transcript)
                os.write(master, b"\r")
                until("app-open-fixture")
                deadline = time.monotonic() + 5
                while time.monotonic() < deadline:
                    child, status = os.waitpid(pid, os.WNOHANG)
                    if child:
                        reaped = True
                        self.assertEqual(os.waitstatus_to_exitcode(status), 0)
                        break
                    time.sleep(.02)
                self.assertTrue(reaped, "setup launcher did not exit after the app")
            finally:
                if not reaped:
                    try:
                        os.killpg(pid, signal.SIGKILL)
                    except ProcessLookupError:
                        pass
                    os.waitpid(pid, 0)
                os.close(master)

if __name__ == "__main__":
    unittest.main()
