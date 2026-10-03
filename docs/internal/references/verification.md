---
created_on: 2026-10-01 20:10
last_modified: 2026-10-02 17:28
status: current
---

# Verified native sessions and standalone packaging

This record summarizes inspected local execution results for maintainers of
`go-tui-frame`. It distinguishes actual macOS runtime checks from Linux
cross-build/linkage checks. Raw logs and temporary mutation copies are local
ignored artifacts; the source tests linked here are retained in the repository.

## Whole-repository checks

### Repaint performance

The repaint checks use the pinned native archive and the macOS Go toolchain.
`go test -race ./...`, `go vet ./...`, and `go mod tidy -diff` pass in the
`fix/repaint-performance` worktree. Logs are retained locally under `.tmp/` as
`performance-final-race.log`, `performance-final-vet.log`, and
`performance-final-tidy.log`. The inspected race-test output contains:

```text
ok github.com/alexgorbatchev/go-tui-frame 15.604s
ok github.com/alexgorbatchev/go-tui-frame/cmd/tui-frame 9.692s
ok github.com/alexgorbatchev/go-tui-frame/internal/emulator 1.821s
ok github.com/alexgorbatchev/go-tui-frame/internal/input (cached)
ok github.com/alexgorbatchev/go-tui-frame/internal/process 18.386s
```

[Repaint tests](../../../repaint_test.go) exercise fragmented output through a
real child PTY, a fixed first repaint deadline, flushing while idle and before
exit, immediate protocol replies, region metadata invalidation, negotiated
outer synchronized output, and restoration of an observed entry hold.
[Damage tests](../../../internal/emulator/damage_test.go)
exercise consumed native dirty flags, owned snapshots, clean-row retention,
scrolling, default-color changes, and resize.

[Performance tests](../../../performance_test.go) verify capture deferred until
repaint, immediate outer input-mode writes, reusable cleared region canvases,
independent retained drawing snapshots, zero allocations for unchanged rendering
and warmed input delivery, partial-write compaction, input origins, composition
limited to damaged rows, selected observation kinds, per-read owned snapshots,
exit-only snapshot freshness, and synchronized-update checkpoints.
[Reuse tests](../../../internal/emulator/reuse_test.go) verify stable internal
viewport storage, damage accumulated across three captures, default-style reuse,
and selection/full-style agreement with the native getters.

The compiler overlay in `.tmp/repaint-disabled.json` disables chunk batching,
metadata comparison, synchronized output, and native damage consumption without
editing the maintained source files. The corresponding regressions fail with
those changes disabled; the restored source passes. `.tmp/repaint-disabled.log`
records the behavioral failures.

The separate exit-flush overlay in `.tmp/repaint-exit-disabled.json` leaves
batching enabled and removes the final flush. Its regression fails because the
pending final text is absent; `.tmp/repaint-exit-disabled.log` records that check.

`.tmp/performance-disabled.json` separately substitutes temporary copies that
disable deferred capture, canvas/input reuse, unchanged-render skipping,
incremental composition, event selection, style reuse, and accumulated damage.
The behavioral and allocation regressions fail with these optimizations disabled;
`.tmp/performance-disabled.log` and `.tmp/performance-native-disabled.log` record
the failures. The maintained source remains enabled during the passing race run.

`BenchmarkStateCapture` and `BenchmarkBorrowedState` measure a 120×40 native
viewport with plain ASCII text over one-second benchmark intervals on an Apple M4 Pro. Owned captures
copy storage for a durable caller; internal captures reuse the viewport and maps:

| Capture | Time/op | Bytes/op | Allocations/op |
| :--- | ---: | ---: | ---: |
| Owned, unchanged | 116.2 µs | 1,010,482 | 84 |
| Internal, unchanged | 3.484 µs | 2,400 | 74 |
| Owned, one row | 173.3 µs | 1,011,512 | 451 |
| Internal, one row | 25.50 µs | 3,424 | 443 |
| Input modes only | 2.247 µs | 320 | 49 |

The session benchmarks use a 20×6 viewport with a real child PTY and an outer
output file. They select an unsupported outer synchronization mode; negotiated
synchronization is covered separately by the behavioral tests. A changing-row
benchmark alternates a character to require a visible diff each iteration:

| Session operation | Time/op | Bytes/op | Allocations/op |
| :--- | ---: | ---: | ---: |
| Unchanged render | 148.6 ns | 0 | 0 |
| Changing-row capture and repaint | 12.87 µs | 3,256 | 173 |
| Warmed input delivery | 1.027 µs | 0 | 0 |

These component measurements come from `.tmp/performance-final-bench.log`; they
do not measure end-to-end physical terminal latency. Snapshot ownership still
requires independent viewport copies when publishing to a callback. Geometry,
new graphemes/styles, and native getter calls can allocate.

The final allocation-object profiles are retained as
`.tmp/borrowed-final-allocations.log` and `.tmp/session-final-allocations.log`.
They attribute remaining internal-capture allocations to libghostty getters and
encoding, and changed-frame allocations to those bindings plus UV's renderer and
cursor encoding. UV's pinned renderer recreates touched-line storage during a
render; unchanged frames skip that call. The verified native dirty-tracking and
render-hold contracts are documented in the
[pinned native render API](https://github.com/ghostty-org/ghostty/blob/33da6848d63b3bba2b4f31ab1531d618f2795192/include/ghostty/vt/render.h).

A separate diagnostic against the original capture source observes a native
foreground of `#123456` after OSC 10 while captured and painted foregrounds
remain white when background defaults are unset. `.tmp/color-probe.log` records
that pre-existing observation issue; the repaint changes do not address it.

### Packaging and drawing checks

The coordinator's `just check` exits successfully after the live-border,
control-digit, measured-pixel and drawing-interface changes. It executes module hygiene,
library/example builds, vet, and all race-test packages. Its inspected output
contains:

```text
$ just check
go mod tidy -diff
go build ./...
go build -o bin/tui-frame ./cmd/tui-frame
go vet ./...
go test -race ./...
ok github.com/alexgorbatchev/go-tui-frame 13.097s
ok github.com/alexgorbatchev/go-tui-frame/cmd/tui-frame 9.379s
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

The whole-repository vet check has no diagnostics. Maintained
[README tests](../../../readme_test.go) compile the actual showcase and its
capture/plain-text variants against the module and native archive. These tests
do not execute interactive nvim; actual screen/session behavior has separate
native and real-PTY tests below.

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

The public drawing target is `uv.Screen`, backed by a native screen buffer with
explicit grapheme width. The [drawing tests](../../../drawing_test.go) exercise
buffer drawing, grapheme cells, and pushed replacements. CLI drawing tests use
native UV buffers and also verify an optional Lip Gloss canvas. A full CLI race
coverage run passes (`10.734s`, 91.8%). The core dependency test resolves the
actual production package graph and confirms that Lip Gloss is absent; the demo
continues to use it.

Isolated negative checks verify all three contracts: reverting the public view
to a Lip Gloss canvas fails the dependency test; disabling grapheme width renders
the tested emoji as four cells instead of two; replacing the README's Header
call with an undefined method fails its build test. Restored files are compared
with the originals. Selected drawing race tests pass (`1.446s`), and restored
README/dependency tests pass (`5.016s`). These retained tests replace manual
temporary-program checks and execute through ordinary `go test`.

The drawing-interface revision also rebuilds Linux amd64/arm64 binaries and
passes all three linkage audits: macOS imports only OS-provided libresolv and
libSystem; both Linux artifacts have no imported libraries or dynamic loader.
Cross-build/audit results do not execute Linux runtime tests.

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
