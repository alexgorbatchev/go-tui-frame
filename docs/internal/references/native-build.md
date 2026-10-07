---
created_on: 2026-10-01 16:06
last_modified: 2026-10-07 09:54
status: current
---

# Native build setup

This reference is for library consumers building their own executables and
contributors building the example. The selected Ghostty binding uses cgo and
statically links the terminal emulator. The compiler and archive are build-time
requirements; shipped executables must pass the linkage audit below.

## Pinned inputs

| Input | Version |
| --- | --- |
| Go | `1.27.1`, from `go.mod` |
| Zig | `0.16.0`, from `scripts/native.sh` |
| just | CI installs `1.58.0` |
| Go Ghostty binding | `go.mitchellh.com/libghostty v0.0.0-20261001181910-76867c77a212` |
| Ghostty native source | `33da6848d63b3bba2b4f31ab1531d618f2795192`, from `scripts/native.sh` |
| PTY transport | `github.com/creack/pty v1.1.24` |

Install the Go toolchain, Zig 0.16.0, just, `pkg-config`, and a C compiler.
macOS builds use the system C compiler and SDK. Linux amd64 and arm64 builds use
Zig's musl C compiler. Supported build and runtime systems are Linux and macOS;
other Unix systems are outside the current contract. The bootstrap also uses
`bash`, `curl`, `tar`, and `shasum`.
On macOS, Homebrew's [`pkgconf`](https://formulae.brew.sh/formula/pkgconf)
provides the `pkg-config` executable.

## Build and verify this repository

From the repository root:

```sh
just native
just check
just linkage
just run -- nvim
just run-ai -- nvim
```

`scripts/native.sh` owns the native build contract: the Ghostty revision, the
archive's SHA-256, the required Zig version, the `zig build` flags, and the
`pkg-config` check. The `native` and `native-linux` recipes run it with
`.tmp/native` as its root; consumers run the same script, see
[Build a consumer](#build-a-consumer). `TestNativeScriptBuildsBindingRevision`
in `cmd/tui-frame` fails when the script's Ghostty revision differs from the
`GIT_TAG` that the `CMakeLists.txt` of the binding version selected by `go.mod`
fetches, so a binding update in `go.mod` must update the script in the same
change.

`just native` downloads the pinned native source archive, verifies its SHA-256,
extracts it into `.tmp/native/ghostty`, and builds only libghostty-vt with:

```sh
zig build -Demit-lib-vt -Demit-xcframework=false -Doptimize=ReleaseFast
```

The script caches the archive as `.tmp/native/ghostty-<revision>.tar.gz`. It
downloads to a temporary file in the same directory and renames that file to
the cached path only after the checksum passes. A failed or interrupted
transfer, or a download with other bytes, leaves no file at the cached path, so
the next run downloads again. When the script exits during the transfer,
including on SIGINT, SIGQUIT, or SIGTERM, it stops curl and removes the
temporary file. A download that fails the check stops the script with the URL,
the cached path, and the expected digest. A cached archive that fails the
check, because its digest differs or it cannot be read, is removed and
downloaded again. When `shasum` is interrupted or cannot run, the script stops
with its status and leaves the cached archive in place. The script needs
`shasum` only while `.tmp/native/ghostty` does not exist; without it, the script
stops before it checks or downloads the archive. Recipes that run Go always need
`shasum`; see below.

The script extracts the archive into a temporary directory under
`.tmp/native` and renames it to `.tmp/native/ghostty`. `just native` and
`just native-linux` share that directory, so runs that start together can both
extract. `mv` moves a directory into an existing target directory instead of
failing, so each run renames its tree only while it holds the lock file
`.tmp/native/ghostty.lock`, and checks for `.tmp/native/ghostty` again first.
When another run has already renamed its tree, the run checks that tree's
revision marker and removes its own extraction. The lock is created with the
shell's [noclobber option](https://pubs.opengroup.org/onlinepubs/9799919799/utilities/V3_chap02.html#tag_19_07_02),
which creates a file atomically, as
[`open()`](https://pubs.opengroup.org/onlinepubs/9799919799/functions/open.html)
does with `O_CREAT` and `O_EXCL`. It holds the run's process ID and is removed
when the run exits, including on SIGINT, SIGQUIT, or SIGTERM, which also remove
the temporary directory. A run ended by SIGKILL can leave the lock behind, so
a run waits at most 30 seconds for it and then stops with the lock's path.
Remove that file when no native build is running.

Ghostty's build runs `git` in its source directory to detect its version. The
extracted archive is not a git repository, so the script runs `zig build`
without git's repository-local variables and with
[`GIT_CEILING_DIRECTORIES`](https://git-scm.com/docs/git#Documentation/git.txt-GITCEILINGDIRECTORIES)
set to `.tmp/native`. It removes the variables that
[`git rev-parse --local-env-vars`](https://git-scm.com/docs/git-rev-parse#Documentation/git-rev-parse.txt---local-env-vars)
lists. [Git hooks](https://git-scm.com/docs/githooks) export `GIT_DIR` and
`GIT_WORK_TREE`, `git rebase --exec` exports `GIT_DIR` in a linked worktree, and
the ceiling does not apply to an explicit `GIT_DIR`. Git's repository
discovery then stops at `.tmp/native`, so Ghostty uses the development version
it assigns to builds outside a git checkout. Without these limits, git reports
this repository's branch, commit, and tag to Ghostty, and a tag that is not
Ghostty's matching release tag aborts the build. When `git` is not installed,
Ghostty uses the same development version.

The recipes pass `.tmp/native` as the root and name the installation and cache
directories relative to it, and the script hands Zig their absolute paths. macOS
uses `.tmp/native/prefix`. Linux selects the host architecture with just's
native `arch()` function and keeps each archive in its own prefix:

| Go architecture | Zig target | Prefix under `.tmp/native/` |
| --- | --- | --- |
| `amd64` | `x86_64-linux-musl` | `linux-musl/amd64/prefix` |
| `arm64` | `aarch64-linux-musl` | `linux-musl/arm64/prefix` |

The Linux target is passed to both Zig's archive build and cgo's C compiler.
An existing source directory must contain the
matching `.frame-source-revision`; the script refuses an unmarked or differently
pinned directory. Zig caches and downloaded sources remain under ignored
`.tmp/native/`. Binaries go to ignored `bin/`.

The binding gets the archive's include and link flags from a `#cgo pkg-config`
directive. Go's build cache key for a cgo package includes the `CGO_CPPFLAGS`,
`CGO_CFLAGS`, and `CGO_LDFLAGS` environment and the package's `#cgo`
directives, but not `pkg-config` output or the files it names (`buildActionID`
in Go 1.27.1's `cmd/go/internal/work/exec.go`; see also
[`go help cache`](https://pkg.go.dev/cmd/go#hdr-Build_and_test_caching)). The
binding's module-cache directory is the same in every checkout, so without
another key input a binding compiled in one checkout is reused in the others
and links the first checkout's archive. Once that checkout's `.tmp/native` is
removed, the other checkouts fail to link. An archive rebuilt at the same
prefix, for example for another Ghostty revision, changes no key either: Go
reuses the compiled binding, the existing `bin/tui-frame`, and cached test
results.

Recipes that compile cgo therefore run `go` through
`scripts/with-libghostty-cppflags`. The script appends two values to
`CGO_CPPFLAGS`, where Go also puts `pkg-config`'s `--cflags` output:

- `pkg-config --static --cflags libghostty-vt-static`, the include flag that
  names the prefix, so each prefix gets its own cache entries.
- `-DGO_TUI_FRAME_LIBGHOSTTY_VT_SHA256=<digest>`, the SHA-256 of the archive
  that `pkg-config --static --libs` names. No C code reads the macro; it only
  identifies the archive. Go records `CGO_CPPFLAGS` in each executable's build
  information, so a changed archive also changes the cache key of the link step
  and of test results. Hashing the 11 MB macOS arm64 archive with `shasum`
  takes about 40 ms per command on an Apple M4 Pro.

Go does not record cgo flags in the build information of a `-trimpath` build,
so such a build still reuses its previous link after the archive changes. The
recipes do not use `-trimpath`. The `--libs` output is not exported through
`CGO_LDFLAGS`: Go applies that variable to every cgo package, so the archive
would reach the final link once for each of them, and the macOS linker warns
about the duplicates.

`just check` checks module hygiene, builds all packages and `bin/tui-frame`,
runs vet, and runs race tests. `just linkage` inspects the resulting executable.
Its Mach-O audit permits only OS-provided `/usr/lib/` or `/System/Library/`
dependencies. Its ELF audit requires an empty `ImportedLibraries()` result and
no `PT_INTERP` dynamic loader. macOS's system libraries are provided by the OS;
Linux executables must include musl rather than require a distro's libc.

Build and inspect a Linux amd64 artifact from either supported build host:

```sh
just build-linux
just linkage bin/tui-frame-linux-amd64

just build-linux arm64
just linkage bin/tui-frame-linux-arm64
```

`build-linux` and `native-linux` accept an optional `amd64` or `arm64` argument;
both default to `amd64`. `native` selects the current host instead. Use the
explicit cross-build recipe when setting a different target architecture.

Cross-compilation and binary inspection do not execute Linux terminal tests on
macOS. The Linux CI job runs those tests on Linux amd64; the macOS job runs on
arm64. Local artifact audits cover macOS arm64 and statically linked Linux
amd64/arm64 executables. Linux runtime tests have not executed locally.

## Build a consumer

A consumer builds the archive with the `scripts/native.sh` of the go-tui-frame
module that its `go.mod` selects, so the Ghostty revision, archive checksum, Zig
version, and build flags follow the module version it builds against. Resolve
the module directory with `go list -m`. Its `Dir` field is empty until the
module is in the module cache, so download it first. When a `go.work` file uses
a local go-tui-frame checkout, `go list -m` reports that checkout instead.
Module versions that predate `scripts/native.sh`, including `v1.0.0`, do not
contain it.

The module cache keeps files read-only and without their executable bit, so run
the scripts with `bash`. `scripts/native.sh` takes four arguments: an absolute
output root, the Zig target, and the installation prefix and Zig cache
directories relative to that root. It writes only under the root, prints the
archive's `pkg-config` flags when it finishes, and exits with status 2 and a
usage message when an argument is missing, the root is not absolute, or the
prefix or cache would leave the root. For a macOS consumer:

```sh
go mod download github.com/alexgorbatchev/go-tui-frame
frame=$(go list -m -f '{{.Dir}}' github.com/alexgorbatchev/go-tui-frame)
native="$PWD/.tmp/native"
bash "$frame/scripts/native.sh" "$native" native prefix zig-cache

export CGO_ENABLED=1
export PKG_CONFIG_PATH="$native/prefix/share/pkgconfig"
bash "$frame/scripts/with-libghostty-cppflags" go build ./...
```

Run the consumer's ordinary Go build through the module's
`scripts/with-libghostty-cppflags`. Go's build cache is shared by every project
of the user, so the wrapper's prefix and archive identity keep it from reusing a
binding or link built against another archive. The wrapper computes them for
each command; a `CGO_CPPFLAGS` value exported once would keep the digest of the
archive that existed at export time.

For a Linux amd64 consumer, build the musl archive and use a static final link:

```sh
bash "$frame/scripts/native.sh" "$native" x86_64-linux-musl linux-musl/amd64/prefix linux-musl-cache

export CGO_ENABLED=1
export GOOS=linux GOARCH=amd64
export CC='zig cc -target x86_64-linux-musl'
export PKG_CONFIG_PATH="$native/linux-musl/amd64/prefix/share/pkgconfig"
bash "$frame/scripts/with-libghostty-cppflags" go build -ldflags='-linkmode=external -extldflags=-static' ./...
```

The wrapper keeps any `CGO_CPPFLAGS` value already set, including one set with
`go env -w`, and appends its own flags.

For Linux arm64, pass the Zig target `aarch64-linux-musl`, the prefix
`linux-musl/arm64/prefix`, and the cache `linux-arm64-musl-cache` to
`scripts/native.sh`, then change `GOARCH` to `arm64`, the C compiler target to
`aarch64-linux-musl`, and the `PKG_CONFIG_PATH` prefix segment to
`linux-musl/arm64/prefix`. Keep the static final-link flags.

The selected binding's default [`cgo_static.go`](https://github.com/mitchellh/go-libghostty/blob/76867c77a212/cgo_static.go)
requests `pkg-config --static libghostty-vt-static`. The generated metadata
names `libghostty-vt.a` explicitly. The `dynamic` build tag selects a different
link contract and is not used by these recipes.

## Primary references

- [Ghostty binding build requirements](https://github.com/mitchellh/go-libghostty/blob/76867c77a212/README.md)
- [Pinned native build](https://github.com/ghostty-org/ghostty/blob/33da6848d63b3bba2b4f31ab1531d618f2795192/build.zig)
- [Pinned native Zig manifest](https://github.com/ghostty-org/ghostty/blob/33da6848d63b3bba2b4f31ab1531d618f2795192/build.zig.zon)
- [Pinned native git version detection](https://github.com/ghostty-org/ghostty/blob/33da6848d63b3bba2b4f31ab1531d618f2795192/src/build/GitVersion.zig)
- [Git repository discovery boundary](https://git-scm.com/docs/git#Documentation/git.txt-GITCEILINGDIRECTORIES)
- [Git repository-local environment variables](https://git-scm.com/docs/git-rev-parse#Documentation/git-rev-parse.txt---local-env-vars)
- [Official Zig versions and checksums](https://ziglang.org/download/index.json)
- [Zig musl cross-compilation](https://ziglang.org/learn/overview/#cross-compilation)
- [Go cgo compiler and pkg-config environment](https://pkg.go.dev/cmd/cgo)
- [Go linker flags](https://pkg.go.dev/cmd/link)
- [just host architecture functions](https://just.systems/man/en/functions.html#system-information)
- [just recipe arguments and dependency parameters](https://just.systems/man/en/recipe-parameters.html)
- [ELF imported libraries](https://pkg.go.dev/debug/elf#File.ImportedLibraries)
- [Mach-O imported libraries](https://pkg.go.dev/debug/macho#File.ImportedLibraries)
