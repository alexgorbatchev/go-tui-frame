---
created_on: 2026-10-01 20:10
last_modified: 2026-10-07 12:00
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
with other work during the benchmark runs: load averages were 4.5–4.8 over
1 minute and 6.5–6.9 over 5 minutes. Each table reports the median of three
one-second runs. Compare a timing only with a run on the same machine under
similar load.

## Capture benchmarks

`BenchmarkStateCapture` in [damage tests](../../../internal/emulator/damage_test.go)
and `BenchmarkBorrowedState` in [reuse tests](../../../internal/emulator/reuse_test.go)
capture a 120×40 native viewport with plain ASCII text. Owned captures
(`State`) copy storage for a durable caller. Internal captures (`UpdateState`)
reuse the viewport and its maps. A one-row capture first writes
`\x1b[Hupdated`. A full-viewport capture first erases the display and fills
all 40 rows with text, so it converts every cell.

Input modes only reads the values input routing uses (`InputState`), without
the display: the 13 modes routing and the outer input-mode mirror read, the
active screen, keyboard protocol flags and mouse tracking. It includes the
key- and mouse-encoder probes that derive `ModifyOtherKeys2` and
`MouseTrackingMode`. The terminal keeps these values until the next `Write`,
`Resize` or `ReleaseHold`, which are the only calls that change the native
terminal after it is created. Input modes only discards them before each
read, as a child read does; input modes, unchanged reads them without a
mutation in between and makes no native call. An internal capture takes these
values from `InputState` and reads the other 30 named modes itself, so an
unchanged internal capture reads no routing value again.

```sh
scripts/with-libghostty-cppflags go test -run '^$' -bench 'BenchmarkBorrowedState|BenchmarkStateCapture' -benchmem -count=3 ./internal/emulator
```

| Capture | Time/op (indicative) | Bytes/op | Allocations/op |
| :--- | ---: | ---: | ---: |
| Owned, unchanged | 160.6 µs | about 1,010,680 | 89 |
| Owned, one row | 133.1 µs | about 1,010,756 | 96 |
| Internal, unchanged | 2.905 µs | 2,200 | 54 |
| Internal, one row | 8.018 µs | 2,664 | 88 |
| Internal, full viewport | 167.4 µs | 4,848 | 322 |
| Input modes only | 1.099 µs | 400 | 25 |
| Input modes, unchanged | 166.1 ns | 0 | 0 |

These capture figures were re-measured on 2026-10-07 after `InputState`
began reading only the routing modes and keeping its values until the next
mutation. Before that change, on the same machine, input modes only made 55
allocations (520 B) and an unchanged internal capture 79 (2,600 B). Every
capture that follows a write still reads each value once, so the one-row,
full-viewport and owned figures are unchanged.

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
[row tests](../../../internal/emulator/row_test.go) logs the allocations of a write and an
internal capture, first for a write that damages no row and then for one that
damages a row. Both writes make the capture read the input values again, so
the difference is the damaged row's cost. The undamaged write resets
cursor-key mode, which is already reset. The test runs at 20, 120 and 240
columns. The erased rows alternate two backgrounds, so each iteration damages
the row:

```sh
scripts/with-libghostty-cppflags go test -count=1 -v -run 'TestRowCaptureAllocationsDoNotScaleWithWidth' ./internal/emulator
```

| Damaged row | Undamaged | Damaged | Additional | Test limit |
| :--- | ---: | ---: | ---: | ---: |
| Plain text (`updated`) | 83 | 89 | 6 | 12 |
| Two styles across the full width | 83 | 93 | 10 | 24 |
| Erased with a palette background (`48;5;33`, `48;5;34`) | 83 | 89 | 6 | 12 |
| Erased with an RGB background (`48;2;10;20;30`, `48;2;40;50;60`) | 83 | 90 | 7 | 12 |

Each count is the same at all three widths, and the same with `-race`. These
counts were measured on 2026-10-07. Capture converts each palette entry and
each default color once after the render colors change, and reuses the most
recent direct RGB color, so erased cells add no allocation per column. The RGB
row adds one allocation because each iteration changes the color.

`TestRowCaptureDoesNotReconvertRepeatedStyles` in the same file measures how
many allocations each further damaged row adds, from one to four 40-column
rows that repeat the same styled runs. Native style IDs belong to the source
page, so each dirty row reads each of its styles once from libghostty, and
`RenderStateRowCells.Style` allocates twice per read. Each row converts each
of its styles once, and the default style is converted once per change of the
render colors:

```sh
scripts/with-libghostty-cppflags go test -count=1 -v -run 'TestRowCaptureDoesNotReconvertRepeatedStyles' ./internal/emulator
```

| Runs in each row | Styles | Per further row | Test limit |
| :--- | ---: | ---: | ---: |
| Plain text | 0 | 6 | — |
| One palette style (`38;5;33`) | 1 | 8 | 8 |
| One RGB style (`38;2;11;22;33`) | 1 | 8 | 8 |
| Two styles around plain text (`1;38;5;33`, `4;38;2;11;22;33`) | 2 | 10 | 10 |

The limit is the plain row's count plus two allocations per style. These
counts were measured on 2026-10-07 and are the same with `-race`. A row with
two or more direct RGB colors converts each of them again on every row,
because capture keeps only the most recent RGB color.

### Styled captures

`BenchmarkStyledCapture` in [damage tests](../../../internal/emulator/damage_test.go)
times an internal capture (`UpdateState`) after a write that damages every
row; the write is not timed. The styles stay the same across iterations and
only the text alternates. Few styles writes three runs on each row: bold
palette, plain and underlined RGB. Style per cell gives every cell its own
direct RGB color, as gradients and image-to-ANSI output do. A row finds its
styles by scanning up to 16 of them and through an ID index past that, so a
row of distinct styles costs time linear in its width:

```sh
scripts/with-libghostty-cppflags go test -run '^$' -bench 'BenchmarkStyledCapture' -benchmem -count=3 ./internal/emulator
```

| Viewport | Time/op (indicative) | Bytes/op | Allocations/op |
| :--- | ---: | ---: | ---: |
| Few styles, 120×40 | 144.6 µs | 17,648 | 479 |
| Style per cell, 40×24 | 189.3 µs | 161,393 | 3,103 |
| Style per cell, 200×24 | 898.1 µs | 791,159 | 14,623 |
| Style per cell, 400×2 | 153.6 µs | 133,920 | 2,491 |
| Style per cell, 1000×1 | 191.8 µs | 166,664 | 3,085 |

These figures were measured on 2026-10-07 with load averages of 6.4–7.7 over
1 minute, directly after the same benchmark on the previous capture code
under the same load. That code converted the default style again on every
row and kept the row's styles in a map it cleared on every row; its medians
were 233.5, 212.3, 1,017.6, 182.5 and 228.5 µs in the order above, with the
same bytes and allocations.

### Synchronized-update captures

`BenchmarkSynchronizedFrames` in
[render-hold tests](../../../internal/emulator/render_hold_test.go) writes two
frames of 30 rows × 100 columns into a 120×40 terminal per `Write`, without
reading the state. The synchronized run wraps each frame in DEC 2026
(`\x1b[?2026h` … `\x1b[?2026l`), so every write begins two render holds and
completes a frame that no read displays.

```sh
scripts/with-libghostty-cppflags go test -run '^$' -bench 'BenchmarkSynchronizedFrames' -benchmem -count=3 ./internal/emulator
```

| Frames per `Write` | Time/op (indicative) | Bytes/op | Allocations/op |
| :--- | ---: | ---: | ---: |
| Unsynchronized | 3.621 µs | 6 | 3 |
| Synchronized | 12.22 µs | about 7,760 | 252 |

These figures were measured on 2026-10-07, with load averages of 4.3–4.4 over
1 minute and 5.7–5.8 over 5 minutes. A hold's beginning updates the native
render state, which preserves the last complete frame, and reads the
hyperlink flag of each row a conversion would read. It converts no cell. The
libghostty row getters make nearly all the allocations, and the synchronized
byte count varies by a few bytes/op between runs. The held frame is converted
when a read of the state during the hold displays it. A frame whose hold ends
first is never converted: the next capture converts the rows of every update
since the last conversion once.

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
| Unchanged render | 20×6 | 189.1 ns | 0 | 0 | 0 |
| Unchanged capture and render | 20×6 | 4.766 µs | 0 | 2,200 | 54 |
| Changing-row capture and repaint | 20×6 | 11.08 µs | 9 | 2,696 | 94 |
| Populated changing-row capture and repaint | 120×40 | 17.50 µs | 9 | 2,696 | 94 |
| Warmed input delivery | 20×6 | 1.055 µs | — | 0 | 0 |

These session figures were re-measured on 2026-10-07 together with the capture
benchmarks. Before `InputState` kept its values between mutations, a run on
the same machine reported 79 allocations (2,600 B) for an unchanged capture and render and
94 for both changing-row repaints.

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

Each job restores `.tmp/native/ghostty`, the installed prefix, the Zig cache
and `.tmp/native/zig-global-cache` with
[`actions/cache`](https://github.com/actions/cache) before `just check`. A
change to any part of the key misses the cache and runs a full native build:

| Key part | Why it is in the key |
| --- | --- |
| Runner system and architecture | Names the job's platform. |
| Zig binary's SHA-256 (`matrix.zig_sha`) | Zig compiled every cached file. |
| Hash of `scripts/native.sh` | Covers the Ghostty revision, archive SHA-256, Zig version and `zig build` flags. |
| Root, target, prefix and cache names that the justfile passes to the script | Choose the build's target and paths. Zig's cache manifests record absolute file paths, so another root rebuilds. |
| Host target that `zig targets` reports: CPU model, features and OS version | Ghostty's Unicode table generators compile for the host, and Zig's cache digests include the target and the paths of generated files. |

The archive's code and data do not depend on the host. On macOS, Ghostty's
build replaces `native` with a generic macOS 13.0 target (`genericMacOSTarget`
in `src/build/Config.zig`); building with `-Dtarget=aarch64-macos` produced a
byte-identical `libghostty-vt.a`, and its objects record `minos 13.0`. The
Linux target is an explicit musl target with a baseline CPU, but there the
archive is identical only after `objcopy --strip-debug`. Its DWARF strings
name the Zig cache directory that holds the generated Unicode tables, and
that directory differs between cold builds even on one host. In a
`debian:bookworm` container, three cold `x86_64-linux-musl` builds, two on one
host and one under `setarch --uname-2.6`, gave three different archive
digests. Only `libghostty-vt-static_zcu.o` differed, in 29 or 60 bytes of cache
paths, and the three stripped objects were identical.

The host target stays in the key because the build cannot pin it. Zig 0.16.0's
build runner always sets `b.graph.host` from native detection
(`lib/compiler/build_runner.zig`), `zig build` has no host-target option, and
Ghostty compiles its Unicode table generators for `b.graph.host`
(`src/build/UnicodeTables.zig`). Zig's cache hashes each input file's path as
well as its contents (`addFileInner` in `std/Build/Cache.zig`), so a host
change gives the generators new cache digests, their identical tables a new
directory, and the library a new compile digest. In the same container, a
cache restored onto another host rebuilt in 42.3 s with 33 new cache objects,
against 46.3 s for a cold build with the Zig packages already fetched and
0.6 s for a rebuild on the same host. Each `ubuntu-latest` CPU model, and each
runner image with a new kernel or macOS version, therefore costs one full
native build and saves one entry of about 160 MiB. After every host a job
lands on has saved its entry, runs hit while that entry stays in the cache.
GitHub removes entries not accessed for 7 days. It also limits a repository's
caches to 10 GB by default, and once that is reached it evicts entries in
order of last access, so with many hosts an entry can go before a week has
passed. The step that resolves the key prints the host block, so a miss shows
which host field changed.

The workflow sets no `restore-keys`. A restore key that ends at `-inputs-`
would match an entry with the same `scripts/native.sh`, and so the same
Ghostty revision, from another host. That restore would save only the archive
download and the Zig package fetch, since the library still rebuilds. The
entry saved after it would hold both hosts' cache objects, and each later
host would add its own. `actions/cache` saves an entry only after
a successful job and never replaces an existing one. On a hit, Zig re-hashes
the restored files, whose inodes changed, and reuses its cached outputs when
their contents match. Locally, a `.tmp/native` restored from a tar of those
four directories, without the archive, rebuilt in under two seconds with no
download and produced the same `libghostty-vt.a`. On CI, four runs have
reached the restore step: 37660508704 (`608a3b7`), 37663431480 (`11a6de0`),
37665156959 (`666f07e`) and 37667067041 (`e9fc4fa`). The macOS key was the
same in all four. The first run missed and failed before saving, the second
saved the entry, and the third and fourth hit it without downloading the
archive. Each of the four Linux runs resolved a distinct `inputs` digest, so
every one missed; the first failed before saving and the other three each
saved an entry. The host block is the only part of that digest that varies
between runs, but those runs did not print it, so the host is the inferred
cause. The Linux hit rate is still to be confirmed on real CI.

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
with default attributes restores the default background. They also verify
that erased cells follow an OSC 4 change and an OSC 104 reset of their
palette entry, and that a capture failing after a color change leaves no
stale converted colors once the colors change back. In
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

[Render-hold tests](../../../internal/emulator/render_hold_test.go) verify that
a write of several synchronized frames converts none of them, and that the
next read converts only the frame it shows: the last frame, or the last
complete frame while a hold is open. They also verify that a held frame keeps
its text and hyperlink URLs when later bytes of the same write replace them,
including linked rows that a default-color change makes a conversion read
again. Conversion reads URLs from the live terminal, so a hold whose rows may
contain a hyperlink converts them when it begins.

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
geometry changes, for new graphemes and styles, for the first use of each color
after the render colors change, and in native getter calls.

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
| [Real framed session](../../../session_test.go) | Idle push rendering, child title, native SID/group/argv/session inventory, real PTY window size, settings and foreground group, observer-copy isolation, termios restoration and closed-controller rejection. Cancellation restores the terminal before observers drain, returns the cause and ends every owned process group. Termination enumerates the host processes once and sends SIGTERM and SIGCONT to every group it found, so a stopped group exits. An observer panic or `runtime.Goexit` ends the session through shutdown, also during the shutdown drain. Error and cancellation cleanup finish while a detached process holds the slave, also when flow control has stopped child output. |
| [Wake pipe](../../../wake_test.go) | Native pipe wake, coalescing on a full pipe, and failure propagation to the event loop and cleanup. |
| [Native routing](../../../routing_test.go) | Keys reach the child as the outer terminal sent them, also while outer and child keyboard modes differ and for a key without a native key code; paste envelopes, focus, localized cell and pixel mouse reports, buttonless releases, capture gestures, and the routed-input bound that mouse re-encoding reaches. A mouse event the child's encoding cannot represent, such as a coordinate beyond 223 for an X10 child, is withheld from the child and reported as a `Routed` event with `Event.Error` set, and the session routes the input that follows. |
| [Native graphics profile](../../../internal/emulator/profile_test.go) | Disabled Kitty graphics does not return a positive capability reply before or after reset and alternate-screen switches. |
| [Native process ownership](../../../internal/process/snapshot_test.go), [groups](../../../internal/process/groups_test.go), [Darwin groups](../../../internal/process/groups_darwin_test.go), [Darwin argv](../../../internal/process/args_darwin_test.go) | Current native fields, runtime cwd and owned copies. Same-session inventory survives a reaped launch leader, and `List` and `Groups` report the same groups. Darwin group exit follows member states, and Darwin empty argv0 and environment alignment have dedicated native tests. |
| [Live border](../../../border_test.go) | Updates before and during Run, closed-session rejection, and the applied native grid and actual PTY size. Measured cell pixels are preserved when the outer winsize has only cells. |
| [Keyboard mode mirroring](../../../console_test.go) | With capture set, a native outer terminal runs the child's Kitty flags where it reported Kitty support and none otherwise, and the child's modifyOtherKeys mode where it reported one; restoration returns its entry modes. A child's Kitty keyboard query gets no reply until the terminal reports Kitty support, also after the probe, while DA1 and DECRQM replies in the same output arrive. A Kitty reply after the probe leaves the flags the session found in place. |
| [Startup probe end](../../../console_test.go) | Against a native outer terminal that answers DA1 but not the modifyOtherKeys query, the startup probe ends at the DA1 reply, well before its deadline, and keeps the unanswered query pending. An alternate-screen report sent after the DA1 reply still starts the session: the probe waits for it. A DA1 reply that arrives after the deadline is consumed instead of reaching the child, and a DA1 report with no pending query stays input. |
| [Mouse mode mirroring](../../../console_test.go) | A native outer terminal runs the child's active tracking mode and an SGR or SGR-pixel report format, and still reports SGR after the child sets and then resets SGR-pixel reports. The tracking mode and report format the terminal had at startup stay active after the session enters and after it restores the terminal, also when the terminal reports modes that a later set replaced. |
| [Example CLI](../../../cmd/tui-frame/session_test.go) | Real-PTY Ctrl+B prefix with 1 for layout, 2 for independent backgrounds and 3 for the child border with actual PTY resize, in legacy and Kitty-with-release-events form: releases and repeats between the keys, a forwarded second Ctrl+B, a discarded other key, Ctrl+Q after the prefix, ordinary digits and unmatched keys, and termios restoration. Other CLI tests cover Ctrl+Q, shutdown failures after Ctrl+Q, complete argv, exit and signal outcomes, the embedded guide and help, and [`--showcase`](../../../cmd/tui-frame/showcase_test.go) sessions surviving a child output burst while observation delivery is held. |

Other root-package tests cover capture dispositions (`capture_test.go`),
composition (`composition_test.go`), layout and invalidation
(`frame_test.go`), outer input queuing (`input_queue_test.go` and
`input_queue_darwin_test.go`), observation delivery (`observation_test.go`),
resize, including one during startup and the outer composition buffer kept
through border, cell-size and outer size changes (`resize_test.go`), configuration
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
