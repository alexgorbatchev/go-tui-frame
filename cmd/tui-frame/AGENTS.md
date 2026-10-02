---
created_on: 2026-10-01 13:56
last_modified: 2026-10-01 22:24
status: current
---

# Example wrapper maintenance

This directory owns the Cobra example CLI and native Lip Gloss demo painters.

## Commands

From the repository root:

- `just example run --help` runs the example's human help.
- `just example run-ai --help` runs its agent help.
- `just example build` writes the executable to `bin/tui-frame`.
- `just example test` runs the CLI tests with the race detector.
- `just native` installs the pinned native archive under ignored `.tmp/native`.
- `just build-linux` builds the Linux amd64 executable with musl and a static link;
  `just build-linux arm64` selects Linux arm64. Native Linux recipes select the
  host architecture and use separate prefixes for each archive.
- `just linkage` inspects executable dependencies; its optional argument selects
  another artifact, including the Linux cross build.
- Export the native `PKG_CONFIG_PATH` and `CGO_ENABLED=1` before invoking Go
  checks directly; see [native build setup](../../docs/internal/references/native-build.md).
- `go test -coverprofile=.tmp/cli-coverage.out ./cmd/tui-frame` measures unit
  coverage after that environment setup. Integration binaries can be instrumented
  with Go's native `-cover` and `GOCOVERDIR` support.

Demo painting can also be checked independently with
`go test -race cmd/tui-frame/demo.go cmd/tui-frame/demo_test.go`.

## Contracts and boundaries

- Keep `SKILL.md` synchronized in the same change as every command, argument,
  option, default, environment variable, output, or side-effect change. Update
  `metadata.last_modified` and verify the guide against the live Cobra tree and
  pinned dependencies. The `skill` command embeds that file verbatim.
- Preserve child argv through Cobra's native `--` handling. Keep just recipes'
  native positional arguments and quoted `"$@"`; string interpolation loses
  arguments containing spaces.
- Keep Lip Gloss optional for the core library. Paint borrowed region-local
  `uv.Screen` targets through Lip Gloss styles, layers, and compositors in this
  demo; use native `Draw(screen, bounds)` and `Bounds().Dx()/Dy()` dimensions.
  Do not retain screens, acquire terminal stdin from
  callbacks, or write directly to the outer terminal.
- Reserve header/footer geometry before `Run`. Ctrl+1 changes layout and Ctrl+2
  changes the native header background independently through immutable region
  payloads. Ctrl+3 uses `SetBorder` to change the child inset and PTY size during
  the session. Ctrl+Q cancels it. Consume reported releases without repeating
  actions; pass plain digits, extra modifiers, and F5/F6 to the child.
- Require native distinct-key negotiation for Ctrl+number controls. Keep legacy
  NUL/Escape/digit input in the child route; do not add ambiguous capture aliases.
  Test actual outer negotiation, native painted cells, and child-reported PTY
  sizes after each border change.
- Keep Ghostty statically linked. Require a Mach-O artifact to import only
  macOS system libraries; require Linux ELF to have no imported shared libraries
  or dynamic loader. Cross-build inspection does not replace Linux runtime tests.
- Whenever a code change affects execution results, change a corresponding
  behavioral test file and require 90% code coverage; `scripts/` is excluded.
  Use real canvases, files, binaries, and terminals rather than runtime stubs.
- Record new user instructions in the appropriate `AGENTS.md` upon receipt;
  check with the user before changing conflicting instructions.
- Never publish releases, tags, packages, or production deployments without
  explicit user authorization.
