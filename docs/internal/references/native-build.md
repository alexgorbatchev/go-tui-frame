---
created_on: 2026-10-01 16:06
last_modified: 2026-10-05 10:28
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
| Zig | `0.16.0` |
| just | CI installs `1.58.0` |
| Go Ghostty binding | `go.mitchellh.com/libghostty v0.0.0-20261001181910-76867c77a212` |
| Ghostty native source | `33da6848d63b3bba2b4f31ab1531d618f2795192` |
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

`just native` downloads the pinned native source into `.tmp/native/ghostty`,
verifies its SHA-256, and builds only libghostty-vt with:

```sh
zig build -Demit-lib-vt -Demit-xcframework=false -Doptimize=ReleaseFast
```

Ghostty's build runs `git` in its source directory to detect its version. The
extracted archive is not a git repository, so the recipe runs `zig build`
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

The recipe supplies absolute installation and cache directories. macOS uses
`.tmp/native/prefix`. Linux selects the host architecture with just's native
`arch()` function and keeps each archive in its own prefix:

| Go architecture | Zig target | Prefix under `.tmp/native/` |
| --- | --- | --- |
| `amd64` | `x86_64-linux-musl` | `linux-musl/amd64/prefix` |
| `arm64` | `aarch64-linux-musl` | `linux-musl/arm64/prefix` |

The Linux target is passed to both Zig's archive build and cgo's C compiler.
An existing source directory must contain the
matching `.frame-source-revision`; the recipe refuses an unmarked or differently
pinned directory. Zig caches and downloaded sources remain under ignored
`.tmp/native/`. Binaries go to ignored `bin/`.

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

Build the pinned archive first. For a macOS consumer, export its actual absolute
prefix before running the consumer's ordinary Go build:

```sh
export CGO_ENABLED=1
export PKG_CONFIG_PATH="/absolute/path/to/go-tui-frame/.tmp/native/prefix/share/pkgconfig"
go build ./...
```

For a Linux amd64 consumer, use the musl archive and a static final link:

```sh
export CGO_ENABLED=1
export GOOS=linux GOARCH=amd64
export CC='zig cc -target x86_64-linux-musl'
export PKG_CONFIG_PATH="/absolute/path/to/go-tui-frame/.tmp/native/linux-musl/amd64/prefix/share/pkgconfig"
go build -ldflags='-linkmode=external -extldflags=-static' ./...
```

For Linux arm64, first run `just native-linux arm64`, then change `GOARCH` to
`arm64`, the C compiler target to `aarch64-linux-musl`, and the prefix segment to
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
