package localvpn

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"

	"golang.org/x/sys/unix"
)

func renderFixture(t *testing.T) RenderOptions {
	t.Helper()
	template, err := filepath.Abs("../../config/sing-box/config.tun.json.template")
	if err != nil {
		t.Fatal(err)
	}
	return RenderOptions{Template: template, Base: t.TempDir(), Policy: "smart", RuleSets: "local-fixture", Outbound: "direct-fixture", Fixture: true, AllowMissingSubscription: true}
}

// During migration the old renderer is an independent compatibility oracle.
func TestRenderPythonParity(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("Python migration oracle unavailable")
	}
	for _, policy := range []string{"smart", "strict"} {
		for _, mode := range []string{"remote", "local-fixture"} {
			for _, outbound := range []string{"subscription", "direct-fixture"} {
				if mode == "remote" && outbound == "direct-fixture" {
					continue
				}
				t.Run(policy+"/"+mode+"/"+outbound, func(t *testing.T) {
					native := renderFixture(t)
					native.Policy = policy
					native.RuleSets = mode
					native.Outbound = outbound
					legacy := t.TempDir()
					for _, base := range []string{native.Base, legacy} {
						if err := os.MkdirAll(filepath.Join(base, "vibe-vpn"), 0700); err != nil {
							t.Fatal(err)
						}
						if err := os.WriteFile(filepath.Join(base, "vibe-vpn/sub_url"), []byte("https://subscription.example.invalid/fixture\n"), 0600); err != nil {
							t.Fatal(err)
						}
					}
					if err := Render(native); err != nil {
						t.Fatal(err)
					}
					cmd := exec.Command(python, "../../test/fixtures/legacy-renderer.py", native.Template, legacy, policy, mode, outbound, "true")
					cmd.Env = append(os.Environ(), "VPNKIT_LOCAL_TEST_FIXTURE=1", "VPNKIT_LOCAL_RENDER_RACE_HOOK=")
					if output, err := cmd.CombinedOutput(); err != nil {
						t.Fatalf("legacy renderer: %v %s", err, output)
					}
					files := 0
					err := filepath.WalkDir(filepath.Join(legacy, "rendered"), func(path string, entry os.DirEntry, err error) error {
						if err != nil {
							return err
						}
						relative, _ := filepath.Rel(legacy, path)
						other := filepath.Join(native.Base, relative)
						stat, err := os.Stat(other)
						if err != nil {
							return err
						}
						wantMode := os.FileMode(0600)
						if entry.IsDir() {
							wantMode = 0700
						}
						if stat.Mode().Perm() != wantMode {
							t.Errorf("%s mode %o", relative, stat.Mode().Perm())
						}
						if entry.IsDir() {
							return nil
						}
						files++
						a, err := os.ReadFile(path)
						if err != nil {
							return err
						}
						b, err := os.ReadFile(other)
						if err != nil {
							return err
						}
						if filepath.Ext(path) == ".json" {
							var av, bv any
							if err = json.Unmarshal(a, &av); err != nil {
								return err
							}
							if err = json.Unmarshal(b, &bv); err != nil {
								return err
							}
							if !reflect.DeepEqual(av, bv) {
								t.Errorf("JSON parity mismatch: %s", relative)
							}
						} else if !bytes.Equal(a, b) {
							t.Errorf("byte parity mismatch: %s", relative)
						}
						return nil
					})
					if err != nil {
						t.Fatal(err)
					}
					if files < 4 {
						t.Fatalf("too few outputs: %d", files)
					}
				})
			}
		}
	}
}

func TestRenderDirectorySwapCannotEscape(t *testing.T) {
	o := renderFixture(t)
	outside := t.TempDir()
	sentinel := filepath.Join(outside, "config.json")
	if err := os.WriteFile(sentinel, []byte("do not touch"), 0644); err != nil {
		t.Fatal(err)
	}
	o.afterRead = func() error {
		original := filepath.Join(o.Base, "rendered/sing-box")
		held := original + "-held"
		if err := os.Rename(original, held); err != nil {
			return err
		}
		if err := os.Symlink(outside, original); err != nil {
			return err
		}
		return os.Link(sentinel, filepath.Join(held, "config.json"))
	}
	if err := Render(o); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(sentinel)
	if err != nil || string(data) != "do not touch" {
		t.Fatalf("external content changed: %v", err)
	}
	st, err := os.Stat(sentinel)
	if err != nil || st.Mode().Perm() != 0644 {
		t.Fatalf("external mode changed: %v", err)
	}
	if _, err = os.Stat(filepath.Join(o.Base, "rendered/sing-box-held/config.json")); err != nil {
		t.Fatal(err)
	}
	stages, _ := filepath.Glob(filepath.Join(o.Base, "rendered/sing-box-held/.vpnkit-*"))
	if len(stages) != 0 {
		t.Fatal("staging files leaked")
	}
}

func TestRenderRejectsUnsafeSources(t *testing.T) {
	for _, kind := range []string{"symlink", "hardlink", "fifo"} {
		t.Run(kind, func(t *testing.T) {
			o := renderFixture(t)
			source := filepath.Join(o.Base, "vibe-vpn")
			if err := os.Mkdir(source, 0700); err != nil {
				t.Fatal(err)
			}
			outside := filepath.Join(t.TempDir(), "secret")
			if err := os.WriteFile(outside, []byte("private"), 0600); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(source, "sub_url")
			var err error
			switch kind {
			case "symlink":
				err = os.Symlink(outside, target)
			case "hardlink":
				err = os.Link(outside, target)
			case "fifo":
				err = unix.Mkfifo(target, 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err = Render(o); err == nil {
				t.Fatal("unsafe source accepted")
			}
			if _, err = os.Stat(filepath.Join(o.Base, "rendered/sing-box/config.json")); !os.IsNotExist(err) {
				t.Fatal("rendered after invalid source")
			}
		})
	}
}

func TestRenderRejectsInvalidOptions(t *testing.T) {
	for _, change := range []func(*RenderOptions){func(o *RenderOptions) { o.Base += "/../escape" }, func(o *RenderOptions) { o.Template = "/tmp/../template" }, func(o *RenderOptions) { o.Policy = "invalid" }, func(o *RenderOptions) { o.Fixture = false }, func(o *RenderOptions) { o.RuleSets = "invalid" }, func(o *RenderOptions) { o.Outbound = "direct" }} {
		o := renderFixture(t)
		change(&o)
		if err := Render(o); err == nil {
			t.Fatal("invalid options accepted")
		}
	}
}

func TestRenderTreePreflightRejectsUnrelatedLinks(t *testing.T) {
	for _, kind := range []string{"symlink", "hardlink"} {
		t.Run(kind, func(t *testing.T) {
			o := renderFixture(t)
			outside := filepath.Join(t.TempDir(), "sentinel")
			if err := os.WriteFile(outside, []byte("unchanged"), 0644); err != nil {
				t.Fatal(err)
			}
			link := os.Link
			if kind == "symlink" {
				link = os.Symlink
			}
			if err := link(outside, filepath.Join(o.Base, "unrelated")); err != nil {
				t.Fatal(err)
			}
			if err := Render(o); err == nil {
				t.Fatal("unsafe unrelated tree entry accepted")
			}
			entries, err := os.ReadDir(o.Base)
			if err != nil || len(entries) != 1 {
				t.Fatal("preflight rejection created output directories")
			}
			info, err := os.Stat(outside)
			if err != nil || info.Mode().Perm() != 0644 {
				t.Fatal("preflight rejection changed external permissions")
			}
		})
	}
}
