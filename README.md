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
6. **Observation has explicit costs.** Raw recording preserves bytes observed at the terminal transport, not application write boundaries. A shared child terminal merges stdout, stderr, and other writers. Owned snapshots and retained history require bounded memory; optional process sampling adds platform-specific work. Linux process metadata has permission and identity limits. [stream reads](https://man7.org/linux/man-pages/man2/read.2.html), [process metadata](https://man7.org/linux/man-pages/man5/proc_pid_stat.5.html).

# Prerequisites

- Go 1.27.1 or newer, as declared in [go.mod](go.mod).
- A Unix-like system or macOS with an interactive outer terminal.
- The executable child TUI you want to wrap.

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
| `Border` | `(enabled bool) *Frame[T]` | Declare a border around the child viewport. |
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

Before terminal mutation, `Run` validates static requirements: non-nil/unstarted command, positive region dimensions, non-nil drawing callbacks, positive initial child viewport, compatible command I/O and process attributes, terminal ownership, and the configured capability profile. It then acquires the terminal reversibly and performs any required capability probes before starting the child. Active query responses can require noncanonical input, so probing cannot always precede every mode change. Failed acquisition or capability verification restores acquired terminal state and returns an error. A frame executes one session and is not reusable after `Run`. [query replies](https://sw.kovidgoyal.net/kitty/keyboard-protocol/#progressive-enhancement), [canonical input](https://man7.org/linux/man-pages/man3/termios.3.html).

The session owns command start/wait, child terminal descriptors, and its I/O workers. Callers must not concurrently start, wait on, or modify an accepted command. Preassigned stdio or lifecycle settings that conflict with terminal/session ownership are rejected rather than silently replaced. The controller remains available to event handlers during execution; `Result` becomes available after the session ends. [Go `Cmd` ownership](https://pkg.go.dev/os/exec#Cmd).

`InvalidateHeader`, `InvalidateFooter`, `InvalidateLeft`, and `InvalidateRight` are safe for concurrent submission before and during `Run`. Before execution, a successful call replaces that configured region's initial payload. During execution, it atomically publishes a complete payload and dirty revision; a callback receives one stable by-value payload. Pending submissions coalesce, retaining the latest accepted value for that region. An update racing startup or a drawing callback schedules the needed drawing pass without losing its wakeup. While the session remains active, the latest settled submission reaches the region.

Invalidating an absent region returns `ErrRegionNotConfigured`; once session shutdown begins, invalidations return `ErrSessionClosed`. Submission and shutdown are ordered, so an accepted update cannot refer to released session resources. Acceptance publishes data but does not acknowledge a completed paint; the session can close before that paint occurs. Invalidation never performs terminal I/O or waits for a renderer or callback.

Application data is copied by value, without a deep clone. Submit immutable snapshots, and keep referenced maps, slices, pointers, and their contents immutable while the frame can use them. The example's struct of strings needs no caller-managed synchronization. `DrawContext.Term` is owned separately from `DrawContext.Data`; grouping them in one value does not make them a joint atomic transaction. [Go assignment semantics](https://go.dev/ref/spec#Assignment_statements).

Frame dimensions are fixed by their declarations. Headers and footers reserve the specified rows; side regions reserve the specified columns. Content is clipped to its region at terminal-cell boundaries, and changing text does not resize the child or paint outside that region. Resizing the outer terminal recomputes the child viewport and canvas dimensions from the same frame declarations.

Drawing callbacks use the canvas in the session's serialized rendering path, outside state locks, and must return promptly. Terminal controls within display text do not become arbitrary outer-terminal operations. Prefer the supplied payload over reading mutable application state directly; synchronize any foreign shared state that a callback does read.

Initial drawing, explicit region invalidation, relevant child-state changes, and resize schedule rendering. Each invalidation requests drawing even if its arbitrary `T` payload appears unchanged; the library does not require comparability or perform a promised deep-equality check. Normal output uses a cell diff, so identical rendered content does not repaint. Frame content has no default polling or periodic resnapshot timer; animation requires explicit consumer scheduling.

Passive `Observe` callbacks run on a separate ordered dispatcher and receive durable copies. Queue overflow is reported as a session error rather than silent byte loss. Live screen snapshots can coalesce with recorded sequence gaps. Callbacks must return promptly and honor session cancellation through the caller's context; a Go function that never returns cannot be forcibly canceled.

## Optional keyboard capture

There is no capture handler by default. If the consumer declares one, it receives recognized keyboard events with this structure; `uv` is `github.com/charmbracelet/ultraviolet`:

```go
type Input struct {
	Raw []byte
	Key uv.KeyEvent
}
```

`Raw` is an owned copy of the event's original input bytes. `Key` supplies native Ultraviolet key information without replacing those bytes. Use `input.Key.Key().MatchString(...)` for matching: the pinned native `KeyEvent` interface exposes `Key()`, and the returned `uv.Key` has `MatchString`. [Native events](https://github.com/charmbracelet/ultraviolet/blob/006e29f97886/event.go), [key matching](https://github.com/charmbracelet/ultraviolet/blob/006e29f97886/key.go).

`Pass` declines host capture and continues ordinary protocol-aware routing; original bytes remain unchanged when the outer and child protocols agree. `Consume` withholds that event from the child and records that the host handled it. Mutating the callback's byte copy does not rewrite routed input. Capture runs inline before delivery and must return promptly.

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

Recognized bracketed-paste payloads stay opaque to shortcut matching and retain their controls/newlines. Unmarked paste is indistinguishable from typing, so an optional keyboard capture cannot guarantee exclusion of those bytes. The default without capture preserves paste through the ordinary forwarding path. [paste boundaries](https://invisible-island.net/xterm/ctlseqs/ctlseqs.html#h2-Bracketed-Paste-Mode).

Semantic key data supplements original bytes instead of replacing them. Legacy terminal encodings cannot distinguish every physical key combination. Multi-key bindings require an explicit buffering timeout and replay policy; they cannot introduce an undocumented prefix delay. [keyboard ambiguities](https://sw.kovidgoyal.net/kitty/keyboard-protocol/).

# Child Information

Observation separates three exact byte streams: outer input before classification, child terminal output before parsing, and successful writes to the child after capture, coordinate translation, and generated replies. Records carry stream offsets, session sequence numbers, timestamps, origins, and routing dispositions. Read chunks do not reconstruct the child's original write calls. [read semantics](https://man7.org/linux/man-pages/man2/read.2.html).

| Observation family | Information | Boundary |
| :--- | :--- | :--- |
| Launch and lifecycle | Executable, argv, configured cwd/env, PID, start/exit, native process state, drain and cleanup events | Launch configuration is distinct from current runtime state. |
| Virtual terminal | Cells/graphemes, styles/colors, hyperlinks, both screens, cursor, modes, margins, scrollback, damage, viewport, capability state | Derived from terminal output under the selected protocol profile. |
| Protocol events | Title/cwd hints, bells, clipboard/graphics requests, replies, unsupported controls, capture/translation decisions | Child-reported metadata is labeled as such; observation does not imply raw passthrough. |
| OS/TTY inspection | Foreground process group, discovered descendants, current cwd, state, CPU/memory accounting, current slave settings where available | Platform, permission, sampling time, identity races, and collection errors remain visible. |

A terminal stream cannot reveal an arbitrary child's widget tree, editor buffers, selected domain objects, or application intent. Cooperative child integration can provide additional semantic data through an explicit protocol. It cannot identify the writer PID or separate stdout from stderr after they share the same terminal stream. Sampling current slave settings does not establish a complete history of termios changes. OS inspection reports availability and collection errors instead of substituting guessed metadata. [PTY behavior](https://man7.org/linux/man-pages/man7/pty.7.html), [Linux cwd access](https://man7.org/linux/man-pages/man5/proc_pid_cwd.5.html), [packet-mode limits](https://man7.org/linux/man-pages/man2/TIOCPKT.2const.html).

# Compatibility

The child connects to the library's virtual terminal, whose advertised capabilities agree with its parsing, input/replies, rendering, observation, and cleanup. Capability responses describe that endpoint rather than claiming every extension of the physical terminal. Unsupported controls remain observable. Images require viewport-aware placement and clipping; opaque passthrough does not guarantee confinement. Clipboard and other outer-terminal side effects are distinct from ordinary screen output. [terminfo](https://invisible-island.net/ncurses/man/terminfo.5.html), [graphics state](https://sw.kovidgoyal.net/kitty/graphics-protocol/).

Font/Unicode shaping, unavailable pixel geometry, unknown extensions, terminal-consumed shortcuts, detached descendants, and uncatchable process termination impose fidelity limits. Process-group cancellation cannot guarantee termination of a descendant that escapes its group. [Unicode width guidance](https://www.unicode.org/reports/tr11/), [process-group signals](https://man7.org/linux/man-pages/man2/kill.2.html).

The [architecture research](reports/TUI%20frame%20architecture%20research.md) explains the terminal contract and dependency tradeoffs in depth.

# License

A project license has not been selected.
