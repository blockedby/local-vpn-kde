//go:build linux

package nativegateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func testFileJournal(t *testing.T) (*FileJournal, string) {
	t.Helper()
	directory := secureTempDir(t)
	journal, err := newFileJournalForTest(context.Background(), directory, os.Geteuid())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = journal.Close() })
	return journal, directory
}

func secureTempDir(t *testing.T) string {
	t.Helper()
	directory := t.TempDir()
	if err := os.Chmod(directory, 0700); err != nil {
		t.Fatal(err)
	}
	return directory
}

func testNetworkJournal() NetworkJournal {
	return NetworkJournal{
		Version:               NetworkJournalVersion,
		BootID:                "00000000-0000-4000-8000-000000000001",
		Uplink:                Uplink{Interface: "eth0", AddressCIDR: "192.0.2.10/24", Gateway: "192.0.2.1"},
		InitialIPv4Forwarding: true,
		EffectCount:           1,
		Effects: [MaxNetworkEffects]JournalEffect{
			{Kind: EffectNamespace, Phase: EffectIntent},
		},
	}
}

func TestFileJournalPersistenceRoundTripAndClear(t *testing.T) {
	journal, directory := testFileJournal(t)
	want := testNetworkJournal()

	if err := journal.Save(context.Background(), want); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(directory, networkJournalFile))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 || !info.Mode().IsRegular() {
		t.Fatalf("journal mode=%o regular=%t", info.Mode().Perm(), info.Mode().IsRegular())
	}

	got, exists, err := journal.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !exists || !reflect.DeepEqual(got, want) {
		t.Fatalf("loaded journal exists=%t got=%+v want=%+v", exists, got, want)
	}

	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := newFileJournalForTest(context.Background(), directory, os.Geteuid())
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	got, exists, err = reopened.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !exists || !reflect.DeepEqual(got, want) {
		t.Fatalf("reopened journal exists=%t got=%+v want=%+v", exists, got, want)
	}

	if err := reopened.Clear(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(directory, networkJournalFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("journal after clear stat error=%v", err)
	}
	if _, exists, err := reopened.Load(context.Background()); err != nil || exists {
		t.Fatalf("load after clear exists=%t err=%v", exists, err)
	}
}

func TestFileJournalDoesNotValidateNetworkSemantics(t *testing.T) {
	journal, _ := testFileJournal(t)
	want := NetworkJournal{}
	if err := journal.Save(context.Background(), want); err != nil {
		t.Fatal(err)
	}
	got, exists, err := journal.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !exists || !reflect.DeepEqual(got, want) {
		t.Fatalf("semantic-only journal exists=%t got=%+v", exists, got)
	}
}

func TestFileJournalRejectsCorruptJSON(t *testing.T) {
	valid, err := json.Marshal(testNetworkJournal())
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string][]byte{
		"malformed":      []byte("{"),
		"truncated":      valid[:len(valid)/2],
		"oversized":      bytes.Repeat([]byte("x"), maxNetworkJournalJSON+1),
		"unknown field":  []byte(`{"Version":1,"unknown":true}`),
		"trailing value": []byte(`{} {}`),
		"null":           []byte(`null`),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			journal, directory := testFileJournal(t)
			if err := os.WriteFile(filepath.Join(directory, networkJournalFile), data, 0600); err != nil {
				t.Fatal(err)
			}
			if _, exists, err := journal.Load(context.Background()); err == nil || exists {
				t.Fatalf("load corrupt JSON exists=%t err=%v", exists, err)
			}
		})
	}
}

func TestFileJournalDoesNotOverwriteUnsafeJournal(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, path string)
	}{
		{
			name: "symlink",
			setup: func(t *testing.T, path string) {
				outside := filepath.Join(t.TempDir(), "outside")
				if err := os.WriteFile(outside, []byte("outside"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, path); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "hardlink",
			setup: func(t *testing.T, path string) {
				if err := os.WriteFile(path, []byte(`{"Version":1}`), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Link(path, path+".link"); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "permissions",
			setup: func(t *testing.T, path string) {
				if err := os.WriteFile(path, []byte(`{"Version":1}`), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(path, 0640); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			journal, directory := testFileJournal(t)
			path := filepath.Join(directory, networkJournalFile)
			test.setup(t, path)
			if _, _, err := journal.Load(context.Background()); err == nil {
				t.Fatal("unsafe journal loaded")
			}
			if err := journal.Save(context.Background(), testNetworkJournal()); err == nil {
				t.Fatal("unsafe journal was overwritten")
			}
			if err := journal.Clear(context.Background()); err == nil {
				t.Fatal("unsafe journal was cleared")
			}
		})
	}
}

func TestFileJournalRejectsUnsafeLock(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, path string)
	}{
		{
			name: "symlink",
			setup: func(t *testing.T, path string) {
				if err := os.Symlink(filepath.Join(t.TempDir(), "target"), path); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "hardlink",
			setup: func(t *testing.T, path string) {
				if err := os.WriteFile(path, nil, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Link(path, path+".link"); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "permissions",
			setup: func(t *testing.T, path string) {
				if err := os.WriteFile(path, nil, 0640); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			directory := secureTempDir(t)
			test.setup(t, filepath.Join(directory, networkJournalLock))
			if journal, err := newFileJournalForTest(context.Background(), directory, os.Geteuid()); err == nil {
				_ = journal.Close()
				t.Fatal("unsafe lock was accepted")
			}
		})
	}
}

func TestFileJournalRejectsUnsafeDirectories(t *testing.T) {
	t.Run("symlink directory", func(t *testing.T) {
		base := t.TempDir()
		realDirectory := t.TempDir()
		link := filepath.Join(base, "link")
		if err := os.Symlink(realDirectory, link); err != nil {
			t.Fatal(err)
		}
		if journal, err := newFileJournalForTest(context.Background(), link, os.Geteuid()); err == nil {
			_ = journal.Close()
			t.Fatal("symlink directory was accepted")
		}
	})

	t.Run("writable ancestor", func(t *testing.T) {
		base := t.TempDir()
		parent := filepath.Join(base, "parent")
		if err := os.Mkdir(parent, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(parent, 0770); err != nil {
			t.Fatal(err)
		}
		directory := filepath.Join(parent, "journal")
		if err := os.Mkdir(directory, 0700); err != nil {
			t.Fatal(err)
		}
		if journal, err := newFileJournalForTest(context.Background(), directory, os.Geteuid()); err == nil {
			_ = journal.Close()
			t.Fatal("writable ancestor was accepted")
		}
	})

	t.Run("writable journal directory", func(t *testing.T) {
		directory := t.TempDir()
		if err := os.Chmod(directory, 0750); err != nil {
			t.Fatal(err)
		}
		if journal, err := newFileJournalForTest(context.Background(), directory, os.Geteuid()); err == nil {
			_ = journal.Close()
			t.Fatal("unsafe journal directory was accepted")
		}
	})
}

func TestFileJournalCancelledLockAndConcurrentExclusion(t *testing.T) {
	directory := secureTempDir(t)
	first, err := newFileJournalForTest(context.Background(), directory, os.Geteuid())
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	if second, err := newFileJournalForTest(ctx, directory, os.Geteuid()); !errors.Is(err, context.DeadlineExceeded) {
		if second != nil {
			_ = second.Close()
		}
		t.Fatalf("cancelled lock journal=%v err=%v", second, err)
	}
	if time.Since(started) > time.Second {
		t.Fatal("cancelled lock wait did not return promptly")
	}

	opened := make(chan *FileJournal, 1)
	openErr := make(chan error, 1)
	go func() {
		second, err := newFileJournalForTest(context.Background(), directory, os.Geteuid())
		if err != nil {
			openErr <- err
			return
		}
		opened <- second
	}()
	select {
	case second := <-opened:
		_ = second.Close()
		t.Fatal("second journal acquired lock while first was open")
	case err := <-openErr:
		t.Fatal(err)
	case <-time.After(40 * time.Millisecond):
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case second := <-opened:
		if err := second.Close(); err != nil {
			t.Fatal(err)
		}
	case err := <-openErr:
		t.Fatal(err)
	case <-time.After(time.Second):
		t.Fatal("second journal did not acquire released lock")
	}
}

func TestFileJournalTemporaryFilesAreCleaned(t *testing.T) {
	journal, directory := testFileJournal(t)
	if err := journal.Save(context.Background(), testNetworkJournal()); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "."+networkJournalFile+".") && strings.HasSuffix(entry.Name(), ".tmp") {
			t.Fatalf("temporary file left behind: %s", entry.Name())
		}
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := journal.Save(cancelled, testNetworkJournal()); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled save error=%v", err)
	}
	entries, err = os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "."+networkJournalFile+".") && strings.HasSuffix(entry.Name(), ".tmp") {
			t.Fatalf("temporary file left after cancelled save: %s", entry.Name())
		}
	}
}

func TestFileJournalCancellationBeforeOperationsAndClose(t *testing.T) {
	journal, _ := testFileJournal(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, exists, err := journal.Load(ctx); !errors.Is(err, context.Canceled) || exists {
		t.Fatalf("cancelled load exists=%t err=%v", exists, err)
	}
	if err := journal.Save(ctx, testNetworkJournal()); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled save error=%v", err)
	}
	if err := journal.Clear(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled clear error=%v", err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := journal.Load(context.Background()); !errors.Is(err, errFileJournalClosed) {
		t.Fatalf("load after close error=%v", err)
	}
}
