package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
)

const (
	nativePackage = "libghostty-vt-static"
	nativeArchive = "libghostty-vt.a"
)

// The Ghostty binding gets its flags from a #cgo pkg-config directive. Go's
// build cache keys hash the CGO_*FLAGS environment, the package's #cgo
// directives, and packages' contents, but not pkg-config output or the files
// it names, and the binding's module-cache directory is the same in every
// checkout. These subtests build the executable through the justfile's Go
// wrapper against copies of the native prefix, with one build cache. A first
// build against this checkout's prefix fills that cache, so the parallel
// subtests compile only the packages whose keys name their prefix copy. Go
// does not share work between builds that start together on an empty cache.
func TestBuildCache(t *testing.T) {
	t.Parallel()
	source := pkgConfigVariable(t, "prefix")
	cache := repositoryTempDir(t)
	buildAgainstPrefix(t, source, cache, filepath.Join(repositoryTempDir(t), "tui-frame"), "")

	// Two checkouts with their own prefixes share one cached binding unless
	// the build environment names the prefix. This builds against two
	// prefixes, removes the first, and requires the second build to link its
	// own archive.
	t.Run("KeepsNativePrefixesApart", func(t *testing.T) {
		t.Parallel()
		first := copyNativePrefix(t, source)
		second := copyNativePrefix(t, source)

		hostLinkAgainstPrefix(t, first, cache)
		if err := os.RemoveAll(first); err != nil {
			t.Fatal(err)
		}
		link := hostLinkAgainstPrefix(t, second, cache)
		want := filepath.Join(second, "lib", nativeArchive)
		if !strings.Contains(link, want) {
			t.Errorf("host link does not use %s:\n%s", want, link)
		}
	})

	// An archive rebuilt at the same path changes no key by itself. This
	// rebuilds an existing executable, as `just build` does with
	// bin/tui-frame, after the archive at the same prefix changes, and
	// requires the link step's cache key to change.
	t.Run("FollowsNativeArchiveContents", func(t *testing.T) {
		t.Parallel()
		prefix := copyNativePrefix(t, source)
		binary := filepath.Join(repositoryTempDir(t), "tui-frame")

		buildAgainstPrefix(t, prefix, cache, binary, "")
		before := goBuildID(t, binary)
		rewriteLastMemberOwner(t, filepath.Join(prefix, "lib", nativeArchive))
		buildAgainstPrefix(t, prefix, cache, binary, "")
		if after := goBuildID(t, binary); after == before {
			t.Errorf("go build reused %s after its native archive changed; build ID %s", binary, after)
		}
	})
}

// repositoryTempDir returns a new directory under the repository's .tmp that
// is removed when the test ends. Unlike projectTempDir it does not set TMPDIR,
// which parallel tests cannot do.
func repositoryTempDir(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(filepath.Join(root, ".tmp"), "native-cache-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Errorf("remove %s: %v", dir, err)
		}
	})
	return dir
}

// rewriteLastMemberOwner changes the owner ID in the header of the archive's
// last member, as a rewrite of the archive by another tool can. Linkers ignore
// the field, so the archive still links while its bytes differ.
func rewriteLastMemberOwner(t *testing.T, archive string) {
	t.Helper()
	const (
		magic      = "!<arch>\n"
		headerSize = 60
		ownerStart = 28
		ownerEnd   = 34
		sizeStart  = 48
		sizeEnd    = 58
	)
	data, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(data), magic) {
		t.Fatalf("%s is not an ar archive", archive)
	}
	last := -1
	for offset := len(magic); offset+headerSize <= len(data); {
		size, err := strconv.Atoi(strings.TrimSpace(string(data[offset+sizeStart : offset+sizeEnd])))
		if err != nil {
			t.Fatalf("%s: member header at %d: %v", archive, offset, err)
		}
		last = offset
		offset += headerSize + size + size%2
	}
	if last < 0 {
		t.Fatalf("%s has no members", archive)
	}
	owner := data[last+ownerStart : last+ownerEnd]
	replacement := "1"
	if strings.TrimSpace(string(owner)) == replacement {
		replacement = "2"
	}
	copy(owner, fmt.Sprintf("%-*s", len(owner), replacement))
	if err := os.WriteFile(archive, data, 0o644); err != nil {
		t.Fatal(err)
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
	prefix := repositoryTempDir(t)
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

// hostLinkAgainstPrefix builds a new executable with the given native prefix
// and Go build cache and returns the external linker command that -ldflags=-v
// reports.
func hostLinkAgainstPrefix(t *testing.T, prefix, cache string) string {
	t.Helper()
	out := buildAgainstPrefix(t, prefix, cache, filepath.Join(repositoryTempDir(t), "tui-frame"), "-v")
	for line := range strings.Lines(out) {
		if strings.HasPrefix(line, "host link: ") {
			return line
		}
	}
	t.Fatalf("build against %s reported no host link:\n%s", prefix, out)
	return ""
}

// buildAgainstPrefix builds the executable to binary through the justfile's Go
// wrapper with the given native prefix, Go build cache, and linker flags, and
// returns go build's output.
func buildAgainstPrefix(t *testing.T, prefix, cache, binary, ldflags string) string {
	t.Helper()
	if os.Getenv("GOOS") == "linux" || runtime.GOOS == "linux" {
		ldflags = strings.TrimSpace(ldflags + " -linkmode=external -extldflags=-static")
	}
	wrapper, err := filepath.Abs("../../scripts/with-libghostty-cppflags")
	if err != nil {
		t.Fatal(err)
	}
	build := exec.Command(wrapper, "go", "build", "-ldflags="+ldflags, "-o", binary, ".")
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
	return string(out)
}

// goBuildID returns the Go build ID of an executable. Its first component is
// the link step's cache key.
func goBuildID(t *testing.T, binary string) string {
	t.Helper()
	out, err := exec.Command("go", "tool", "buildid", binary).Output()
	if err != nil {
		t.Fatalf("go tool buildid %s: %v", binary, err)
	}
	return strings.TrimSpace(string(out))
}
