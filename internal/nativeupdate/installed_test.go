package nativeupdate

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
)

type fakeResult struct {
	output string
	err    error
}

func fakeRunner(t *testing.T, results map[string]fakeResult) Runner {
	t.Helper()
	return func(_ context.Context, name string, args ...string) ([]byte, error) {
		t.Helper()
		key := strings.Join(append([]string{name}, args...), " ")
		result, ok := results[key]
		if !ok {
			t.Errorf("unexpected command: %s", key)
			return nil, errors.New("unexpected command")
		}
		return []byte(result.output), result.err
	}
}

func TestDetectInstallation(t *testing.T) {
	pacOwner := "/usr/bin/pacman -Qqo " + OwnedFile
	pacVersion := "/usr/bin/pacman -Q " + PackageName
	dpkgOwner := "/usr/bin/dpkg-query -S " + OwnedFile
	dpkgVersion := "/usr/bin/dpkg-query -W -f=${Version} " + PackageName
	missing := errors.New("not owned")
	tests := []struct {
		name     string
		existing []string
		outputs  map[string]fakeResult
		want     Installation
		wantErr  error
		errText  string
	}{
		{"pacman", []string{"/usr/bin/pacman"}, map[string]fakeResult{pacOwner: {output: PackageName + "\n"}, pacVersion: {output: PackageName + " 0.4.3-1\n"}}, Installation{Manager: "pacman", Raw: "0.4.3-1", Minor: 4, Patch: 3, Pkgrel: 1}, nil, ""},
		{"dpkg", []string{"/usr/bin/dpkg-query"}, map[string]fakeResult{dpkgOwner: {output: PackageName + ": " + OwnedFile + "\n"}, dpkgVersion: {output: "0.4.3"}}, Installation{Manager: "dpkg", Raw: "0.4.3", Minor: 4, Patch: 3}, nil, ""},
		{"ambiguous", []string{"/usr/bin/pacman", "/usr/bin/dpkg-query"}, map[string]fakeResult{pacOwner: {output: PackageName}, pacVersion: {output: PackageName + " 0.4.3-1"}, dpkgOwner: {output: PackageName + ": " + OwnedFile}, dpkgVersion: {output: "0.4.3"}}, Installation{}, nil, "ambiguous package ownership"},
		{"neither", []string{"/usr/bin/pacman", "/usr/bin/dpkg-query"}, map[string]fakeResult{pacOwner: {err: missing}, dpkgOwner: {err: missing}}, Installation{}, ErrNotPackaged, ""},
		{"other package", []string{"/usr/bin/pacman"}, map[string]fakeResult{pacOwner: {output: "other-package\n"}}, Installation{}, ErrNotPackaged, ""},
		{"malformed ownership", []string{"/usr/bin/pacman"}, map[string]fakeResult{pacOwner: {output: PackageName + "\nother"}}, Installation{}, nil, "unexpected pacman ownership"},
		{"malformed dpkg ownership", []string{"/usr/bin/dpkg-query"}, map[string]fakeResult{dpkgOwner: {output: PackageName + ": /wrong/path"}}, Installation{}, nil, "unexpected dpkg ownership"},
		{"malformed version output", []string{"/usr/bin/pacman"}, map[string]fakeResult{pacOwner: {output: PackageName}, pacVersion: {output: PackageName + " 0.4.3-1 extra"}}, Installation{}, nil, "unexpected pacman version"},
		{"version query fails", []string{"/usr/bin/pacman"}, map[string]fakeResult{pacOwner: {output: PackageName}, pacVersion: {err: missing}}, Installation{}, ErrNotPackaged, ""},
		{"snapshot", []string{"/usr/bin/pacman"}, map[string]fakeResult{pacOwner: {output: PackageName}, pacVersion: {output: PackageName + " 0.0.0.r278.gabc.snapshot.x-1"}}, Installation{}, ErrDevelopmentBuild, ""},
		{"epoch", []string{"/usr/bin/dpkg-query"}, map[string]fakeResult{dpkgOwner: {output: PackageName + ": " + OwnedFile}, dpkgVersion: {output: "1:0.4.3"}}, Installation{}, ErrDevelopmentBuild, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			exists := func(path string) bool {
				for _, p := range tc.existing {
					if p == path {
						return true
					}
				}
				return false
			}
			got, err := DetectInstallation(context.Background(), fakeRunner(t, tc.outputs), exists)
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("error = %v, want %v", err, tc.wantErr)
			}
			if tc.errText != "" && (err == nil || !strings.Contains(err.Error(), tc.errText)) {
				t.Fatalf("error = %v, want text %q", err, tc.errText)
			}
			if tc.wantErr == nil && tc.errText == "" && err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestDetectRejectsNonStableVersions(t *testing.T) {
	for _, tc := range []struct{ manager, version string }{
		{"pacman", "01.2.3-1"}, {"pacman", "1.02.3-1"}, {"pacman", "1.2.03-1"},
		{"pacman", "1.2.3-01"}, {"pacman", "1.2.3-0"}, {"pacman", "1.2.3"},
		{"pacman", "1.2.3-1-1"}, {"pacman", "1:1.2.3-1"},
		{"dpkg", "01.2.3"}, {"dpkg", "1.2.3~rc1"}, {"dpkg", "1.2.3+build"},
	} {
		t.Run(tc.manager+"/"+tc.version, func(t *testing.T) {
			results := map[string]fakeResult{}
			path := "/usr/bin/" + tc.manager
			if tc.manager == "pacman" {
				results[path+" -Qqo "+OwnedFile] = fakeResult{output: PackageName}
				results[path+" -Q "+PackageName] = fakeResult{output: PackageName + " " + tc.version}
			} else {
				path = "/usr/bin/dpkg-query"
				results[path+" -S "+OwnedFile] = fakeResult{output: PackageName + ": " + OwnedFile}
				results[path+" -W -f=${Version} "+PackageName] = fakeResult{output: tc.version}
			}
			_, err := DetectInstallation(context.Background(), fakeRunner(t, results), func(p string) bool { return p == path })
			if !errors.Is(err, ErrDevelopmentBuild) || !strings.Contains(err.Error(), tc.version) {
				t.Fatalf("error = %v, want ErrDevelopmentBuild with raw version", err)
			}
		})
	}
}

func TestOlder(t *testing.T) {
	tests := []struct {
		installed Installation
		target    [3]int
		want      bool
	}{
		{Installation{Major: 1, Minor: 2, Patch: 3}, [3]int{2, 0, 0}, true},
		{Installation{Major: 1, Minor: 2, Patch: 3}, [3]int{1, 3, 0}, true},
		{Installation{Major: 1, Minor: 2, Patch: 3}, [3]int{1, 2, 4}, true},
		{Installation{Major: 1, Minor: 2, Patch: 3}, [3]int{1, 2, 3}, false},
		{Installation{Major: 1, Minor: 2, Patch: 3, Pkgrel: 1}, [3]int{1, 2, 3}, false},
		{Installation{Major: 2}, [3]int{1, 9, 9}, false},
	}
	for _, tc := range tests {
		if got := tc.installed.Older(tc.target[0], tc.target[1], tc.target[2]); got != tc.want {
			t.Errorf("%+v.Older(%v) = %v, want %v", tc.installed, tc.target, got, tc.want)
		}
	}
}

func TestExecRunnerEnvironment(t *testing.T) {
	if _, err := os.Stat("/usr/bin/env"); os.IsNotExist(err) {
		t.Skip("/usr/bin/env is unavailable")
	}
	t.Setenv("NATIVEUPDATE_TEST_EXTRA", "not inherited")
	out, err := ExecRunner(context.Background(), "/usr/bin/env")
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
	want := []string{"LC_ALL=C", "PATH=/usr/bin:/bin"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("environment = %q, want %q", got, want)
	}
}
