//go:build linux

package localvpn

import (
	"bufio"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func newNativeProfileTransactionState(t *testing.T) string {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("unprivileged profile transaction test")
	}
	state := filepath.Join(t.TempDir(), "local-vpn-kde")
	if err := os.Mkdir(state, 0700); err != nil {
		t.Fatal(err)
	}
	return state
}

func TestNativeProfileFileTransactionRollbackAndCommit(t *testing.T) {
	state := newNativeProfileTransactionState(t)
	target := filepath.Join(state, "openvpn/client", nativeClientProfileName)
	first := append(testNativeDeliveryProfile(t), []byte("# old\n")...)
	second := append(testNativeDeliveryProfile(t), []byte("# new\n")...)
	if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, first, 0600); err != nil {
		t.Fatal(err)
	}

	tx, err := beginNativeProfileFileTransaction(state, second, "/", uint32(os.Geteuid()), uint32(os.Getegid()))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(mustReadNativeDeliveryFile(t, target)); got != string(second) {
		t.Fatal("new profile was not staged")
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if got := string(mustReadNativeDeliveryFile(t, target)); got != string(first) {
		t.Fatal("rollback did not restore old profile")
	}

	tx, err = beginNativeProfileFileTransaction(state, second, "/", uint32(os.Geteuid()), uint32(os.Getegid()))
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if got := string(mustReadNativeDeliveryFile(t, target)); got != string(second) {
		t.Fatal("commit did not retain new profile")
	}
	entries, err := os.ReadDir(filepath.Dir(target))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != nativeClientProfileName {
		t.Fatalf("snapshot leaked after commit: %v", entries)
	}
}

func TestNativeProfileFileTransactionRefusesForeignConcurrentChange(t *testing.T) {
	state := newNativeProfileTransactionState(t)
	target := filepath.Join(state, "openvpn/client", nativeClientProfileName)
	if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, append(testNativeDeliveryProfile(t), []byte("# old\n")...), 0600); err != nil {
		t.Fatal(err)
	}
	tx, err := beginNativeProfileFileTransaction(state, append(testNativeDeliveryProfile(t), []byte("# new\n")...), "/", uint32(os.Geteuid()), uint32(os.Getegid()))
	if err != nil {
		t.Fatal(err)
	}
	moved := target + ".moved"
	if err := os.Rename(target, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("foreign"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); err == nil {
		t.Fatal("rollback overwrote a concurrent profile")
	}
	if got := string(mustReadNativeDeliveryFile(t, target)); got != "foreign" {
		t.Fatalf("foreign profile changed: %q", got)
	}
	tx.close()
}

func TestNativeProfileFileTransactionRefusesInPlaceChanges(t *testing.T) {
	for _, action := range []string{"commit", "rollback"} {
		t.Run(action, func(t *testing.T) {
			state := newNativeProfileTransactionState(t)
			target := filepath.Join(state, "openvpn/client", nativeClientProfileName)
			if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(target, append(testNativeDeliveryProfile(t), []byte("# old\n")...), 0600); err != nil {
				t.Fatal(err)
			}
			tx, err := beginNativeProfileFileTransaction(state, append(testNativeDeliveryProfile(t), []byte("# new\n")...), "/", uint32(os.Geteuid()), uint32(os.Getegid()))
			if err != nil {
				t.Fatal(err)
			}
			defer tx.close()
			if err := os.WriteFile(target, []byte("foreign in-place contents"), 0600); err != nil {
				t.Fatal(err)
			}
			if action == "commit" {
				err = tx.Commit()
			} else {
				err = tx.Rollback()
			}
			if err == nil {
				t.Fatalf("%s accepted in-place profile overwrite", action)
			}
			if got := string(mustReadNativeDeliveryFile(t, target)); got != "foreign in-place contents" {
				t.Fatalf("%s changed foreign contents: %q", action, got)
			}
		})
	}

	t.Run("saved old profile", func(t *testing.T) {
		state := newNativeProfileTransactionState(t)
		target := filepath.Join(state, "openvpn/client", nativeClientProfileName)
		if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, append(testNativeDeliveryProfile(t), []byte("# old\n")...), 0600); err != nil {
			t.Fatal(err)
		}
		tx, err := beginNativeProfileFileTransaction(state, append(testNativeDeliveryProfile(t), []byte("# new\n")...), "/", uint32(os.Geteuid()), uint32(os.Getegid()))
		if err != nil {
			t.Fatal(err)
		}
		defer tx.close()
		if err := os.WriteFile(filepath.Join(filepath.Dir(target), tx.oldName), []byte("altered old snapshot"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := tx.Rollback(); err == nil {
			t.Fatal("rollback accepted an in-place overwrite of the saved old profile")
		}
		if got := string(mustReadNativeDeliveryFile(t, target)); !strings.HasSuffix(got, "# new\n") {
			t.Fatal("rollback replaced the staged profile with an altered snapshot")
		}
	})

	t.Run("mode", func(t *testing.T) {
		state := newNativeProfileTransactionState(t)
		tx, err := beginNativeProfileFileTransaction(state, testNativeDeliveryProfile(t), "/", uint32(os.Geteuid()), uint32(os.Getegid()))
		if err != nil {
			t.Fatal(err)
		}
		defer tx.close()
		target := filepath.Join(state, "openvpn/client", nativeClientProfileName)
		if err := os.Chmod(target, 0640); err != nil {
			t.Fatal(err)
		}
		if err := tx.Rollback(); err == nil {
			t.Fatal("rollback accepted changed profile metadata")
		}
	})
}

func TestNativeProfileFileTransactionRefusesReplacedDirectoryHierarchy(t *testing.T) {
	state := newNativeProfileTransactionState(t)
	tx, err := beginNativeProfileFileTransaction(state, testNativeDeliveryProfile(t), "/", uint32(os.Geteuid()), uint32(os.Getegid()))
	if err != nil {
		t.Fatal(err)
	}
	defer tx.close()
	client := filepath.Join(state, "openvpn/client")
	moved := client + ".moved"
	if err := os.Rename(client, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(client, 0700); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); err == nil {
		t.Fatal("rollback accepted a replaced client directory")
	}
	if info, err := os.Stat(client); err != nil || !info.IsDir() {
		t.Fatalf("replacement client directory was removed: info=%v err=%v", info, err)
	}
	if got := string(mustReadNativeDeliveryFile(t, filepath.Join(moved, nativeClientProfileName))); got != string(testNativeDeliveryProfile(t)) {
		t.Fatal("profile in renamed pinned directory was changed")
	}
}

func TestNativeProfileFileTransactionRefusesRenamedStateDirectory(t *testing.T) {
	state := newNativeProfileTransactionState(t)
	tx, err := beginNativeProfileFileTransaction(state, testNativeDeliveryProfile(t), "/", uint32(os.Geteuid()), uint32(os.Getegid()))
	if err != nil {
		t.Fatal(err)
	}
	defer tx.close()
	moved := state + ".moved"
	if err := os.Rename(state, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(state, 0700); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err == nil {
		t.Fatal("commit accepted a renamed state directory")
	}
	if _, err := os.Stat(filepath.Join(state, "openvpn/client", nativeClientProfileName)); !os.IsNotExist(err) {
		t.Fatalf("profile unexpectedly appeared at the canonical state path: %v", err)
	}
}

type nativeProfileReadyWriter struct {
	ready chan struct{}
	sent  bool
}

func (w *nativeProfileReadyWriter) Write(p []byte) (int, error) {
	if !w.sent && string(p) == nativeProfileTransactionReady {
		w.sent = true
		close(w.ready)
	}
	return len(p), nil
}

func TestRunNativeProfileTransactionClosesFDsAfterFailedRollback(t *testing.T) {
	state := newNativeProfileTransactionState(t)
	profile := testNativeDeliveryProfile(t)
	input, inputWriter := io.Pipe()
	output := &nativeProfileReadyWriter{ready: make(chan struct{})}
	result := make(chan error, 1)
	go func() {
		result <- RunNativeProfileTransaction(context.Background(), input, output)
	}()
	if err := writeNativeProfileUint32(inputWriter, uint32(len(state))); err != nil {
		t.Fatal(err)
	}
	if err := writeAll(inputWriter, []byte(state)); err != nil {
		t.Fatal(err)
	}
	if err := writeNativeProfileUint32(inputWriter, uint32(len(profile))); err != nil {
		t.Fatal(err)
	}
	if err := writeAll(inputWriter, profile); err != nil {
		t.Fatal(err)
	}
	<-output.ready
	target := filepath.Join(state, "openvpn/client", nativeClientProfileName)
	if err := os.WriteFile(target, []byte("foreign in-place contents"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := inputWriter.Write([]byte{'R'}); err != nil {
		t.Fatal(err)
	}
	_ = inputWriter.Close()
	if err := <-result; err == nil {
		t.Fatal("transaction unexpectedly rolled back altered staged profile")
	}
	fds, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	for _, fd := range fds {
		link, err := os.Readlink(filepath.Join("/proc/self/fd", fd.Name()))
		if err == nil && strings.HasPrefix(link, state) {
			t.Fatalf("transaction FD %s remained open on error: %s", fd.Name(), link)
		}
	}
}

func TestNativeProfileTransactionHelperProcess(t *testing.T) {
	if os.Getenv("LOCALVPN_PROFILE_TRANSACTION_HELPER") != "1" {
		return
	}
	if err := RunNativeProfileTransaction(context.Background(), os.Stdin, os.Stdout); err != nil {
		os.Exit(2)
	}
	os.Exit(0)
}

func TestNativeProfileTransactionUnprivilegedChildProtocol(t *testing.T) {
	state := newNativeProfileTransactionState(t)
	profile := testNativeDeliveryProfile(t)
	command := exec.Command(os.Args[0], "-test.run=TestNativeProfileTransactionHelperProcess")
	command.Env = append(os.Environ(), "LOCALVPN_PROFILE_TRANSACTION_HELPER=1")
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	command.Stderr = io.Discard
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	if err := writeNativeProfileUint32(stdin, uint32(len(state))); err != nil {
		t.Fatal(err)
	}
	if err := writeAll(stdin, []byte(state)); err != nil {
		t.Fatal(err)
	}
	if err := writeNativeProfileUint32(stdin, uint32(len(profile))); err != nil {
		t.Fatal(err)
	}
	if err := writeAll(stdin, profile); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(stdout)
	if token, err := reader.ReadString('\n'); err != nil || token != nativeProfileTransactionReady {
		t.Fatalf("ready token=%q err=%v", token, err)
	}
	if _, err := stdin.Write([]byte{'C'}); err != nil {
		t.Fatal(err)
	}
	_ = stdin.Close()
	if token, err := reader.ReadString('\n'); err != nil || token != nativeProfileTransactionCommitted {
		t.Fatalf("commit token=%q err=%v", token, err)
	}
	if err := command.Wait(); err != nil {
		t.Fatal(err)
	}
	if got := string(mustReadNativeDeliveryFile(t, filepath.Join(state, "openvpn/client", nativeClientProfileName))); got != string(profile) {
		t.Fatal("child did not commit profile")
	}
}

func TestNativeProfileTransactionSignalCompensates(t *testing.T) {
	state := newNativeProfileTransactionState(t)
	old := append(testNativeDeliveryProfile(t), []byte("# old\n")...)
	target := filepath.Join(state, "openvpn/client", nativeClientProfileName)
	if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, old, 0600); err != nil {
		t.Fatal(err)
	}
	profile := append(testNativeDeliveryProfile(t), []byte("# staged\n")...)
	command := exec.Command(os.Args[0], "-test.run=TestNativeProfileTransactionHelperProcess")
	command.Env = append(os.Environ(), "LOCALVPN_PROFILE_TRANSACTION_HELPER=1")
	stdin, _ := command.StdinPipe()
	stdout, _ := command.StdoutPipe()
	command.Stderr = io.Discard
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	_ = writeNativeProfileUint32(stdin, uint32(len(state)))
	_ = writeAll(stdin, []byte(state))
	_ = writeNativeProfileUint32(stdin, uint32(len(profile)))
	_ = writeAll(stdin, profile)
	reader := bufio.NewReader(stdout)
	if token, err := reader.ReadString('\n'); err != nil || token != nativeProfileTransactionReady {
		t.Fatalf("ready token=%q err=%v", token, err)
	}
	if err := command.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	_ = stdin.Close()
	if err := command.Wait(); err == nil {
		t.Fatal("signal unexpectedly committed")
	}
	if got := string(mustReadNativeDeliveryFile(t, target)); got != string(old) {
		t.Fatal("signal did not restore old profile")
	}
}

func TestNativeProfileTransactionEOFCompensates(t *testing.T) {
	state := newNativeProfileTransactionState(t)
	old := append(testNativeDeliveryProfile(t), []byte("# old\n")...)
	target := filepath.Join(state, "openvpn/client", nativeClientProfileName)
	if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, old, 0600); err != nil {
		t.Fatal(err)
	}
	profile := append(testNativeDeliveryProfile(t), []byte("# staged\n")...)
	command := exec.Command(os.Args[0], "-test.run=TestNativeProfileTransactionHelperProcess")
	command.Env = append(os.Environ(), "LOCALVPN_PROFILE_TRANSACTION_HELPER=1")
	stdin, _ := command.StdinPipe()
	stdout, _ := command.StdoutPipe()
	command.Stderr = io.Discard
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	_ = writeNativeProfileUint32(stdin, uint32(len(state)))
	_ = writeAll(stdin, []byte(state))
	_ = writeNativeProfileUint32(stdin, uint32(len(profile)))
	_ = writeAll(stdin, profile)
	reader := bufio.NewReader(stdout)
	if token, err := reader.ReadString('\n'); err != nil || token != nativeProfileTransactionReady {
		t.Fatalf("ready token=%q err=%v", token, err)
	}
	_ = stdin.Close()
	if err := command.Wait(); err == nil {
		t.Fatal("EOF unexpectedly committed")
	}
	if got := string(mustReadNativeDeliveryFile(t, target)); got != string(old) {
		t.Fatalf("EOF did not restore profile; got suffix %q", got[len(got)-min(16, len(got)):])
	}
	entries, err := os.ReadDir(filepath.Dir(target))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".vpnkit-profile-") {
			t.Fatalf("snapshot leaked: %s", entry.Name())
		}
	}
}
