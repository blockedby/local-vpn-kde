package state

import (
	"bytes"
	"context"
	"errors"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestLoadOrCreateSecretLockedPersistsPrivateIdentity(t *testing.T) {
	dir := t.TempDir()
	first, err := LoadOrCreateSecretLocked(dir, ".identity-salt", 32)
	if err != nil {
		t.Fatal(err)
	}
	second, err := LoadOrCreateSecretLocked(dir, ".identity-salt", 32)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) || len(first) != 32 {
		t.Fatal("private identity secret was not stable")
	}
	info, err := os.Stat(filepath.Join(dir, ".identity-salt"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode() != 0600 {
		t.Fatalf("secret mode=%o", info.Mode())
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Nlink != 1 || int(stat.Uid) != os.Geteuid() {
		t.Fatalf("secret ownership/link metadata is unsafe: %#v", info.Sys())
	}
	temps, err := filepath.Glob(filepath.Join(dir, ".identity-salt.*.tmp"))
	if err != nil || len(temps) != 0 {
		t.Fatalf("secret creation left temporary files: %v err=%v", temps, err)
	}
}

func TestLoadOrCreateSecretLockedRejectsSymlinkAndMalformedSecret(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, make([]byte, 32), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, ".symlink-salt")); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateSecretLocked(dir, ".symlink-salt", 32); err == nil {
		t.Fatal("symlink secret was accepted")
	}
	if err := os.WriteFile(filepath.Join(dir, ".short-salt"), []byte("short"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateSecretLocked(dir, ".short-salt", 32); err == nil {
		t.Fatal("malformed secret was accepted")
	}
}

func TestLoadOrCreateSecretLockedRejectsModeHardlinkAndForeignOwner(t *testing.T) {
	for _, test := range []struct {
		name  string
		setup func(*testing.T, string, string)
	}{
		{name: "mode", setup: func(t *testing.T, _ string, path string) {
			if err := os.WriteFile(path, make([]byte, 32), 0644); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "hardlink", setup: func(t *testing.T, dir, path string) {
			if err := os.WriteFile(path, make([]byte, 32), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Link(path, filepath.Join(dir, "second-link")); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, ".identity-salt")
			test.setup(t, dir, path)
			if _, err := LoadOrCreateSecretLocked(dir, filepath.Base(path), 32); err == nil {
				t.Fatalf("%s-invalid secret was accepted", test.name)
			}
			if test.name == "mode" {
				info, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				if info.Mode().Perm() != 0644 {
					t.Fatalf("mode-invalid secret was repaired: mode=%v", info.Mode().Perm())
				}
			}
		})
	}

	dir := t.TempDir()
	path := filepath.Join(dir, ".identity-salt")
	if err := os.WriteFile(path, make([]byte, 32), 0600); err != nil {
		t.Fatal(err)
	}
	previousUID := privateFileUID
	privateFileUID = func() int { return os.Geteuid() + 1 }
	t.Cleanup(func() { privateFileUID = previousUID })
	if _, err := LoadOrCreateSecretLocked(dir, filepath.Base(path), 32); err == nil {
		t.Fatal("foreign-owner secret was accepted")
	}
}

func TestLoadOrCreateSecretLockedRejectsLinkedParentAndReplacementRace(t *testing.T) {
	realDir := t.TempDir()
	linkedParent := filepath.Join(t.TempDir(), "state-link")
	if err := os.Symlink(realDir, linkedParent); err != nil {
		t.Fatal(err)
	}
	linkedDir := filepath.Join(linkedParent, "new-state")
	if _, err := LoadOrCreateSecretLocked(linkedDir, ".identity-salt", 32); err == nil {
		t.Fatal("symlinked state directory parent was accepted")
	}
	if _, err := os.Stat(filepath.Join(realDir, "new-state")); !os.IsNotExist(err) {
		t.Fatalf("linked parent target was modified: %v", err)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, ".identity-salt")
	replacement := filepath.Join(dir, "replacement")
	if err := os.WriteFile(path, bytes.Repeat([]byte{1}, 32), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(replacement, bytes.Repeat([]byte{2}, 32), 0600); err != nil {
		t.Fatal(err)
	}
	previousHook := privateFileReadOpenedHook
	privateFileReadOpenedHook = func() {
		privateFileReadOpenedHook = nil
		if err := os.Rename(path, filepath.Join(dir, "old-secret")); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(replacement, path); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { privateFileReadOpenedHook = previousHook })
	if _, err := LoadOrCreateSecretLocked(dir, filepath.Base(path), 32); err == nil {
		t.Fatal("replacement race was accepted")
	}
}

func TestRemovePrivateFileLockedDurablyRemovesOnlyValidatedFiles(t *testing.T) {
	dir := t.TempDir()
	if err := WritePrivateFileAtomicLocked(context.Background(), dir, ".publication-journal", []byte("private\n")); err != nil {
		t.Fatal(err)
	}
	if err := RemovePrivateFileLocked(dir, ".publication-journal", 4096); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(dir, ".publication-journal")); !os.IsNotExist(err) {
		t.Fatalf("validated private file was not removed: %v", err)
	}
	if err := RemovePrivateFileLocked(dir, ".publication-journal", 4096); err != nil {
		t.Fatalf("idempotent private removal failed: %v", err)
	}

	for _, test := range []struct {
		name  string
		setup func(*testing.T, string)
	}{
		{name: "symlink", setup: func(t *testing.T, path string) {
			target := path + ".target"
			if err := os.WriteFile(target, []byte("private\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, path); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "hardlink", setup: func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte("private\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Link(path, path+".second"); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "mode", setup: func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte("private\n"), 0644); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			testDir := t.TempDir()
			path := filepath.Join(testDir, ".publication-journal")
			test.setup(t, path)
			if err := RemovePrivateFileLocked(testDir, filepath.Base(path), 4096); err == nil {
				t.Fatalf("unsafe %s file was removed", test.name)
			}
			if _, err := os.Lstat(path); err != nil {
				t.Fatalf("unsafe %s path changed: %v", test.name, err)
			}
		})
	}
}

func TestWritePrivateFileAtomicLockedReportsPostRenameDirectorySyncFailure(t *testing.T) {
	dir := t.TempDir()
	lock, err := AcquireLock(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()

	injected := false
	restore := SetPrivateFileDirectorySyncHookForTesting(func(operation, name string) error {
		if operation == "write" && name == ".publication-journal" && !injected {
			injected = true
			return errors.New("injected directory fsync failure")
		}
		return nil
	})
	t.Cleanup(restore)
	body := []byte("private journal\n")
	if err := WritePrivateFileAtomicLocked(context.Background(), dir, ".publication-journal", body); err == nil || !injected {
		t.Fatalf("post-rename directory sync fault err=%v injected=%t", err, injected)
	}
	visible, err := os.ReadFile(filepath.Join(dir, ".publication-journal"))
	if err != nil || !bytes.Equal(visible, body) {
		t.Fatalf("renamed file was not visible after directory sync fault body=%q err=%v", visible, err)
	}
}

func TestCurrentBaselineValidationFailsClosed(t *testing.T) {
	for name, current := range map[string]Current{
		"zero":     {},
		"negative": {Mbps: -1},
		"nan":      {Mbps: math.NaN()},
		"infinity": {Mbps: math.Inf(1)},
	} {
		t.Run(name, func(t *testing.T) {
			if current.ValidateBaseline() == nil || current.HasValidBaseline() {
				t.Fatal("invalid baseline was accepted")
			}
		})
	}
}

func TestVersionedJSONPublicationRejectsStaleAndSameContentVersions(t *testing.T) {
	dir := t.TempDir()
	first, err := SaveJSONVersioned(context.Background(), dir, "last-results.json", map[string]string{"result": "same"})
	if err != nil {
		t.Fatal(err)
	}
	second, committed, err := SaveJSONIfVersion(context.Background(), dir, "last-results.json", map[string]string{"result": "same"}, first)
	if err != nil || !committed {
		t.Fatalf("same-content publication committed=%t err=%v", committed, err)
	}
	if first.Equal(second) {
		t.Fatal("atomic same-content publication retained the old file version")
	}
	if restored, err := RestoreFileIfVersion(context.Background(), dir, "last-results.json", first, []byte("stale\n"), 0600); err != nil {
		t.Fatal(err)
	} else if restored {
		t.Fatal("stale conditional restore unexpectedly committed")
	}
	got, err := os.ReadFile(filepath.Join(dir, "last-results.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(got, []byte(`"same"`)) {
		t.Fatalf("stale restore changed current result: %s", got)
	}
}

func TestDeleteIfVersionOnlyRemovesExactCandidate(t *testing.T) {
	dir := t.TempDir()
	candidate, err := SaveJSONVersioned(context.Background(), dir, "last-results.json", map[string]string{"result": "candidate"})
	if err != nil {
		t.Fatal(err)
	}
	newer, err := SaveJSONVersioned(context.Background(), dir, "last-results.json", map[string]string{"result": "manual"})
	if err != nil {
		t.Fatal(err)
	}
	if deleted, err := DeleteFileIfVersion(context.Background(), dir, "last-results.json", candidate); err != nil {
		t.Fatal(err)
	} else if deleted {
		t.Fatal("stale candidate delete unexpectedly removed newer result")
	}
	if deleted, err := DeleteIfVersion(context.Background(), dir, "last-results.json", newer); err != nil {
		t.Fatal(err)
	} else if !deleted {
		t.Fatal("exact current result was not deleted")
	}
	if _, err := os.Stat(filepath.Join(dir, "last-results.json")); !os.IsNotExist(err) {
		t.Fatalf("last-results.json remains after exact delete: %v", err)
	}
}

func TestVersionedSaveChecksCancellationAfterLockAcquisition(t *testing.T) {
	dir := t.TempDir()
	baseline, err := SaveJSONVersioned(context.Background(), dir, "last-results.json", map[string]string{"result": "old"})
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	previousHook := afterLockAcquiredHook
	afterLockAcquiredHook = func() {
		close(entered)
		<-release
	}
	t.Cleanup(func() { afterLockAcquiredHook = previousHook })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type result struct {
		version   FileVersion
		committed bool
		err       error
	}
	done := make(chan result, 1)
	go func() {
		version, committed, saveErr := SaveJSONIfVersion(ctx, dir, "last-results.json", map[string]string{"result": "canceled"}, baseline)
		done <- result{version: version, committed: committed, err: saveErr}
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("versioned save did not acquire the shared lock")
	}
	cancel()
	close(release)
	got := <-done
	if !errors.Is(got.err, context.Canceled) || got.committed || got.version.Exists() {
		t.Fatalf("canceled save result=%+v, want no publication", got)
	}
	current, err := CaptureFileVersion(filepath.Join(dir, "last-results.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !current.Equal(baseline) {
		t.Fatal("canceled save changed the result after acquiring the lock")
	}
}

func TestVersionedSaveReportsVisibleCandidateAfterDirectoryFsyncFault(t *testing.T) {
	dir := t.TempDir()
	baseline, err := SaveJSONVersioned(context.Background(), dir, "last-results.json", map[string]string{"result": "old"})
	if err != nil {
		t.Fatal(err)
	}
	previousSync := atomicSyncDir
	fail := true
	atomicSyncDir = func(path string) error {
		if fail {
			fail = false
			return errors.New("injected directory fsync failure")
		}
		return previousSync(path)
	}
	t.Cleanup(func() { atomicSyncDir = previousSync })
	candidate, committed, err := SaveJSONIfVersion(context.Background(), dir, "last-results.json", map[string]string{"result": "failed"}, baseline)
	if err == nil || !committed || !candidate.Exists() {
		t.Fatalf("fsync fault result candidate=%+v committed=%t err=%v", candidate, committed, err)
	}
	visible, err := CaptureFileVersion(filepath.Join(dir, "last-results.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !visible.Equal(candidate) {
		t.Fatal("published candidate was not reported exactly after fsync fault")
	}
	// Once the fault seam is restored, exact-version compensation can safely
	// remove the visible failed candidate.
	atomicSyncDir = previousSync
	if deleted, err := DeleteFileIfVersion(context.Background(), dir, "last-results.json", candidate); err != nil {
		t.Fatal(err)
	} else if !deleted {
		t.Fatal("visible candidate was not deleted during compensation")
	}
}

func TestVersionedSaveDoesNotPublishAfterRenameFault(t *testing.T) {
	dir := t.TempDir()
	baseline, err := SaveJSONVersioned(context.Background(), dir, "last-results.json", map[string]string{"result": "old"})
	if err != nil {
		t.Fatal(err)
	}
	previousRename := atomicRename
	atomicRename = func(string, string) error { return errors.New("injected rename failure") }
	t.Cleanup(func() { atomicRename = previousRename })
	candidate, committed, err := SaveJSONIfVersion(context.Background(), dir, "last-results.json", map[string]string{"result": "candidate"}, baseline)
	if err == nil || committed || candidate.Exists() {
		t.Fatalf("rename fault result candidate=%+v committed=%t err=%v", candidate, committed, err)
	}
	current, err := CaptureFileVersion(filepath.Join(dir, "last-results.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !current.Equal(baseline) {
		t.Fatal("rename fault changed the prior result")
	}
}

func TestSnapshotRestoresMissingAndExistingCurrentState(t *testing.T) {
	dir := t.TempDir()
	if err := SaveCurrent(dir, Current{Name: "old", Link: "old-link", Mbps: 100}); err != nil {
		t.Fatal(err)
	}
	snap, err := Capture(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveCurrent(dir, Current{Name: "new", Link: "new-link", Mbps: 120}); err != nil {
		t.Fatal(err)
	}
	if err := snap.Restore(dir); err != nil {
		t.Fatal(err)
	}
	got, err := LoadCurrent(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "old" || got.Link != "old-link" || got.Mbps != 100 {
		t.Fatalf("restored current=%+v", got)
	}

	missingDir := filepath.Join(dir, "missing")
	missing, err := Capture(missingDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveCurrent(missingDir, Current{Name: "temporary", Link: "temporary", Mbps: 1}); err != nil {
		t.Fatal(err)
	}
	if err := missing.Restore(missingDir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(missingDir, "current-node.json")); !os.IsNotExist(err) {
		t.Fatalf("current-node.json remains after missing snapshot restore: %v", err)
	}
}

func TestLegacySnapshotsMigrateOutOfRuntimeBackupDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := SaveCurrent(dir, Current{Name: "old", Link: "opaque-link", Mbps: 10}); err != nil {
		t.Fatal(err)
	}
	snap, err := Capture(dir)
	if err != nil {
		t.Fatal(err)
	}
	runtime := filepath.Join(dir, "backups", "sing-box-20240101-000000000.json")
	if err := os.MkdirAll(filepath.Dir(runtime), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(runtime, []byte(`{"outbounds":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Base(runtime) + ".state.json"
	if err := SaveJSON(filepath.Dir(runtime), legacy, snap.persisted()); err != nil {
		t.Fatal(err)
	}

	paired, err := PairedRuntimeBackups(dir, "sing-box-")
	if err != nil {
		t.Fatal(err)
	}
	if len(paired) != 1 || paired[0] != runtime {
		t.Fatalf("paired backups=%v, want exact runtime backup", paired)
	}
	if _, err := os.Stat(filepath.Join(dir, "backups", legacy)); !os.IsNotExist(err) {
		t.Fatalf("legacy sidecar remains in runtime backup directory: %v", err)
	}
	migrated := filepath.Join(StateSnapshotDir(dir), legacy)
	if _, err := os.Stat(migrated); err != nil {
		t.Fatalf("migrated sidecar missing: %v", err)
	}
	files, err := RuntimeBackupFiles(dir, "sing-box-")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0] != runtime {
		t.Fatalf("runtime glob/filter returned %v", files)
	}
}

func TestPairedRuntimeBackupsIgnoreNewestUnpairedRuntimeFile(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"sing-box-20240101-000000000.json", "sing-box-99999999-999999999.json"} {
		path := filepath.Join(dir, "backups", name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(name), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := SaveSnapshotForBackup(dir, filepath.Join(dir, "backups", "sing-box-20240101-000000000.json"), Snapshot{}); err != nil {
		t.Fatal(err)
	}
	paired, err := PairedRuntimeBackups(dir, "sing-box-")
	if err != nil {
		t.Fatal(err)
	}
	if len(paired) != 1 || filepath.Base(paired[0]) != "sing-box-20240101-000000000.json" {
		t.Fatalf("paired backups=%v, want only the exact paired basename", paired)
	}
}

func TestCrossProcessStateDirLockContract(t *testing.T) {
	if os.Getenv("VIBE_VPN_STATE_LOCK_HELPER") == "1" {
		dir := os.Getenv("VIBE_VPN_STATE_LOCK_DIR")
		wantBlocked := os.Getenv("VIBE_VPN_STATE_LOCK_EXPECT") == "blocked"
		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		defer cancel()
		lock, err := AcquireLock(ctx, dir)
		if wantBlocked {
			if lock != nil {
				_ = lock.Close()
				t.Fatal("child acquired a lock held by the parent")
			}
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("blocked child error=%v, want context deadline", err)
			}
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		if err := lock.Close(); err != nil {
			t.Fatal(err)
		}
		return
	}

	dir := t.TempDir()
	lock, err := AcquireLock(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	runHelper := func(expect string) error {
		cmd := exec.Command(os.Args[0], "-test.run=TestCrossProcessStateDirLockContract$", "-test.v")
		cmd.Env = append(os.Environ(),
			"VIBE_VPN_STATE_LOCK_HELPER=1",
			"VIBE_VPN_STATE_LOCK_DIR="+dir,
			"VIBE_VPN_STATE_LOCK_EXPECT="+expect,
		)
		return cmd.Run()
	}
	if err := runHelper("blocked"); err != nil {
		_ = lock.Close()
		t.Fatalf("cross-process blocked contract failed: %v", err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	if err := runHelper("released"); err != nil {
		t.Fatalf("cross-process release contract failed: %v", err)
	}
}

func TestTransactionJournalDurableLayoutAndExactReload(t *testing.T) {
	dir := t.TempDir()
	oldRuntime := []byte("old-runtime-secret-bytes")
	candidateRuntime := []byte("candidate-runtime-secret-bytes")
	oldState := Snapshot{CurrentNode: fileSnapshot{Exists: true, Data: []byte(`{"name":"old"}`), Mode: 0600}}
	candidateState := Snapshot{CurrentLink: fileSnapshot{Exists: true, Data: []byte("candidate-link\n"), Mode: 0600}}
	if err := BeginTransaction(dir, "tx-1", oldRuntime, candidateRuntime, oldState, candidateState); err != nil {
		t.Fatal(err)
	}
	tx, err := LoadTransaction(dir, "tx-1")
	if err != nil {
		t.Fatal(err)
	}
	if tx.Phase != PhasePrepared || !bytes.Equal(tx.OldRuntime, oldRuntime) || !bytes.Equal(tx.CandidateRuntime, candidateRuntime) || !tx.OldState.Equal(oldState) || !tx.CandidateState.Equal(candidateState) {
		t.Fatalf("reloaded transaction does not preserve exact artifacts: phase=%q", tx.Phase)
	}
	for _, name := range []string{journalFile, oldRuntimeFile, candidateRuntimeFile, oldStateFile, candidateStateFile} {
		path := filepath.Join(TransactionsDir(dir), "tx-1", name)
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0600 {
			t.Fatalf("%s mode=%o, want 600", name, info.Mode().Perm())
		}
	}
}

func TestTransactionPhasePersistenceAndCompletion(t *testing.T) {
	dir := t.TempDir()
	if err := BeginTransaction(dir, "phase-test", []byte("old"), []byte("new"), Snapshot{}, Snapshot{}); err != nil {
		t.Fatal(err)
	}
	if err := UpdateTransactionPhase(dir, "phase-test", PhaseRuntimeAcknowledged); err != nil {
		t.Fatal(err)
	}
	if err := UpdateTransactionPhase(dir, "phase-test", PhaseStateCommitted); err != nil {
		t.Fatal(err)
	}
	pending, err := PendingTransactions(dir)
	if err != nil || len(pending) != 1 || pending[0].Phase != PhaseStateCommitted {
		t.Fatalf("pending=%v err=%v, want committed journal", pending, err)
	}
	if err := CompleteTransaction(dir, "phase-test"); err != nil {
		t.Fatal(err)
	}
	pending, err = PendingTransactions(dir)
	if err != nil || len(pending) != 0 {
		t.Fatalf("pending=%v err=%v, want no transactions after completion", pending, err)
	}
	if _, err := os.Stat(filepath.Join(TransactionsDir(dir), "phase-test")); !os.IsNotExist(err) {
		t.Fatalf("completed transaction remains: %v", err)
	}
	if err := UpdateTransactionPhase(dir, "missing", PhaseRuntimeAcknowledged); !os.IsNotExist(err) {
		t.Fatalf("missing journal error=%v, want not-exist", err)
	}
}

func TestPruneOrphanTransactionArtifactsIsBounded(t *testing.T) {
	dir := t.TempDir()
	root := TransactionsDir(dir)
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "stray.tmp"), []byte("payload"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "bad/id"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "orphan"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := BeginTransaction(dir, "keep-me", []byte("old"), []byte("new"), Snapshot{}, Snapshot{}); err != nil {
		t.Fatal(err)
	}
	if err := PruneOrphanTransactionArtifacts(dir); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(root, "stray.tmp"), filepath.Join(root, "bad"), filepath.Join(root, "orphan")} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("orphan artifact remains at %s: %v", path, err)
		}
	}
	if _, err := LoadTransaction(dir, "keep-me"); err != nil {
		t.Fatalf("valid transaction was pruned: %v", err)
	}
}

func TestTransactionIDValidation(t *testing.T) {
	for _, id := range []string{"", ".", "..", "../escape", "/absolute", "bad id"} {
		if err := BeginTransaction(t.TempDir(), id, []byte("a"), []byte("b"), Snapshot{}, Snapshot{}); err == nil {
			t.Fatalf("unsafe transaction ID %q accepted", id)
		}
	}
}
