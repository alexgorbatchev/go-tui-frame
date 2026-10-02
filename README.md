`go-tui-frame` surrounds one external TUI application with information supplied by its Go host. Add a header, footer, border, or side region around an existing interactive tool, then update that information directly from application events. Child input passes through by default, with no pane management or built-in keyboard shortcuts.

# What It Does

- **One child TUI:** Launch an external executable with a real terminal endpoint and reserve the remaining display for consumer-supplied information.
- **Native Lip Gloss canvases:** Style and compose each region with Lip Gloss v2, using its allocated cell dimensions, layers, colors, padding, and alignment.
- **Declarative configuration:** Chain command, frame regions, observation, and optional capture declarations, then execute with `Run(ctx)`.
- **Default input forwarding:** Deliver uncaptured keyboard, paste, and focus input to the child without a prefix key or built-in quit shortcut. Route mouse input using the child's viewport and requested protocol.
- **Rich observation:** Expose transport bytes, screen state, terminal modes, protocol requests, process lifecycle, and available operating-system metadata with their sources and availability.
- **Explicit capture:** Let the consumer consume selected keyboard input while forwarding unmatched input. Observation alone does not consume input.
- **Push updates:** Submit typed application data with `InvalidateHeader`, `InvalidateFooter`, or the side-region equivalents, including while the child produces no output.

# How It Works

1. Supply a child command and declare the regions surrounding it. Frame content can use a child snapshot and the consumer's own application data.
2. Call `Run(ctx)`. The library validates static configuration, acquires the terminal, verifies required capabilities, and starts the child with the dimensions left by the frame. Failed acquisition or verification restores acquired terminal state.
3. Interact with the child normally. The frame remains visible while the child updates its own display; the host receives observations and can request a frame refresh.
4. Wait for the child to exit or cancel the context. The library drains output, releases its resources, restores the terminal state it owns, and returns the child result plus any operational error.

# How it Really Works

1. **The child sees its own terminal.** Its size excludes the frame. Full-screen erase, cursor movement, resets, and alternate-screen changes affect its virtual display. Sending its output directly to the outer terminal cannot reliably contain those operations. The frame therefore requires terminal emulation and composition. [PTY semantics](https://man7.org/linux/man-pages/man7/pty.7.html), [terminal controls](https://invisible-island.net/xterm/ctlseqs/ctlseqs.html).
2. **Forwarding respects terminal semantics.** Uncaptured keyboard and paste bytes remain unchanged when the outer and child input protocols match. The outer terminal uses raw input; the child retains its own line discipline, so a forwarded Ctrl-C can become a native foreground-process-group signal. There is no automatic host-side Ctrl-C, Escape, or Ctrl-Z binding. Modern keyboard modes still require negotiation. [termios](https://man7.org/linux/man-pages/man3/termios.3.html), [kitty keyboard protocol](https://sw.kovidgoyal.net/kitty/keyboard-protocol/).
3. **Mouse delivery requires geometry.** Mouse input targeting the child uses child-local coordinates while retaining its button, modifiers, and action. Pixel reports require actual pixel geometry. A frame position has no corresponding child cell, so the library does not fabricate an edge coordinate. Byte-identical forwarding of every mouse report would give the child incorrect coordinates. [mouse protocols](https://invisible-island.net/xterm/ctlseqs/ctlseqs.html).
4. **Queries describe the child endpoint.** Cursor, size, mode, and capability queries receive answers consistent with the virtual terminal. Generated replies are distinguishable from user input. The child must not inherit capability claims that the library cannot deliver. [terminfo](https://invisible-island.net/ncurses/man/terminfo.5.html), [terminal queries](https://invisible-island.net/xterm/ctlseqs/ctlseqs.html).
5. **The session owns terminal I/O.** `Run` blocks for one session. Construction and chained declarations have no terminal side effects. Frame callbacks draw into region-local buffers instead of writing to the terminal. Snapshots contain owned data, and consumer mutation cannot alter the live emulator. Only one session owns a particular outer terminal at a time.
6. **Observation has explicit costs.** Byte observations preserve the terminal transport, not application write boundaries. A shared child terminal merges stdout, stderr, and other writers. The active viewport is copied for owned snapshots; scrollback counts do not expose the complete retained history. Process observations have platform, permission, and identity limits. [stream reads](https://man7.org/linux/man-pages/man2/read.2.html), [process metadata](https://man7.org/linux/man-pages/man5/proc_pid_stat.5.html).

# Prerequisites

- Go 1.27.1 or newer, as declared in [go.mod](go.mod).
- Linux or macOS with an interactive outer terminal.
- The executable child TUI you want to wrap.
- For building a consumer: CGO enabled, a C compiler, `pkg-config`, and the pinned libghostty headers/static archive. Building that native archive requires Zig 0.16.0.

The terminal emulator links into the consumer binary statically. Running the binary needs no separate libghostty installation, Ghostty application, or multiplexer server; the operating system, outer terminal, and child executable remain required. The binding selects `libghostty-vt-static` through `pkg-config`, so its archive must be available at build time. [Static binding](https://github.com/mitchellh/go-libghostty/blob/76867c77a212/cgo_static.go), [native build requirement](https://github.com/ghostty-org/ghostty/blob/33da6848d63b3bba2b4f31ab1531d618f2795192/build.zig.zon).

Follow the [native build setup](docs/internal/references/native-build.md) when building a consumer executable. macOS executables use OS-provided system libraries; Linux builds use a static musl link.

# Installation

```sh
go get github.com/alexgorbatchev/go-tui-frame
```

# Quick Start

Draw a three-row red header with live application status, and a footer showing the child's title and viewport dimensions. Import `github.com/alexgorbatchev/go-tui-frame` as `frame` and `charm.land/lipgloss/v2` as `lipgloss`; `fmt` and `os/exec` are standard-library packages, and `ctx` is your application's `context.Context`.

```go
type UIData struct{ Status string }

banner := lipgloss.NewStyle().
	Background(lipgloss.Color("#B91C1C")).
	Foreground(lipgloss.Color("#FFFFFF")).Bold(true).Padding(0, 1)

app := frame.New(exec.Command("nvim", "--clean"), UIData{Status: "Indexing…"}).
	Header(3, func(ctx frame.DrawContext[UIData]) {
		if ctx.View.Bounds().Empty() {
			return
		}
		text := fmt.Sprintf("Editor · PID %d\n%s", ctx.Term.Child.PID, ctx.Data.Status)
		ctx.View.Compose(lipgloss.NewLayer(banner.
			Width(ctx.View.Width()).Height(ctx.View.Height()).
			MaxWidth(ctx.View.Width()).MaxHeight(ctx.View.Height()).Render(text)))
	}).
	Footer(1, func(ctx frame.DrawContext[UIData]) {
		if ctx.View.Bounds().Empty() {
			return
		}
		text := fmt.Sprintf("%s · %d×%d",
			ctx.Term.Terminal.Title, ctx.Term.Viewport.Cols, ctx.Term.Viewport.Rows)
		ctx.View.Compose(lipgloss.NewLayer(lipgloss.NewStyle().
			Foreground(lipgloss.Color("#9CA3AF")).Padding(0, 1).
			Width(ctx.View.Width()).Height(ctx.View.Height()).
			MaxWidth(ctx.View.Width()).MaxHeight(ctx.View.Height()).Render(text)))
	}).
	Border(true)

result, err := app.Run(ctx)
```

From the wrapper's existing indexing-completion event handler, independently of the blocking `Run` call:

```go
err := app.InvalidateHeader(UIData{Status: "Index complete"})
```

The `app` controller exists before the blocking `Run` call, so event handlers can submit updates during the session. `InvalidateHeader` replaces only that region's complete application-data payload and schedules drawing; handle its returned error in your event handler. Submissions are safe from any goroutine and coalesce to the latest pending payload. They do not write to the terminal or wait for rendering. Normal rendering emits only changed cells. There is no default polling/redraw loop, and you do not need watchers, locks, or invalidation channels.

`Run` blocks until the session completes. Its result contains `ProcessState *os.ProcessState` after a successful start and wait, along with output-drain and cleanup outcomes. A nonzero child exit belongs in this result; configuration, startup, transport, observation, or restoration failures produce an operational `error`. Handle both the returned result and error.

Supply a standard `exec.Cmd` to configure `Dir`, `Env`, and other compatible launch settings. `exec.Command` does not interpret a shell command string. [Go command semantics](https://pkg.go.dev/os/exec#Cmd).

# API

| Entry point or method | Signature | Consumer contract |
| :--- | :--- | :--- |
| `New` | `New[T any](cmd *exec.Cmd, initial T) *Frame[T]` | Create the configuration and live controller for one unstarted command. |
| `Header`, `Footer` | `(rows int, draw func(DrawContext[T])) *Frame[T]` | Reserve rows; receive one typed drawing context. |
| `Left`, `Right` | `(cols int, draw func(DrawContext[T])) *Frame[T]` | Reserve columns with the same callback contract. |
| `Border` | `(enabled bool) *Frame[T]` | Configure the initial one-cell border around the child viewport. |
| `SetBorder` | `(enabled bool) error` | Submit a concurrent border change before or during `Run`. |
| `Terminal` | `(input, output *os.File) *Frame[T]` | Borrow outer terminal files; default to `os.Stdin` and `os.Stdout`. |
| `InvalidateHeader`, `InvalidateFooter` | `(data T) error` | Replace the selected region's data and schedule drawing. |
| `InvalidateLeft`, `InvalidateRight` | `(data T) error` | Replace the selected side region's data and schedule drawing. |
| `Observe` | `(handler func(Event)) *Frame[T]` | Receive owned input/output, terminal, and process observations without consuming input. |
| `Capture` | `(handler func(Input) Disposition) *Frame[T]` | Explicitly return `Pass` or `Consume` for keyboard input. |
| `Run` | `(ctx context.Context) (Result, error)` | Validate and execute one session; return after the defined drain and cleanup boundary. |

`New` infers `T` from the initial application data; the example produces a `*Frame[UIData]`. Configuration methods use the controller's existing type parameter, and drawing callbacks come last. `Snapshot`, `Event`, `Input`, and `Result` carry child state, observations, input-routing information, and session outcomes. Metadata carries provenance and availability; unknown values are not presented as established facts. [Go type inference](https://go.dev/ref/spec#Type_inference).

## Drawing a region

Each callback receives one value with this public structure:

```go
type DrawContext[T any] struct {
	Term Snapshot
	View *lipgloss.Canvas
	Data T
}
```

`Term` is an owned child snapshot separate from the live emulator. `View` is a borrowed native drawing target for this callback only. `Data` is this region's stable by-value application payload; referenced contents remain immutable.

`DrawContext[T]` is drawing data, not a `context.Context`. Inside the example's callbacks, `ctx` names this value; the outer `ctx` passed to `Run` is the application's cancellation context.

`View` is backed by terminal cells. Its `Width()`, `Height()`, and `Bounds()` describe the region's allocated cells. Coordinates start at the region's local origin, not at its outer-terminal position. The library supplies a cleared, region-sized canvas on each drawing pass.

Use native Lip Gloss styles for color, padding, borders, and alignment, then compose a `lipgloss.NewLayer` into the canvas. A style sized with the canvas's width and height fills the allocation, including the red background in the example. `MaxWidth` and `MaxHeight` bound arbitrary content; their zero values disable limits, so the empty-bounds guard is intentional. For positioned or overlapping layers, compose `lipgloss.NewCompositor(layers...)` so native layer offsets and stacking are applied. [Lip Gloss canvas](https://github.com/charmbracelet/lipgloss/blob/v2.0.6/canvas.go), [styles](https://github.com/charmbracelet/lipgloss/blob/v2.0.6/set.go), [layers](https://github.com/charmbracelet/lipgloss/blob/v2.0.6/layer.go).

The frame composes each local canvas inside its reserved rectangle, with display-cell and grapheme-aware clipping that preserves wide-cell invariants at edges. Drawing does not write raw escapes to the outer terminal, and native Lip Gloss composition does not require a Bubble Tea program. `View` is borrowed for the callback only: keep its allocated dimensions, do not retain it through a saved drawing context, and do not draw asynchronously after returning.

Use offscreen styling and composition in callbacks. Direct Lip Gloss `Print`/`Println` writes and standalone background-color queries bypass the session's terminal ownership. [Lip Gloss query transport](https://github.com/charmbracelet/lipgloss/blob/v2.0.6/terminal.go).

## Configuration and ownership

`Frame[T]` records configuration and acts as the live controller. Chained declarations have no terminal side effects; configuration methods are not safe to mutate concurrently and freeze when `Run` begins. Each configured region initially receives a by-value copy of the data supplied to `New`. An invalidation replaces only the named region's data: updating the header does not change the footer's payload. The same `T` is used by all regions, while each retains its own latest value.

Before terminal mutation, `Run` validates static requirements: non-nil/unstarted command, positive region dimensions, non-nil drawing callbacks, positive initial child viewport, and compatible command I/O and process attributes. Input and output must refer to the same interactive terminal, which can have only one frame owner in the process. The session then acquires raw input reversibly and probes capabilities before starting the child. Active query responses can require noncanonical input, so probing cannot always precede every mode change. Failed acquisition or capability verification restores acquired terminal state and returns an error. A frame executes one session and is not reusable after `Run`. [query replies](https://sw.kovidgoyal.net/kitty/keyboard-protocol/#progressive-enhancement), [canonical input](https://man7.org/linux/man-pages/man3/termios.3.html).

The session owns command start/wait, child terminal descriptors, and its I/O workers. Callers must not concurrently start, wait on, or modify an accepted command. Preassigned stdio or lifecycle settings that conflict with terminal/session ownership are rejected rather than silently replaced. The controller remains available to event handlers during execution; `Result` becomes available after the session ends. [Go `Cmd` ownership](https://pkg.go.dev/os/exec#Cmd).

`Terminal(input, output)` selects the outer terminal files before `Run`; both must be non-nil. The session borrows them, restores the terminal state it acquires, and leaves the caller's files open. The default files are `os.Stdin` and `os.Stdout`.

`InvalidateHeader`, `InvalidateFooter`, `InvalidateLeft`, and `InvalidateRight` are safe for concurrent submission before and during `Run`. Before execution, a successful call replaces that configured region's initial payload. During execution, it atomically publishes a complete payload and dirty revision; a callback receives one stable by-value payload. Pending submissions coalesce, retaining the latest accepted value for that region. An update racing startup or a drawing callback schedules the needed drawing pass without losing its wakeup. While the session remains active, the latest settled submission reaches the region.

Invalidating an absent region returns `ErrRegionNotConfigured`; once session shutdown begins, invalidations return `ErrSessionClosed`. Submission and shutdown are ordered, so an accepted update cannot refer to released session resources. Acceptance publishes data but does not acknowledge a completed paint; the session can close before that paint occurs. Invalidation never performs terminal I/O or waits for a renderer or callback.

Application data is copied by value, without a deep clone. Submit immutable snapshots, and keep referenced maps, slices, pointers, and their contents immutable while the frame can use them. The example's struct of strings needs no caller-managed synchronization. `DrawContext.Term` is owned separately from `DrawContext.Data`; grouping them in one value does not make them a joint atomic transaction. [Go assignment semantics](https://go.dev/ref/spec#Assignment_statements).

Region reservations are fixed by their declarations: headers and footers reserve the specified rows, and side regions reserve the specified columns. Content is clipped to its region at terminal-cell boundaries, so changing text does not resize the child or paint outside that region. Outer resize recomputes the child viewport and canvas dimensions from those reservations.

Use `Border(enabled)` for initial configuration and `SetBorder(enabled)` for a live change. `SetBorder` is safe concurrently before and during `Run`; for example, `app.SetBorder(false)` requests removal of the child border. Requests coalesce to the latest pending value and return without terminal I/O or waiting for drawing. The session applies the border inset, resizes the child terminal and PTY, and updates input coordinates in its owned path. Border-only changes retain measured cell pixels; region reservations stay unchanged. An accepted request can produce `ErrViewportTooSmall` as a session error when applied, just as an outer resize can. After shutdown begins, `SetBorder` returns `ErrSessionClosed`; a successful submission does not acknowledge a completed paint.

Drawing callbacks use the canvas in the session's serialized rendering path, outside state locks, and must return promptly. Terminal controls within display text do not become arbitrary outer-terminal operations. Prefer the supplied payload over reading mutable application state directly; synchronize any foreign shared state that a callback does read.

Initial drawing, explicit region invalidation, relevant child-state changes, and resize schedule rendering. Each invalidation requests drawing even if its arbitrary `T` payload appears unchanged; the library does not require comparability or perform a promised deep-equality check. Normal output uses a cell diff, so identical rendered content does not repaint. Frame content has no default polling or periodic resnapshot timer; animation requires explicit consumer scheduling.

Passive `Observe` callbacks run on a separate ordered dispatcher and receive durable copies. The queue holds up to 64 waiting events plus one executing callback, with a 16 MiB weighted metadata budget covering both. Overflow reports `ErrObservationOverflow` as a session error rather than silently dropping bytes or waiting for a slow callback. These limits cover library admissions; consumers remain responsible for memory they retain or allocate. Shutdown restores the terminal, drains admitted events, and joins the dispatcher. Callbacks must return promptly and honor session cancellation through the caller's context; a Go function that never returns cannot be forcibly canceled.

## Optional keyboard capture

There is no capture handler by default. If the consumer declares one, it receives recognized keyboard events with this structure; `uv` is `github.com/charmbracelet/ultraviolet`:

```go
type Input struct {
	Raw []byte
	Key uv.KeyEvent
}
```

Explicit capture requests distinct key reports when the outer terminal supports them: verified Kitty support retains escape-code disambiguation even for a legacy child; otherwise, verified modifyOtherKeys support enables mode 2. Uncaptured events still follow the child's requested protocol through native conversion. Without those capabilities, control-digit shortcuts cannot be distinguished reliably; legacy Ctrl-2/NUL and Ctrl-3/Escape are not treated as capture aliases. The library's default path without a capture handler does not request these extra keyboard reports. [Keyboard disambiguation](https://sw.kovidgoyal.net/kitty/keyboard-protocol/#disambiguate-escape-codes).

`Raw` is an owned copy of the event's original input bytes. `Key` supplies native Ultraviolet key information without replacing those bytes. Use `input.Key.Key().MatchString(...)` for matching: the pinned native `KeyEvent` interface exposes `Key()`, and the returned `uv.Key` has `MatchString`. [Native events](https://github.com/charmbracelet/ultraviolet/blob/006e29f97886/event.go), [key matching](https://github.com/charmbracelet/ultraviolet/blob/006e29f97886/key.go).

`Pass` declines host capture and continues ordinary protocol-aware routing; original bytes remain unchanged when the outer and child protocols agree. `Consume` withholds that event from the child and records that the host handled it. Mutating the callback's byte copy does not rewrite routed input. Capture runs inline before delivery and must return promptly.

When the negotiated protocol reports releases, the press determines ownership of its final release. A consumed press keeps its repeats and release out of the child, even if the handler would subsequently return `Pass`; a passed press keeps its release on the child path. The handler still receives these phases. Legacy protocols without release reports do not latch capture ownership across later keys.

To add an explicit Ctrl-Q quit binding, replace the Quick Start's `Run` call with the following. Import the standard-library `context` package and Ultraviolet as `uv`.

```go
ctx, cancel := context.WithCancel(context.Background())
defer cancel()

result, err := app.Capture(func(input frame.Input) frame.Disposition {
	if !input.Key.Key().MatchString("ctrl+q") {
		return frame.Pass
	}
	if _, pressed := input.Key.(uv.KeyPressEvent); pressed {
		cancel()
	}
	return frame.Consume
}).Run(ctx)
```

This handler consumes matching Ctrl-Q events and requests cancellation on press, including reported repeats; a matching release is consumed without an action. Other events pass through. Press/repeat/release information depends on the negotiated input protocol; legacy input cannot report every distinction. The binding is entirely opt-in. [Key phases](https://github.com/charmbracelet/ultraviolet/blob/006e29f97886/key.go), [context cancellation](https://pkg.go.dev/context#WithCancel).

The session requests outer bracketed paste when that capability is verified, independently of the child's mode. Recognized paste payloads stay opaque to shortcut matching and retain their controls/newlines; only the bracketed envelope follows the child's requested mode. A mode change during a paste does not orphan its closing marker. If the outer terminal sends unmarked paste, those bytes are indistinguishable from typing, so optional keyboard capture cannot guarantee their exclusion. [paste boundaries](https://invisible-island.net/xterm/ctlseqs/ctlseqs.html#h2-Bracketed-Paste-Mode).

Semantic key data supplements original bytes instead of replacing them. Legacy terminal encodings cannot distinguish every physical key combination. Multi-key bindings require an explicit buffering timeout and replay policy; they cannot introduce an undocumented prefix delay. [keyboard ambiguities](https://sw.kovidgoyal.net/kitty/keyboard-protocol/).

# Child Information

Observation separates three exact byte streams: outer input before classification, child terminal output before parsing, and successful writes to the child after capture, coordinate translation, and generated replies. Records carry stream offsets, session sequence numbers, timestamps, origins, and routing dispositions. Read chunks do not reconstruct the child's original write calls. [read semantics](https://man7.org/linux/man-pages/man2/read.2.html).

| Observation family | Information | Boundary |
| :--- | :--- | :--- |
| Launch and lifecycle | Executable, argv, configured cwd/env, PID, start/exit, native process state, drain and cleanup events | Launch configuration is distinct from current runtime state. |
| Virtual terminal | Active viewport cells/graphemes, styles/colors, hyperlink URLs, cursor, alternate-screen selection, named modes and getter errors, Kitty/modifyOtherKeys state, scrollback counts/limits, parser and native memory state | `Terminal.Native` owns the native observations; it does not expose both screen buffers, full scrollback contents, margins, or damage records. |
| Protocol events | Title/cwd hints, bells, clipboard requests, replies, progress, notifications, semantic shell marks, native unknown-sequence observations, capture/translation decisions | Child-reported metadata is distinct from OS inspection; unknown-sequence callbacks are not an exhaustive parser audit. Raw `ChildOutput` preserves admitted transport bytes. |
| OS/TTY inspection | Launch leader's native process fields, current session members, parent/session/group IDs, runtime cwd/executable, kernel argv/env views, CPU/memory/threads, native counters, actual PTY winsize/termios/foreground group | Fields expose availability, source, acquisition time, and collection errors. Session membership excludes descendants that create a different session. |

Use `snapshot.Child.OperatingSystem`, `SessionProcesses`, `SessionError`, and `PTY` for these OS/TTY observations. A native field uses `Observation[T]{Value, Available, Source, Error}`; check `Available` before treating a zero or empty value as observed. The launch configuration in `Child.Args`, `Environment`, and `Directory` remains separate. Process and PTY reads are sequential samples collected at initial/relevant-child drawing or explicit region invalidation, with no periodic metadata ticker. An idle process can change after its last sample; invalidate a region when your application needs another sample.

After the launch leader is reaped, `OperatingSystem` retains its last running sample and its own `ObservedAt`. Session members and PTY state continue to be sampled during output drain and immediately before the exit observation; their acquisition times can differ from the retained leader sample.

Linux uses procfs/native syscalls, and macOS uses libproc/sysctl. Kernel argv is a mutable memory view; Linux environment describes the exec image rather than later libc environment changes. macOS can omit environment, so an empty/omitted result is marked unavailable. RSS accounting is approximate. Start identity detects observed PID reuse without making all field reads atomic. Snapshot slices, maps, cells, and PTY structures are independently copied; retain error values and `ProcessState` as read-only values. [Linux argv](https://man7.org/linux/man-pages/man5/proc_pid_cmdline.5.html), [environment](https://man7.org/linux/man-pages/man5/proc_pid_environ.5.html), [Darwin process interface](https://github.com/apple-oss-distributions/xnu/blob/main/libsyscall/wrappers/libproc/libproc.h).

A terminal stream cannot reveal an arbitrary child's widget tree, editor buffers, selected domain objects, or application intent. Cooperative child integration can provide additional semantic data through an explicit protocol. It cannot identify the writer PID or separate stdout from stderr after they share the same terminal stream. Sampling current slave settings does not establish a complete history of termios changes. OS inspection reports availability and collection errors instead of substituting guessed metadata. [PTY behavior](https://man7.org/linux/man-pages/man7/pty.7.html), [Linux cwd access](https://man7.org/linux/man-pages/man5/proc_pid_cwd.5.html), [packet-mode limits](https://man7.org/linux/man-pages/man2/TIOCPKT.2const.html).

# Compatibility

The supported runtime platforms are Linux and macOS. The child receives `TERM=xterm-256color` and `COLORTERM=truecolor`; physical-terminal vendor and graphics hints are removed. The outer terminal must answer DEC mode queries and report mode 1049 reset before startup. An existing outer alternate screen is rejected because its contents cannot be recovered from the TTY. Optional keyboard, mouse, focus, paste, and grapheme modes depend on verified outer support. [terminal identity](https://invisible-island.net/ncurses/man/terminfo.5.html), [DEC queries](https://invisible-island.net/xterm/ctlseqs/ctlseqs.html).

| Capability | Supported profile and boundary |
| :--- | :--- |
| Keyboard | Matching protocols preserve original bytes. Negotiated cursor/keypad, Kitty, and modifyOtherKeys differences use native encoding; recognized cursor/keypad wire forms supplement unavailable mode reports. Unsupported key/modifier identities fail explicitly when conversion is required; legacy input cannot supply absent physical identity or release phases. Bare Escape/two-byte introducer ambiguity uses a 50 ms deadline; parameterized fragmented controls and paste do not. |
| Paste and focus | Recognized paste payloads remain opaque; markers adapt to the child's bracketed mode. Focus reports reach only a child that requests them. |
| Mouse | Supported native cell encodings use child-local coordinates. Pixel reporting requires measured geometry and verified outer pixel support. Frame-origin gestures and outside releases are not clamped into child cells; dropping an outside release can leave the child's held-button state until a later valid event. Legacy/UTF-8 coordinate overflow, invalid native UTF-8 button reports, and unsupported buttons 10/11 fail explicitly. Highlight tracking is unsupported. |
| Screen state | Text, colors, native supported rendition, hyperlinks, cursor, alternate screens, and synchronized-update holds are composed inside the viewport. Overline is preserved in native cell metadata but cannot be rendered by the selected UV cell style. Hyperlink URLs are available; OSC 8 parameter/id metadata is not exposed by the binding. |
| Geometry and widths | Outer resize recomputes the viewport and native PTY size. A resize leaving no positive child viewport returns `ErrViewportTooSmall`. A child-requested DECCOLM grid change outside that allocation returns `*frame.GeometryError`, matching `frame.ErrGeometry`; it does not resize the physical terminal or silently crop a 132-column grid. Font/shaping and an outer terminal without agreed grapheme-width support limit Unicode fidelity. |
| Graphics and side effects | Graphics are not rasterized into the composed display or advertised through device attributes. Kitty graphics is disabled natively, including positive capability replies; Sixel is unsupported by the selected emulator. Clipboard requests are observed and receive unsupported replies; notifications/progress are observations rather than desktop side effects. Bell requests ring the outer terminal. Child titles and cwd hints remain metadata. |

These boundaries define the current supported profile. Full image placement/clipping, complete historical/screen metadata, and additional outer effects remain implementation-spec requirements where the requested arbitrary-TUI scope exceeds that profile. Raw passthrough cannot provide confined graphics or arbitrary outer-terminal effects. The [architecture research](reports/TUI%20frame%20architecture%20research.md) records these gaps and dependency tradeoffs. [Graphics protocol](https://sw.kovidgoyal.net/kitty/graphics-protocol/).

Font/Unicode shaping, unavailable pixel geometry, unknown extensions, terminal-consumed shortcuts, detached descendants, and uncatchable process termination impose fidelity limits. Process-group cancellation cannot guarantee termination of a descendant that escapes its group. [Unicode width guidance](https://www.unicode.org/reports/tr11/), [process-group signals](https://man7.org/linux/man-pages/man2/kill.2.html).

Cancellation sends SIGTERM and SIGCONT to observed groups in the owned child session, then SIGKILL after one second if needed. Final cleanup inventories that session even after the launch leader exits. Output drain has a two-second deadline after the leader is reaped. Descendants that create a different session and races in inventory/signaling remain boundaries; the wrapper does not implement suspend/resume of its own terminal session.

# Example CLI

The bundled example wraps the command and complete argument list after `--`:

```sh
./bin/tui-frame -- nvim --clean
```

The example's explicit capture handler supplies these controls:

| Key | Behavior |
| :--- | :--- |
| Ctrl-1 | Cycle the Signal bar, Layered badge, and Bordered card layouts. |
| Ctrl-2 | Cycle the header background through red, navy, and teal independently of the layout. |
| Ctrl-3 | Toggle the child border, recomputing its viewport and terminal size. |
| Ctrl-Q | Exit the example. |

Control-digit bindings require distinct modified-key reports from the outer terminal, as described under keyboard capture. Other input follows the normal child path; the library has no default shortcuts. See the [example source](cmd/tui-frame) and [native build setup](docs/internal/references/native-build.md) for the executable.

`AGENT=1` uses plain header/footer text and starts with the child border disabled; Ctrl-3 can enable it. The [example's usage guide](cmd/tui-frame/SKILL.md) documents its commands, environment, and terminal side effects.

# License

[MIT License](LICENSE) © 2026 Alex Gorbatchev.
