package scripts

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestBuildScratchLifetime(t *testing.T) {
	data, err := os.ReadFile("with-build-scratch")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "scripts"), 0700); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(root, "scripts/with-build-scratch")
	if err := os.WriteFile(script, data, 0700); err != nil {
		t.Fatal(err)
	}
	for _, status := range []int{0, 7, 143} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			t.Parallel()
			marker := filepath.Join(t.TempDir(), "finished")
			// A signal sent only to the wrapper must not remove scratch while
			// its foreground command still uses it. Actual commands and files
			// exercise success, failure, and deferred signal cleanup concurrently.
			cmd := exec.Command("bash", script, "bash", "-c", `set -eu
test "$TMPDIR" = "$GOTMPDIR"
test -d "$TMPDIR"
printf '%s\n' "$TMPDIR"
test "$2" = 'argument with spaces'
if test "$1" = 143; then kill -TERM "$PPID"; fi
sleep 0.05
printf retained > "$TMPDIR/still-owned"
printf finished > "$3"
if test "$1" = 143; then exit 0; fi
exit "$1"`, "probe", strconv.Itoa(status), "argument with spaces", marker)
			output, err := cmd.CombinedOutput()
			var exit *exec.ExitError
			if status == 0 && err != nil || status != 0 && (!errors.As(err, &exit) || exit.ExitCode() != status) {
				t.Fatalf("status %d: %v: %s", status, err, output)
			}
			path := strings.TrimSpace(string(output))
			if filepath.Dir(path) != filepath.Join(root, ".tmp") || !strings.HasPrefix(filepath.Base(path), "build.") {
				t.Fatalf("scratch escaped checkout: %q", path)
			}
			if _, err := os.Stat(marker); err != nil {
				t.Fatalf("command did not finish using scratch: %v", err)
			}
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("scratch survived exit: %v", err)
			}
		})
	}
}

func TestTestsUseNativeTemporaryDirectory(t *testing.T) {
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	path := t.TempDir()
	if strings.HasPrefix(path, filepath.Join(root, ".tmp")+string(os.PathSeparator)) {
		t.Fatalf("test fixture inherited build scratch: %s", path)
	}
	if err := os.WriteFile(filepath.Join(path, "fixture"), []byte("owned"), 0600); err != nil {
		t.Fatal(err)
	}
}
