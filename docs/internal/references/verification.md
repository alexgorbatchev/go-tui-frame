---
created_on: 2026-10-01 20:10
last_modified: 2026-10-07 09:27
status: current
---

# Verified native sessions and standalone packaging

This reference is for maintainers of `go-tui-frame` who compare a change
against the library's measured costs and verified behavior. It records the
benchmark and allocation results the current code produces, what the retained
tests verify, and the standalone-binary audits. macOS results come from local
runs and CI. Linux results come from CI and from executables cross-built and
inspected on macOS.

## Measurement method

The figures below were measured on 2026-10-06 at commit `f674572` on macOS
arm64 (Apple M4 Pro, 14 cores) with Go 1.27.1 and the pinned Ghostty revision
`33da6848d63b3bba2b4f31ab1531d618f2795192`.

Direct Go commands need the native environment that the justfile exports. Run
`just native` once, then export the environment from the repository root. On
macOS:

```sh
export CGO_ENABLED=1 PKG_CONFIG_PATH="$PWD/.tmp/native/prefix/share/pkgconfig" TMPDIR="$PWD/.tmp"
```

The commands below run `go` through `scripts/with-libghostty-cppflags`, as the
recipes do. It adds this checkout's prefix and the archive's SHA-256 to
`CGO_CPPFLAGS`; without them, Go can reuse a binding or test binary built
against another archive. Linux uses a different prefix and compiler; see the
[pinned native build](native-build.md).

Allocation counts are deterministic: each benchmark reports the same count in
every run. Byte counts are also identical across runs, except for owned
captures, which vary by a few bytes/op between runs. Re-measure both when a
change touches capture, repaint or input delivery.

Timings depend on machine load and are indicative only. The machine was shared
with other work during the benchmark runs: load averages were 3.1–3.5 over
1 minute and 7.6–8.7 over 5 minutes. Each table reports the median of three
one-second runs. Compare a timing only with a run on the same machine under
similar load.

## Capture benchmarks

`BenchmarkStateCapture` in [damage tests](../../../internal/emulator/damage_test.go)
and `BenchmarkBorrowedState` in [reuse tests](../../../internal/emulator/reuse_test.go)
capture a 120×40 native viewport with plain ASCII text. Owned captures
(`State`) copy storage for a durable caller. Internal captures (`UpdateState`)
reuse the viewport and its maps. A one-row capture first writes
`\x1b[Hupdated`. A full-viewport capture first erases the display and fills
all 40 rows with text, so it converts every cell. Input modes only reads the terminal modes, keyboard protocol
flags and mouse tracking (`InputState`), without the display. It includes the
key- and mouse-encoder probes that derive `ModifyOtherKeys2` and
`MouseTrackingMode`.

```sh
scripts/with-libghostty-cppflags go test -run '^$' -bench 'BenchmarkBorrowedState|BenchmarkStateCapture' -benchmem -count=3 ./internal/emulator
```

| Capture | Time/op (indicative) | Bytes/op | Allocations/op |
| :--- | ---: | ---: | ---: |
| Owned, unchanged | 158.4 µs | about 1,010,680 | 89 |
| Owned, one row | 120.6 µs | about 1,010,756 | 96 |
| Internal, unchanged | 3.804 µs | 2,600 | 79 |
| Internal, one row | 8.076 µs | 2,664 | 88 |
| Internal, full viewport | 169.3 µs | 4,848 | 322 |
| Input modes only | 2.541 µs | 520 | 55 |

These capture figures were re-measured on 2026-10-07 on the same machine after
cell conversion began writing each cell in place and passing the render colors
by pointer. Load averages were 11.2–13.9 over 1 minute during that run. In an
interleaved comparison of eight runs per binary under similar load, the
full-viewport median fell from 433.3 µs to 175.4 µs and the one-row median from
15.31 µs to 8.413 µs; bytes and allocations did not change.

The owned one-row median is below the owned unchanged median. An owned capture
allocates about 1 MB more than an internal one because `State` clones the
viewport's cell storage.

The allocation profile of an internal one-row capture attributes 99.9% of
allocated objects to libghostty binding calls. These are terminal getters
(`Terminal.Mode` alone is 32%), render-state cursor, color and row reads, the
key-encoder call that derives `ModifyOtherKeys2`, and the mouse-encoder calls
that derive `MouseTrackingMode`:

```sh
scripts/with-libghostty-cppflags go test -run '^$' -bench 'BenchmarkBorrowedState/one_row' -benchmem -memprofile .tmp/borrowed.mem -memprofilerate=1 -o .tmp/emulator.test ./internal/emulator
go tool pprof -sample_index=alloc_objects -top .tmp/emulator.test .tmp/borrowed.mem
```

### Row capture allocations

`TestRowCaptureAllocationsDoNotScaleWithWidth` in
[row tests](../../../internal/emulator/row_test.go) logs the allocations of an
unchanged internal capture and of a capture after one damaged row. It runs at
20, 120 and 240 columns:

```sh
scripts/with-libghostty-cppflags go test -count=1 -v -run 'TestRowCaptureAllocationsDoNotScaleWithWidth' ./internal/emulator
```

| Damaged row | Unchanged | Damaged | Additional | Test limit |
| :--- | ---: | ---: | ---: | ---: |
| Plain text (`updated`) | 74 | 83 | 9 | 12 |
| Two styles across the full width | 74 | 93 | 19 | 24 |

Each count is the same at all three widths, and the same with `-race`.

## Session benchmarks

The session benchmarks in [performance tests](../../../performance_test.go)
paint a session without regions to an outer output file. The child side is a
real PTY pair; no child process runs. Outer synchronized output is unsupported
in these sessions; the behavioral tests cover negotiated synchronization. The
repaint benchmarks write child output directly to the session's native terminal,
so they do not exercise the PTY read path, region painting or scrolling.

A changing-row benchmark changes one character of `\x1b[Hupdated` per
iteration, so each repaint has a visible change. The populated benchmark first
fills each row of a 120×40 child with 119 ASCII characters. Outer bytes/op is
the output written to the outer file per iteration. Warmed input delivery
queues 5 bytes, writes them to the PTY master and reads them from the slave.

```sh
scripts/with-libghostty-cppflags go test -run '^$' -bench 'BenchmarkSessionRepaint|BenchmarkPopulatedSessionRepaint|BenchmarkSessionInputQueue' -benchmem -count=3 .
```

| Session operation | Viewport | Time/op (indicative) | Outer bytes/op | Bytes/op | Allocations/op |
| :--- | :--- | ---: | ---: | ---: | ---: |
| Unchanged render | 20×6 | 166.9 ns | 0 | 0 | 0 |
| Unchanged capture and render | 20×6 | 5.460 µs | 0 | 2,408 | 74 |
| Changing-row capture and repaint | 20×6 | 10.49 µs | 9 | 2,504 | 89 |
| Populated changing-row capture and repaint | 120×40 | 23.06 µs | 9 | 2,504 | 89 |
| Warmed input delivery | 20×6 | 1.057 µs | — | 0 | 0 |

The allocation profile of a 20×6 changing-row repaint attributes 92% of
allocated objects to capture, through the same libghostty binding calls as the
capture benchmarks. Rendering accounts for 6%, mostly Ultraviolet's cursor
movement and the cursor-position sequences it builds with `x/ansi`:

```sh
scripts/with-libghostty-cppflags go test -run '^$' -bench 'BenchmarkSessionRepaint/changing_row' -benchmem -memprofile .tmp/session.mem -memprofilerate=1 -o .tmp/frame.test .
go tool pprof -sample_index=alloc_objects -top .tmp/frame.test .tmp/session.mem
```

These component measurements do not establish physical terminal latency.

### Native formatter comparison

The [native formatter benchmark](../../../internal/emulator/formatter_benchmark_test.go)
measures the same one-character child write plus libghostty's VT export of the
whole screen. It excludes placement, clearing, composition and terminal
delivery. For the populated 120×40 fixture it exports 4,838 bytes per frame:
8.200 µs/op with 4 allocations into a buffer, and 8.745 µs/op with 3 allocations
through a writer. The session's incremental repaint emits 9 bytes for the same
change.

```sh
scripts/with-libghostty-cppflags go test -run '^$' -bench 'BenchmarkGhosttyVTFormatter' -benchmem -count=3 ./internal/emulator
```

## Whole-repository gate

CI ([workflow](../../../.github/workflows/ci.yml)) runs `just check` and then
`just linkage` on `ubuntu-latest` (Linux x86_64) and `macos-latest` (macOS
arm64). `just check` runs `go mod tidy -diff`, the native archive build,
`go build ./...`, the CLI build to `bin/tui-frame`, `go vet ./...` and
`go test -race ./...`. Both jobs passed at `f674572` (CI run 37510976188),
so the race suite also ran on Linux x86_64.

The same gate passes locally. The race suite's package results follow; the
elapsed times are indicative, with load averages of 4.9–5.7 over 1 minute
during the run:

```text
ok  	github.com/alexgorbatchev/go-tui-frame	64.633s
ok  	github.com/alexgorbatchev/go-tui-frame/cmd/tui-frame	27.497s
ok  	github.com/alexgorbatchev/go-tui-frame/internal/emulator	1.401s
ok  	github.com/alexgorbatchev/go-tui-frame/internal/input	2.391s
ok  	github.com/alexgorbatchev/go-tui-frame/internal/process	18.578s
```

The CLI's statement coverage is 93.5%, with or without `-race`.
[CLI maintenance](../../../cmd/tui-frame/AGENTS.md) requires 90%:

```sh
scripts/with-libghostty-cppflags go test -coverprofile=.tmp/cli-coverage.out ./cmd/tui-frame
go tool cover -func=.tmp/cli-coverage.out | tail -1
```

## Behavior verified by retained tests

### Packed native cell capture

Capture decodes each damaged row's copied packed cells (`CellsRaw`) with the
bit layout from the linked library's type manifest.
[Cell layout decoding](../../../internal/emulator/cell_layout.go) parses the
manifest once per process and validates its schema, cell storage, field types,
bit ranges and content-union arms. Bit positions come from the manifest rather
than fixed offsets. The supported contract is documented in Ghostty's pinned
[type API](https://github.com/ghostty-org/ghostty/blob/33da6848d63b3bba2b4f31ab1531d618f2795192/include/ghostty/vt/types.h)
and [screen API](https://github.com/ghostty-org/ghostty/blob/33da6848d63b3bba2b4f31ab1531d618f2795192/include/ghostty/vt/screen.h).

Codepoints, width, style IDs, hyperlink flags and the backgrounds of erased
cells come from that decode, without a native getter per cell. Multi-codepoint
graphemes, nondefault styles and hyperlink URIs use native getters.
[Style capture](../../../internal/emulator/cell_style.go) reads each style once
per captured row and keeps its map storage between rows. Style IDs are
page-local and can be reused after mutations, so cached entries are cleared for
each row.

[Row tests](../../../internal/emulator/row_test.go) compare captured content,
packed metadata, full styles, erased-cell backgrounds and hyperlinks with
libghostty's own getters. They verify that plain text written over a captured
hyperlink leaves no link in the reused cell storage, and they reject
unsupported manifests.
[Color tests](../../../internal/emulator/colors_test.go) verify that RGB and
palette backgrounds survive erase-line and erase-display, and that a later erase
with default attributes restores the default background. In
[repaint tests](../../../repaint_test.go), child output that erases with a
background color arrives through a real PTY. Replaying the repaint on a native
outer terminal shows that background on the erased rows without overwriting the
header.

### Damage, reuse and repaint

[Damage tests](../../../internal/emulator/damage_test.go) verify consumed native
dirty flags, owned snapshots, clean-row retention, scrolling, default-color
changes and resize. Capture copies only the rows that libghostty reports dirty.
A change to the default colors or palette marks every row dirty.

[Reuse tests](../../../internal/emulator/reuse_test.go) verify stable internal
viewport storage and damage accumulated across three captures. They also verify
that input-mode reads leave the display clean, that a plain row does not
allocate a style per cell, and that selection and full styles agree with
libghostty's getters.

[Repaint tests](../../../repaint_test.go) read most child output through a real
PTY. They verify:

- fragmented output coalesced under a fixed first repaint deadline;
- flushing while idle and before exit;
- region invalidations paced to one repaint per frame interval, and outer
  reads and released held input that write routed mode changes at once
  without painting pending child output;
- immediate protocol replies;
- child text and cursor changes that neither redraw regions nor resample
  processes, and metadata changes that redraw regions;
- scroll-only redraws that reuse a process and PTY sample younger than
  250 ms, while title changes and region invalidation resample;
- no process and PTY sample outside startup and resize without a configured
  region or a selected snapshot-carrying observation;
- negotiated outer synchronized output;
- restoration of an observed entry hold;
- palette colors on 256-color, 16-color and truecolor outer terminals.

[Performance tests](../../../performance_test.go) verify:

- capture deferred until repaint;
- immediate outer input-mode writes;
- reusable cleared region canvases;
- independent retained drawing snapshots;
- one shared cell grid per snapshot copy;
- zero allocations for unchanged rendering and warmed input delivery;
- partial-write compaction and input origins;
- composition limited to damaged rows;
- selected observation kinds and per-read owned snapshots;
- exit-only snapshot freshness;
- synchronized-update checkpoints.

Each drawing callback receives its own copy of the snapshot. The copy holds
one cell grid, which `Terminal.Cells` and `Terminal.Native.Cells` share, and
the observation budget weighs that grid once. Capture also allocates when the
geometry changes, for new graphemes and styles, and in native getter calls.

### Default foreground and background colors

The native terminal sets both default colors once, at construction, through its
[preference profile](../../../internal/emulator/profile.go). Reported host
colors seed the defaults when available. Otherwise the native render state's
fallbacks, a white foreground and a black background, are set explicitly.
Ghostty's render state copies the terminal's default colors only when both are
set, so leaving either unset would keep an OSC 10 or OSC 11 override from
reaching the rendered cells. The initialization uses the native setters
described in the
[pinned terminal color API](https://github.com/ghostty-org/ghostty/blob/33da6848d63b3bba2b4f31ab1531d618f2795192/include/ghostty/vt/terminal.h)
and adds no work to later captures or repaints.

[Color tests](../../../internal/emulator/colors_test.go) verify independent
OSC 10/11 overrides and OSC 110/111 resets. They also verify unchanged explicit
SGR colors, owned snapshots, damage on every row, synchronized render holds,
reverse colors, and native query replies that match the rendered colors. The
foreground test in [repaint tests](../../../repaint_test.go) replays the emitted
outer bytes on a native outer terminal. Existing text changes color and resets
without new text. Its capture-file renderer selects a truecolor profile
explicitly.

### Inherited terminal preferences

[Inheritance tests](../../../inheritance_test.go) run real sessions that inherit
the outer terminal's default colors, palette, cursor color and style, and modes.
They also cover palette encoding for the outer color profile, a child that
measures text with the session's width rule, the PTY line discipline, cursor
rendering and restoration, and disabled inheritance.
[Preference tests](../../../terminal_preferences_test.go) verify that reported
keyboard protocols seed the child's encoder. Capture keeps the default rendition
for default colors that match the host's (`uvStyle` in
[state capture](../../../internal/emulator/state.go)).

The [cursor mirror](../../../cursor.go) follows the child's cursor style and
color only for the attributes the outer terminal reported at entry. It writes
nothing while the child keeps the reported values, and resets the terminal's
default when the child returns to them. A cursor-style or color change repaints
in one synchronized update even when cells and cursor position are unchanged.
The appearance test in [performance tests](../../../performance_test.go) also
verifies that an unchanged inherited cursor produces no output and no
allocations.

### Drawing, README examples and dependencies

The public drawing target is `uv.Screen`. Regions draw into native
Ultraviolet screen buffers that measure grapheme width. The
[drawing tests](../../../drawing_test.go) exercise buffer drawing, grapheme
cells and pushed replacements. CLI drawing tests use native Ultraviolet buffers
and also accept an optional Lip Gloss canvas.

[README tests](../../../readme_test.go) build the README's showcase, Keyboard
Capture and plain-text programs against the module and native archive. They
run the Keyboard Capture program on a real PTY whose outer terminal is a
libghostty emulator. A shell script stands in for nvim on `PATH`, so the
documented source runs unchanged. Five cases check the exit status and
standard error. Ctrl+Q never reports the quit itself: a child that its SIGTERM
ends is reported as `signal: terminated`, and a child that exits 0 on SIGTERM
exits 0 with empty standard error. A Ctrl+Q that the frame holds behind a full
input queue until the child has exited with status 7 still reports
`exit status 7`. A child exit status without Ctrl+Q is reported, and a session
error after Ctrl+Q is reported. The core dependency
test resolves the production package graph with `go list -deps` and confirms
that Lip Gloss is absent; the CLI demo uses it.

### Sessions, routing and the CLI

| Retained test | Verified behavior |
| :--- | :--- |
| [Real framed session](../../../session_test.go) | Idle push rendering, child title, native SID/group/argv/session inventory, real PTY window size, settings and foreground group, observer-copy isolation, termios restoration and closed-controller rejection. Cancellation restores the terminal before observers drain, returns the cause and ends every owned process group. An observer panic or `runtime.Goexit` ends the session through shutdown, also during the shutdown drain. Error and cancellation cleanup finish while a detached process holds the slave, also when flow control has stopped child output. |
| [Wake pipe](../../../wake_test.go) | Native pipe wake, coalescing on a full pipe, and failure propagation to the event loop and cleanup. |
| [Native routing](../../../routing_test.go) | Keys reach the child as the outer terminal sent them, also while outer and child keyboard modes differ and for a key without a native key code; paste envelopes, focus, localized cell and pixel mouse reports, buttonless releases, capture gestures, and the routed-input bound that mouse re-encoding reaches. A mouse event the child's encoding cannot represent, such as a coordinate beyond 223 for an X10 child, is withheld from the child and reported as a `Routed` event with `Event.Error` set, and the session routes the input that follows. |
| [Native graphics profile](../../../internal/emulator/profile_test.go) | Disabled Kitty graphics does not return a positive capability reply before or after reset and alternate-screen switches. |
| [Native process ownership](../../../internal/process/snapshot_test.go), [groups](../../../internal/process/groups_test.go), [Darwin groups](../../../internal/process/groups_darwin_test.go), [Darwin argv](../../../internal/process/args_darwin_test.go) | Current native fields, runtime cwd and owned copies. Same-session inventory survives a reaped launch leader. Darwin group exit follows member states, and Darwin empty argv0 and environment alignment have dedicated native tests. |
| [Live border](../../../border_test.go) | Updates before and during Run, closed-session rejection, and the applied native grid and actual PTY size. Measured cell pixels are preserved when the outer winsize has only cells. |
| [Keyboard mode mirroring](../../../console_test.go) | With capture set, a native outer terminal runs the child's Kitty flags where it reported Kitty support and none otherwise, and the child's modifyOtherKeys mode where it reported one; restoration returns its entry modes. A child's Kitty keyboard query gets no reply until the terminal reports Kitty support, also after the probe, while DA1 and DECRQM replies in the same output arrive. A Kitty reply after the probe leaves the flags the session found in place. |
| [Startup probe end](../../../console_test.go) | Against a native outer terminal that answers DA1 but not the modifyOtherKeys query, the startup probe ends at the DA1 reply, well before its deadline, and keeps the unanswered query pending. An alternate-screen report sent after the DA1 reply still starts the session: the probe waits for it. A DA1 reply that arrives after the deadline is consumed instead of reaching the child, and a DA1 report with no pending query stays input. |
| [Mouse mode mirroring](../../../console_test.go) | A native outer terminal runs the child's active tracking mode and an SGR or SGR-pixel report format, and still reports SGR after the child sets and then resets SGR-pixel reports. The tracking mode and report format the terminal had at startup stay active after the session enters and after it restores the terminal, also when the terminal reports modes that a later set replaced. |
| [Example CLI](../../../cmd/tui-frame/session_test.go) | Real-PTY Ctrl+B prefix with 1 for layout, 2 for independent backgrounds and 3 for the child border with actual PTY resize, in legacy and Kitty-with-release-events form: releases and repeats between the keys, a forwarded second Ctrl+B, a discarded other key, Ctrl+Q after the prefix, ordinary digits and unmatched keys, and termios restoration. Other CLI tests cover Ctrl+Q, shutdown failures after Ctrl+Q, complete argv, exit and signal outcomes, the embedded guide and help, and [`--showcase`](../../../cmd/tui-frame/showcase_test.go) sessions surviving a child output burst while observation delivery is held. |

Other root-package tests cover capture dispositions (`capture_test.go`),
composition (`composition_test.go`), layout and invalidation
(`frame_test.go`), outer input queuing (`input_queue_test.go` and
`input_queue_darwin_test.go`), observation delivery (`observation_test.go`),
resize, including one during startup (`resize_test.go`), configuration
validation (`validation_test.go`), snapshot metadata (`metadata_test.go`) and
Darwin process-group signalling (`signal_darwin_test.go`). The
`internal/emulator`, `internal/input` and `internal/process` packages have
further unit tests.

## Artifact audits

[Standalone-binary tests](../../../cmd/tui-frame/linkage_test.go) inspect the
executable's import table and, for ELF, its program headers. All three
artifacts pass at `f674572`:

| Artifact | Build and audit | Result |
| :--- | :--- | :--- |
| macOS arm64 Mach-O | `just build`, then `just linkage`; locally and on `macos-latest` | Imports only `/usr/lib/libresolv.9.dylib` and `/usr/lib/libSystem.B.dylib`; no separately installed libghostty. |
| Linux amd64 ELF | `just build-linux amd64`, then `just linkage bin/tui-frame-linux-amd64`, cross-built on macOS; `just build` and `just linkage` on `ubuntu-latest` | Statically linked; imported libraries `[]`, no `PT_INTERP`. |
| Linux arm64 ELF | `just build-linux arm64`, then `just linkage bin/tui-frame-linux-arm64`, cross-built on macOS | Statically linked; imported libraries `[]`, no `PT_INTERP`. |

The CLI tests also build the host executable and verify that its `skill`
command prints the embedded guide unchanged, from a directory without
repository files.

Go's build cache key for a cgo package includes the cgo flags from the
environment and `#cgo` directives, but not `pkg-config` output (`buildActionID`
in Go 1.27.1's `cmd/go/internal/work/exec.go`). The recipes add the prefix's
include flags and the archive's SHA-256 to `CGO_CPPFLAGS`, so a cached
libghostty package or link is reused only for the same native prefix and
archive; see the [pinned native build](native-build.md). In
[the CLI tests](../../../cmd/tui-frame/native_cache_test.go), the
`TestBuildCache` subtests share one build cache. `KeepsNativePrefixesApart`
builds against two prefixes and removes the first before building the second.
`FollowsNativeArchiveContents` changes the archive in place and requires the
rebuilt executable to get a new build ID. Both audited cross-builds used an
empty `GOCACHE`, so each linked this checkout's archive.

These audits follow the [pinned native build](native-build.md). Source changes
require rebuilding and reauditing the resulting binaries. Linux x86_64 tests
run in CI; Linux arm64 executables are only cross-built and inspected, and no
Linux arm64 runtime test runs. This record does not establish a consumer
`go get` of the published module.

## Fidelity boundaries

Passing tests do not establish arbitrary-TUI transparency. The current profile
does not render graphics or overline, expose complete screen, scrollback,
margin or damage state, retain all hyperlink parameters, or guarantee that a
gesture closes after an outside release. Mouse events beyond native encoding
limits or without the pixel precision the child requested are withheld from
the child and reported to observers. OS observations are sampled and
permission-limited; descendants in detached sessions and wrapper
suspend/resume are teardown boundaries. See the
[consumer profile](../../../README.md) and
[architecture research](../../../reports/TUI%20frame%20architecture%20research.md).
