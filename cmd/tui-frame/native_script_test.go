package main

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const bindingModule = "go.mitchellh.com/libghostty"

var (
	// scriptRevision matches the Ghostty revision that scripts/native.sh
	// downloads, builds, and records in .frame-source-revision.
	scriptRevision = regexp.MustCompile(`(?m)^revision='([0-9a-f]{40})'$`)
	// bindingRevision matches the Ghostty revision that the binding's CMake
	// build fetches; the binding's cgo code is written against that revision.
	bindingRevision = regexp.MustCompile(`(?s)FetchContent_Declare\(ghostty\b[^)]*\bGIT_TAG\s+([0-9a-f]{40})\b`)
)

// scripts/native.sh is the native build that this repository and its
// consumers run, and it pins the Ghostty revision by hand. The binding's
// headers and cgo calls belong to the Ghostty revision its own CMake build
// fetches, at the binding version go.mod selects. A go.mod bump of the binding
// that leaves the script's pin behind must fail here, not in cgo or at runtime
// against a native library the binding was never built for.
func TestNativeScriptBuildsBindingRevision(t *testing.T) {
	t.Parallel()
	script, err := os.ReadFile("../../scripts/native.sh")
	if err != nil {
		t.Fatal(err)
	}
	pinned := scriptRevision.FindSubmatch(script)
	if pinned == nil {
		t.Fatalf("scripts/native.sh assigns no 40-digit revision='...' line:\n%s", script)
	}

	out, err := exec.Command("go", "list", "-m", "-f", "{{.Version}} {{.Dir}}", bindingModule).Output()
	if err != nil {
		t.Fatalf("go list -m %s: %v", bindingModule, err)
	}
	version, dir, ok := strings.Cut(strings.TrimSpace(string(out)), " ")
	if !ok || dir == "" {
		t.Fatalf("go list -m %s reports no module directory: %q", bindingModule, out)
	}
	cmake, err := os.ReadFile(filepath.Join(dir, "CMakeLists.txt"))
	if err != nil {
		t.Fatal(err)
	}
	fetched := bindingRevision.FindSubmatch(cmake)
	if fetched == nil {
		t.Fatalf("%s@%s CMakeLists.txt declares no ghostty GIT_TAG:\n%s", bindingModule, version, cmake)
	}

	if string(pinned[1]) != string(fetched[1]) {
		t.Errorf("scripts/native.sh builds Ghostty %s, but go.mod selects %s@%s, which is built against Ghostty %s", pinned[1], bindingModule, version, fetched[1])
	}
}

// Consumers run scripts/native.sh from a read-only module cache, so it must
// write only under the root it is given. It refuses, before creating
// anything, a root that is not absolute and installation or cache names that
// leave the root.
func TestNativeScriptRefusesPathsOutsideRoot(t *testing.T) {
	t.Parallel()
	script, err := filepath.Abs("../../scripts/native.sh")
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(repositoryTempDir(t), "native")
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"missing arguments", []string{root, "native", "prefix"}, "Expected four arguments."},
		{"relative root", []string{"native", "native", "prefix", "zig-cache"}, "ROOT must be an absolute path: native"},
		{"empty target", []string{root, "", "prefix", "zig-cache"}, "TARGET must not be empty."},
		{"absolute prefix", []string{root, "native", root + "-prefix", "zig-cache"}, "PREFIX and CACHE must be relative paths inside ROOT: " + root + "-prefix"},
		{"prefix above root", []string{root, "native", "../prefix", "zig-cache"}, "PREFIX and CACHE must be relative paths inside ROOT: ../prefix"},
		{"cache leaving root", []string{root, "native", "prefix", "cache/../../zig-cache"}, "PREFIX and CACHE must be relative paths inside ROOT: cache/../../zig-cache"},
		{"empty cache", []string{root, "native", "prefix", ""}, "PREFIX and CACHE must be relative paths inside ROOT: "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := exec.Command("bash", append([]string{script}, tt.args...)...)
			cmd.Dir = filepath.Dir(root)
			out, err := cmd.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 2 {
				t.Fatalf("bash scripts/native.sh %q: %v, want exit status 2\n%s", tt.args, err, out)
			}
			if lines := strings.Split(strings.TrimSpace(string(out)), "\n"); lines[len(lines)-1] != strings.TrimSpace(tt.want) {
				t.Errorf("bash scripts/native.sh %q reported:\n%s\nwant last line %q", tt.args, out, tt.want)
			}
			if _, err := os.Lstat(root); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("bash scripts/native.sh %q created %s or failed to stat it: %v", tt.args, root, err)
			}
		})
	}
}
