---
created_on: 2026-10-01 20:10
last_modified: 2026-10-01 21:11
status: current
---

# Verified native sessions and standalone packaging

This record summarizes inspected local execution results for maintainers of
`go-tui-frame`. It distinguishes actual macOS runtime checks from Linux
cross-build/linkage checks. Raw logs and temporary mutation copies are local
ignored artifacts; the source tests linked here are retained in the repository.

## Whole-repository checks

The coordinator's `just check` exits successfully after the live-border,
control-digit and measured-pixel changes. It executes module hygiene,
library/example builds, vet, and all race-test packages. Its inspected output
contains:

```text
$ just check
go mod tidy -diff
go build ./...
go build -o bin/tui-frame ./cmd/tui-frame
go vet ./...
go test -race ./...
ok github.com/alexgorbatchev/go-tui-frame 7.421s
ok github.com/alexgorbatchev/go-tui-frame/cmd/tui-frame 9.145s
ok github.com/alexgorbatchev/go-tui-frame/internal/emulator (cached)
ok github.com/alexgorbatchev/go-tui-frame/internal/input (cached)
ok github.com/alexgorbatchev/go-tui-frame/internal/process (cached)
```

A component race check for the live-border/capture source passes with these
inspected results:

```text
ok github.com/alexgorbatchev/go-tui-frame 7.576s
ok github.com/alexgorbatchev/go-tui-frame/internal/input 1.604s
ok github.com/alexgorbatchev/go-tui-frame/internal/emulator 1.858s
ok github.com/alexgorbatchev/go-tui-frame/internal/process 18.313s
```

The whole-repository vet check has no diagnostics. The README's unchanged first Go
example compiles in a temporary executable wrapper against the actual module
and native archive; that check does not execute interactive nvim.

The current live-border/capture changes pass an independent targeted race run:

```text
$ CGO_ENABLED=1 PKG_CONFIG_PATH="$PWD/.tmp/native/prefix/share/pkgconfig" go test -race -count=1 -run 'Test(CaptureNegotiates|SetBorder|RunBorder)' .
ok github.com/alexgorbatchev/go-tui-frame 3.928s
```

The native reported-MOK2 fallback race check passes (`1.758s`). The CLI author's
new real-PTY control race check passes (`2.000s`); its full
race suite passes (`9.850s`, 91.8% statement coverage). The updated embedded
guide also passes its validator. The measured-pixel correction passes a
selected native race check (`4.509s`). The whole-repository check and the rebuilt
binary audits below include that correction.

## Behavior and deliberate negative checks

| Retained test | Verified behavior and negative check |
| :--- | :--- |
| [Real framed session](../../../session_test.go) | Idle push rendering, child/frame isolation, title/query replies, native SID/group/argv/session inventory, real PTY geometry/settings/foreground, observer-copy isolation and terminal restoration. Disabling metadata collection fails native child identity; disabling wake-triggered rendering fails the idle-push display assertion. Both restored checks pass. |
| [Wake pipe](../../../wake_test.go) | Native pipe wake/coalescing/failure propagation. Disabling failure handling makes Poll remain unready and loses the cleanup error; the restored test passes. |
| [Native routing](../../../routing_test.go) | Original-byte agreement, native key conversion, paste envelopes, focus, localized cell/pixel mouse and capture gestures. Deliberately disabling key agreement, paste/bounds/text-release guards reproduces behavioral failures. |
| [Cursor/keypad provenance](../../../routing_test.go) | SS3/C1 cursor input converts for a normal child. SS3 keypad0/Enter converts for a numeric child; matching C1 application input remains exact. Disabling the keypad guard fails all four cases; restored routing race tests pass. |
| [Native graphics profile](../../../internal/emulator/profile_test.go) | Disabled Kitty graphics does not return a positive capability response before/after reset or alternate-screen operations. The original positive reply was reproduced before native protocol disablement. |
| [Native process ownership](../../../internal/process/snapshot_test.go), [groups](../../../internal/process/groups_test.go), [Darwin argv](../../../internal/process/args_darwin_test.go) | Current native fields, runtime cwd and owned copies; same-session inventory survives a reaped launch leader. Darwin empty argv0/alignment have dedicated native tests. Linux test binaries compile, without a local Linux execution claim. |
| [Live border](../../../border_test.go) | Before/during-Run updates and closed-session rejection, applied native grid and actual PTY size, including measured cell pixels when outer winsize has only cells. Requests coalesce to the latest border inset; region reservations remain fixed. Disabling border publication fails the child-layout assertion; disabling pixel preservation yields zero native/PTY pixels. |
| [Capture negotiation](../../../console_test.go) | A real native outer terminal applies Kitty disambiguation only with explicit capture and restores its keyboard state. Child event-reporting flags are retained; supported modifyOtherKeys is the fallback when Kitty is unavailable. Disabling enhancement fails both native Kitty flag cases and the native MOK2 fallback assertion. |
| [Example CLI](../../../cmd/tui-frame/session_test.go) | Real PTY Ctrl-1 layout and Ctrl-2 independent backgrounds, Ctrl-3 child border with actual PTY resize, selected-release exclusion, ordinary digits/unmatched keys and termios restoration. Separate tests cover Ctrl-Q, complete argv, exit/signal outcomes and embedded guide/help behavior. Current full CLI race coverage is 91.8%. |

The coordinator's isolated mutation copy restores the metadata, wake rendering and
wake changes, then reruns the selected session/wake tests successfully. Mutation
results establish detection of those behaviors, not exhaustive conformance.

The live-border/capture negative checks also use an isolated source copy.
Disabling border publication fails `TestSetBorderUpdatesLayoutBeforeAndDuringRun`
because the child remains inset. Disabling capture enhancement fails the native
outer's expected flags (`0` vs `1`, `2` vs `3`) and the reported MOK2 fallback.
The isolated source files are restored and compared with the originals; the
selected restored check passes (`4.436s`) with verified process exit 0.

The native border test also uses an outer OS winsize without pixel dimensions
while the terminal query reports 10×20-pixel cells. Removing preservation during
a border-only update fails with zero native cell pixels and zero PTY pixel
dimensions. Restoring byte-identical source passes the selected race check
(`2.898s`) with verified process exit 0.

The CLI author's isolated native mutation checks disable independent palette
selection and the SetBorder call separately. The first fails the actual native
cell background assertion; the second fails the real-PTY viewport update.
Restored byte-identical source passes the selected CLI tests (`3.345s`). A
separate real-PTY agent-mode test checks plain initial regions/no child border,
then Ctrl-3 enables the border and resizes the child.

## Artifact audits

[Standalone-binary tests](../../../cmd/tui-frame/linkage_test.go) inspect actual
binary import tables and ELF interpreter segments. All three rebuilt artifacts
pass after the live-border/control-digit and measured-pixel changes:

| Artifact | Actual audit result |
| :--- | :--- |
| macOS arm64 Mach-O | Only `/usr/lib/libresolv.9.dylib` and `/usr/lib/libSystem.B.dylib`; no separately installed libghostty dependency. |
| Linux amd64 ELF | Statically linked; imported libraries `[]`, no `PT_INTERP`. |
| Linux arm64 ELF | Statically linked; imported libraries `[]`, no `PT_INTERP`. |

The inspected `just linkage` runs for `bin/tui-frame`,
`bin/tui-frame-linux-amd64` and `bin/tui-frame-linux-arm64` exit 0 (`0.755s`,
`0.934s` and `0.663s`, respectively). The macOS binary contains the byte-identical
current embedded usage guide. Each Linux build also exits 0.

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
