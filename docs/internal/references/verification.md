---
created_on: 2026-10-01 20:10
last_modified: 2026-10-01 20:10
status: current
---

# Verified native sessions and standalone packaging

This record summarizes inspected local execution results for maintainers of
`go-tui-frame`. It distinguishes actual macOS runtime checks from Linux
cross-build/linkage checks. Raw logs and temporary mutation copies are local
ignored artifacts; the source tests linked here are retained in the repository.

## Whole-repository checks

The coordinator's final `just check` executes module hygiene, library/example
builds, vet, and all race-test packages. The inspected output contains:

```text
$ just check
go mod tidy -diff
go build ./...
go build -o bin/tui-frame ./cmd/tui-frame
go vet ./...
go test -race ./...
ok github.com/alexgorbatchev/go-tui-frame 4.826s
ok github.com/alexgorbatchev/go-tui-frame/cmd/tui-frame 9.018s
ok github.com/alexgorbatchev/go-tui-frame/internal/emulator (cached)
ok github.com/alexgorbatchev/go-tui-frame/internal/input (cached)
ok github.com/alexgorbatchev/go-tui-frame/internal/process (cached)
```

A separate uncached reviewer run after the keypad/native fixes passes:

```text
$ CGO_ENABLED=1 PKG_CONFIG_PATH="$PWD/.tmp/native/prefix/share/pkgconfig" go test -race -count=1 . ./internal/input ./internal/emulator ./internal/process
ok github.com/alexgorbatchev/go-tui-frame 5.424s
ok github.com/alexgorbatchev/go-tui-frame/internal/input 2.712s
ok github.com/alexgorbatchev/go-tui-frame/internal/emulator 3.369s
ok github.com/alexgorbatchev/go-tui-frame/internal/process 20.394s
```

The same four packages pass `go vet` with no diagnostics. The README's first
Go example compiles in a temporary executable wrapper against the actual
module and native archive; that check does not execute interactive nvim.

## Behavior and deliberate negative checks

| Retained test | Verified behavior and negative check |
| :--- | :--- |
| [Real framed session](../../../session_test.go) | Idle push rendering, child/frame isolation, title/query replies, native SID/group/argv/session inventory, real PTY geometry/settings/foreground, observer-copy isolation and terminal restoration. Disabling metadata collection fails native child identity; disabling wake-triggered rendering fails the idle-push display assertion. Both restored checks pass. |
| [Wake pipe](../../../wake_test.go) | Native pipe wake/coalescing/failure propagation. Disabling failure handling makes Poll remain unready and loses the cleanup error; the restored test passes. |
| [Native routing](../../../routing_test.go) | Original-byte agreement, native key conversion, paste envelopes, focus, localized cell/pixel mouse and capture gestures. Deliberately disabling key agreement, paste/bounds/text-release guards reproduces behavioral failures. |
| [Cursor/keypad provenance](../../../routing_test.go) | SS3/C1 cursor input converts for a normal child. SS3 keypad0/Enter converts for a numeric child; matching C1 application input remains exact. Disabling the keypad guard fails all four cases; restored routing race tests pass. |
| [Native graphics profile](../../../internal/emulator/profile_test.go) | Disabled Kitty graphics does not return a positive capability response before/after reset or alternate-screen operations. The original positive reply was reproduced before native protocol disablement. |
| [Native process ownership](../../../internal/process/snapshot_test.go), [groups](../../../internal/process/groups_test.go), [Darwin argv](../../../internal/process/args_darwin_test.go) | Current native fields, runtime cwd and owned copies; same-session inventory survives a reaped launch leader. Darwin empty argv0/alignment have dedicated native tests. Linux test binaries compile, without a local Linux execution claim. |
| [Example CLI](../../../cmd/tui-frame/session_test.go) | Real PTY F5 previous/F6 next updates, Ctrl-Q, ordinary child input, complete argv including empty/spaced/flag values, native exit/signal outcomes, termios restoration, and embedded guide/help behavior. The inspected CLI race suite reports 92.0% statement coverage. |

The coordinator's isolated mutation copy restores the metadata, wake rendering and
wake changes, then reruns the selected session/wake tests successfully. Mutation
results establish detection of those behaviors, not exhaustive conformance.

## Artifact audits

[Standalone-binary tests](../../../cmd/tui-frame/linkage_test.go) inspect actual
binary import tables and ELF interpreter segments. Inspected rebuilt artifacts:

| Artifact | Actual audit result |
| :--- | :--- |
| macOS arm64 Mach-O | Only `/usr/lib/libresolv.9.dylib` and `/usr/lib/libSystem.B.dylib`; no separately installed libghostty dependency. |
| Linux amd64 ELF | Statically linked; imported libraries `[]`, no `PT_INTERP`. |
| Linux arm64 ELF | Statically linked; imported libraries `[]`, no `PT_INTERP`. |

These audits follow the [pinned native build](native-build.md). Source changes
require rebuilding and reauditing the resulting binaries. Linux executables
were cross-built and inspected on macOS; they were not run locally. No remote
CI execution, published release or consumer `go get` result is established.

## Fidelity boundaries

Passing tests do not establish arbitrary-TUI transparency. The current profile
does not render graphics/overline, expose complete screen/history/margin/damage
state, retain all hyperlink parameters, or guarantee outside-release gesture
closure. Unsupported input identities, native mouse limits and unavailable
pixel precision remain explicit. OS observations are sampled and
permission-limited; detached sessions and wrapper suspend/resume remain
teardown boundaries. See the [consumer profile](../../../README.md) and
[architecture research](../../../reports/TUI%20frame%20architecture%20research.md).
