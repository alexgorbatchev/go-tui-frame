package main

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

const (
	nativePackage = "libghostty-vt-static"
	nativeArchive = "libghostty-vt.a"
)

// The Ghostty binding gets its flags from a #cgo pkg-config directive. Go's
// build cache key for a cgo package covers the CGO_*FLAGS environment and the
// package's #cgo directives, but not pkg-config output, and the binding's
// module-cache directory is the same in every checkout. Two checkouts with
// their own native prefixes therefore share one cached binding unless the
// build environment names the prefix. This builds the executable through the
// justfile's Go wrapper against two prefixes that share one build cache,
// removes the first, and requires the second build to link its own archive.
func TestBuildCacheKeepsNativePrefixesApart(t *testing.T) {
	source := pkgConfigVariable(t, "prefix")
	cache := filepath.Join(projectTempDir(t), "gocache")
	first := copyNativePrefix(t, source)
	second := copyNativePrefix(t, source)

	buildAgainstPrefix(t, first, cache)
	if err := os.RemoveAll(first); err != nil {
		t.Fatal(err)
	}
	link := buildAgainstPrefix(t, second, cache)
	want := filepath.Join(second, "lib", nativeArchive)
	if !strings.Contains(link, want) {
		t.Errorf("host link does not use %s:\n%s", want, link)
	}
}

func pkgConfigVariable(t *testing.T, name string) string {
	t.Helper()
	out, err := exec.Command("pkg-config", "--variable="+name, nativePackage).Output()
	if err != nil {
		t.Fatalf("pkg-config --variable=%s %s: %v", name, nativePackage, err)
	}
	return strings.TrimSpace(string(out))
}

// copyNativePrefix copies the files the static build reads into a new prefix
// and points the pkg-config file's prefix variable at it, as Zig's install
// step does for each checkout's --prefix.
func copyNativePrefix(t *testing.T, source string) string {
	t.Helper()
	prefix := projectTempDir(t)
	if err := os.CopyFS(filepath.Join(prefix, "include"), os.DirFS(filepath.Join(source, "include"))); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(prefix, "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	copyFile(t, filepath.Join(source, "lib", nativeArchive), filepath.Join(prefix, "lib", nativeArchive))

	pcDir := filepath.Join(prefix, "share", "pkgconfig")
	if err := os.MkdirAll(pcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	pc, err := os.ReadFile(filepath.Join(source, "share", "pkgconfig", nativePackage+".pc"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(pc), "\n")
	i := slices.IndexFunc(lines, func(line string) bool { return strings.HasPrefix(line, "prefix=") })
	if i < 0 {
		t.Fatalf("%s.pc has no prefix variable:\n%s", nativePackage, pc)
	}
	lines[i] = "prefix=" + prefix
	if err := os.WriteFile(filepath.Join(pcDir, nativePackage+".pc"), []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}
	return prefix
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	src, err := os.Open(from)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := src.Close(); err != nil {
			t.Errorf("close %s: %v", from, err)
		}
	}()
	dst, err := os.Create(to)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		t.Fatal(err)
	}
	if err := dst.Close(); err != nil {
		t.Fatal(err)
	}
}

// buildAgainstPrefix builds the executable with the given native prefix and
// Go build cache and returns the external linker command that -ldflags=-v
// reports.
func buildAgainstPrefix(t *testing.T, prefix, cache string) string {
	t.Helper()
	ldflags := "-v"
	if os.Getenv("GOOS") == "linux" || runtime.GOOS == "linux" {
		ldflags += " -linkmode=external -extldflags=-static"
	}
	wrapper, err := filepath.Abs("../../scripts/with-libghostty-cppflags")
	if err != nil {
		t.Fatal(err)
	}
	build := exec.Command(wrapper, "go", "build", "-ldflags="+ldflags, "-o", filepath.Join(projectTempDir(t), "tui-frame"), ".")
	// The test itself runs inside the wrapper, so drop the CGO_CPPFLAGS it
	// inherited for this checkout's prefix.
	build.Env = slices.DeleteFunc(os.Environ(), func(kv string) bool {
		return strings.HasPrefix(kv, "CGO_CPPFLAGS=") || strings.HasPrefix(kv, "PKG_CONFIG_PATH=") || strings.HasPrefix(kv, "GOCACHE=")
	})
	build.Env = append(build.Env, "PKG_CONFIG_PATH="+filepath.Join(prefix, "share", "pkgconfig"), "GOCACHE="+cache)
	out, err := build.CombinedOutput()
	if err != nil {
		t.Fatalf("build against %s: %v\n%s", prefix, err, out)
	}
	for line := range strings.Lines(string(out)) {
		if strings.HasPrefix(line, "host link: ") {
			return line
		}
	}
	t.Fatalf("build against %s reported no host link:\n%s", prefix, out)
	return ""
}
