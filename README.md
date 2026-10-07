`go-tui-frame` lets Go applications put their own UI around an external TUI. Wrap an editor, shell, or interactive tool with styled headers, footers, side regions, and a border, then update them from your application's events. The child keeps its own terminal; input passes through by default.

![Pressed Ctrl+B, then 1, 2, or 3, changes the frame's layouts, header colors, and border while Yazi runs inside.](assets/demo.gif)

# What It Does

- **Custom frame:** Draw into region-sized cell buffers. Use Lip Gloss for styles and layers, or draw directly without it.
- **Concise configuration:** Chain region declarations and optional handlers, then call `Run(ctx)`.
- **Live updates:** Push typed application data to individual regions and toggle the child border during a session.
- **Child information:** Read terminal cells, title, cursor, modes, process metadata, and PTY settings; observe input, output, and lifecycle events.
- **Optional shortcuts:** Capture selected keys and forward the rest. The library has no default bindings or pane management.
- **Standalone executable:** The terminal emulator links into your binary. Running it requires no Ghostty installation or multiplexer server.

# How It Works

1. Supply an unstarted `exec.Cmd`, initial application data, and drawing callbacks for the regions you want.
2. Call `Run(ctx)`. The child starts with the terminal space left inside the frame.
3. Use the child normally. Your callbacks receive its snapshot and your region's data. Application events can push updates while the child is idle.
4. When the child exits or the context is canceled, the session restores the terminal and returns the child result and any session error.

# How it Really Works

1. **The child has a separate terminal display.** Its erase, cursor, reset, and alternate-screen operations stay inside its viewport. That display is combined with your regions before writing to the outer terminal.
2. **Input follows the child's protocol.** The outer terminal runs the child's keyboard modes, so keys reach the child exactly as the terminal sends them. Mouse reports become child-local coordinates in the child's mouse encoding. Input over the frame is not mapped to an invented child position. Up to 1 MiB of routed input waits for a child that is not reading; beyond that, the frame stops reading the outer terminal until the child reads again, as a terminal connected directly to the child would wait. A terminal hangup still ends the session.
3. **Updates are event-driven.** Initial drawing, application invalidation, relevant child changes, and resize trigger rendering. Child-output bursts share a repaint deadline of about 17 ms. Normal rendering updates damaged rows and emits only changed cells, with synchronized output when the outer terminal reports support. Idle sessions have no periodic redraw or metadata polling timer. Input modes and protocol replies take effect without waiting for repaint.
4. **One session owns terminal I/O.** Construction has no terminal side effects; `Run` blocks. Draw into the supplied canvases and submit updates through the controller. Concurrent terminal writes interfere with the composed display.
5. **Metadata has limits.** Snapshots own their data, and OS fields report availability and errors. Terminal output reveals the child's display and protocol requests; it cannot reveal an arbitrary application's widget tree or editor buffers.

# Prerequisites

- Linux or macOS and an interactive terminal. Windows is unsupported.
- The child executable you want to wrap, such as `nvim`.
- [Go](https://go.dev/dl/) 1.27.1 or newer to build a consumer.
- CGO, a C compiler, `pkg-config`, and the pinned libghostty headers/static archive at build time. Building the archive requires Zig 0.16.0; follow the [native build setup](docs/internal/references/native-build.md).

The compiled consumer needs no separate libghostty runtime. macOS binaries use OS-provided system libraries; the repository's Linux builds link statically with musl. The child executable and outer terminal are still required.

# Installation

```sh
go get github.com/alexgorbatchev/go-tui-frame
```

Set up the native archive before compiling your consumer; `go get` installs the Go dependency, not the native build tools or archive.

# Quick Start

This complete program wraps Neovim in a three-row red header showing application status and child PID, with a footer showing the child's title and viewport size.

```go
package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"

	"charm.land/lipgloss/v2"
	frame "github.com/alexgorbatchev/go-tui-frame"
	uv "github.com/charmbracelet/ultraviolet"
)

type UIData struct{ Status string }

func main() {
	banner := lipgloss.NewStyle().
		Background(lipgloss.Color("#B91C1C")).
		Foreground(lipgloss.Color("#FFFFFF")).Bold(true).Padding(0, 1)

	app := frame.New(exec.Command("nvim", "--clean"), UIData{Status: "Ready"}).
		Header(3, func(ctx frame.DrawContext[UIData]) {
			text := fmt.Sprintf("My workspace · PID %d\n%s", ctx.Term.Child.PID, ctx.Data.Status)
			paint(ctx.View, banner, text)
		}).
		Footer(1, func(ctx frame.DrawContext[UIData]) {
			text := fmt.Sprintf("%s · %d×%d",
				ctx.Term.Terminal.Title, ctx.Term.Viewport.Cols, ctx.Term.Viewport.Rows)
			paint(ctx.View, lipgloss.NewStyle().Foreground(lipgloss.Color("#9CA3AF")), text)
		}).
		Border(true)

	result, err := app.Run(context.Background())
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if !result.ProcessState.Success() {
		fmt.Fprintln(os.Stderr, result.ProcessState)
		os.Exit(1)
	}
}

func paint(view uv.Screen, style lipgloss.Style, text string) {
	bounds := view.Bounds()
	if bounds.Empty() {
		return
	}
	lipgloss.NewLayer(style.
		Width(bounds.Dx()).Height(bounds.Dy()).
		MaxWidth(bounds.Dx()).MaxHeight(bounds.Dy()).Render(text)).Draw(view, bounds)
}
```

Lip Gloss is recommended for styling and used by this example; it is optional for library consumers. See the [demo app](cmd/tui-frame) for three interactive layouts with layered badges, background changes, and a live border toggle. Its [drawing callbacks](cmd/tui-frame/demo.go) and [capture handler](cmd/tui-frame/session.go) use the same public API.

## Push application updates

Keep `app` available to your application's event handlers while `Run` is blocking. For example, an indexing-completion handler can submit:

```go
if err := app.InvalidateHeader(UIData{Status: "Index complete"}); err != nil {
	return fmt.Errorf("update workspace status: %w", err)
}
```

Invalidation replaces that region's complete data value and schedules drawing. It is safe from any goroutine before or during `Run`, including while the child emits no output. Pending updates coalesce to the latest value. Submission performs no terminal I/O and does not wait for painting; there are no watcher declarations or refresh channels to manage.

Each region keeps its own data: updating the header does not update the footer. Data is copied by value, so keep referenced maps, slices, and pointers immutable while the frame can use them. Invalidating an absent region returns `ErrRegionNotConfigured`; submission after shutdown begins returns `ErrSessionClosed`.

# API

| Method | Signature | Purpose |
| :--- | :--- | :--- |
| `New` | `New[T any](cmd *exec.Cmd, initial T) *Frame[T]` | Create a controller for one unstarted command. |
| `Header`, `Footer` | `(rows int, draw func(DrawContext[T])) *Frame[T]` | Reserve top or bottom rows. |
| `Left`, `Right` | `(cols int, draw func(DrawContext[T])) *Frame[T]` | Reserve side columns between the header and footer. |
| `Border` | `(enabled bool) *Frame[T]` | Configure the initial one-cell child border. |
| `SetBorder` | `(enabled bool) error` | Submit a live border change. |
| `InvalidateHeader`, `InvalidateFooter` | `(data T) error` | Replace a region's data and schedule drawing. |
| `InvalidateLeft`, `InvalidateRight` | `(data T) error` | Replace a side region's data and schedule drawing. |
| `Capture` | `(handler func(Input) Disposition) *Frame[T]` | Consume selected keyboard events or pass them to the child. |
| `Observe` | `(handler func(Event)) *Frame[T]` | Receive owned observations without consuming input. |
| `ObserveEvents` | `(kinds []EventKind, handler func(Event)) *Frame[T]` | Receive only the selected observation kinds. |
| `Terminal` | `(input, output *os.File) *Frame[T]` | Borrow terminal files; defaults to standard input and output. |
| `InheritTerminal` | `(enabled bool) *Frame[T]` | Inherit reported terminal preferences and original PTY settings; enabled by default. |
| `Run` | `(ctx context.Context) (Result, error)` | Execute one session and wait for drain and cleanup. |

## Drawing context

All drawing callbacks receive the same strongly typed context:

```go
type DrawContext[T any] struct {
	Term Snapshot
	View uv.Screen
	Data T
}
```

`Term` is an owned child snapshot. `Data` is the selected region's application payload. `View` is a cleared, region-sized `uv.Screen` backed by a native screen buffer. `View.Bounds().Dx()` and `.Dy()` give its width and height in terminal cells. Coordinates start at the region's own origin.

Regions redraw for title, directory, size, terminal-mode, alternate-screen, or
scroll metadata changes, and for application invalidation. Text changes, cursor
movement, cursor visibility, and synchronized-update holds update the child
display without invalidating region canvases. Use region invalidation when your
application needs to redraw a region from those display details.

The drawing contract requires synchronous drawing into `View`; filling the region and using Lip Gloss styles are optional. You can call `SetCell` directly or draw any `uv.Drawable`. Lip Gloss layers implement that interface: `lipgloss.NewLayer(text).Draw(ctx.View, ctx.View.Bounds())`. For positioned or overlapping layers, use `lipgloss.NewCompositor(layers...).Draw(ctx.View, ctx.View.Bounds())`. The library clips the completed region with wide-cell boundaries preserved. [Screen and drawable interfaces](https://github.com/charmbracelet/ultraviolet/blob/878653296cfd/uv.go), [Lip Gloss layers](https://github.com/charmbracelet/lipgloss/blob/v2.0.6/layer.go).

For plain text without Lip Gloss, use the existing Ultraviolet drawing primitive:

```go
app.Header(1, func(ctx frame.DrawContext[UIData]) {
	uv.NewStyledString(ctx.Data.Status).Draw(ctx.View, ctx.View.Bounds())
})
```

The canvas is borrowed until the callback returns. Keep its dimensions, draw synchronously, and do not retain it. Callbacks run in the session's drawing path and must return promptly. Use offscreen styling; direct printing or terminal queries bypass the session's I/O ownership. `DrawContext` is drawing data, separate from the cancellation `context.Context` passed to `Run`.

## Session and layout

Configure the frame before calling `Run`; region declarations and handlers freeze at startup. Region sizes must be positive, the child viewport must fit, and the command must be unstarted with no conflicting stdio or process settings. The session owns command start/wait. Use a standard `exec.Cmd` to set `Dir` or `Env`; its arguments are passed without shell interpretation. [Go command semantics](https://pkg.go.dev/os/exec#Cmd).

`Run` borrows the outer terminal files and restores the state it acquires. For each cursor attribute the terminal reported at startup, the outer terminal shows the child's style or color; while they match what the terminal reported, the session writes neither. When the child's style or color returns to the reported value, and on exit, the session resets an attribute that differed to the terminal's defaults with `CSI 0 SP q` or `OSC 112`, so the cursor follows the terminal's configuration and theme again. Terminals that follow the VT510 definition of `CSI 0 SP q` show a blinking block instead. A cursor style or color override that was active before `Run`, such as an `OSC 12` color from a shell theme script, is lost once the child's cursor differs in that attribute, because the terminal reports an override and a default the same way. With `.InheritTerminal(false)`, the child starts with the emulator's default cursor style, so any other reported style differs even when the child never changes its cursor, and an entry style override is lost the same way. Input and output must refer to the same interactive terminal, with one frame owner at a time. Each controller executes one session and cannot be reused.

At startup, the child inherits reported foreground/background colors, the 256-color palette, cursor color/style/blink, supported preference modes, keyboard protocol settings, and light/dark color scheme. Its PTY receives the outer terminal's original line discipline, including control characters, echo, and flow control. Default-colored cells keep the outer terminal's default rendition, preserving its background appearance. Queries have a 300 ms deadline; missing or invalid replies retain native defaults.

The outer terminal's color profile is whatever `colorprofile.Detect` decides from the output and environment, including `TERM`, `COLORTERM`, `NO_COLOR`, terminfo, and tmux. On a 256-color profile, cells using an inherited palette entry the child has not redefined keep the outer terminal's palette index, so its theme colors them instead of the nearest 256-color value; a 16-color profile does this for entries 0–15, and higher entries keep their RGB approximation. Snapshot cells carry these indexed colors as `ansi.BasicColor` or `ansi.IndexedColor`, with the RGB in `Terminal.Native.Colors.Palette`. True-color terminals receive resolved RGB colors, and colorless profiles, such as `NO_COLOR` or `TERM=dumb`, receive no color; snapshot cells keep resolved RGB on both. Known limitation: Ultraviolet compares colors only by RGB value (`charmbracelet/ultraviolet#205`), and an index's value is its xterm default rather than the theme color. On a 256-color profile, two colors with the same such value, such as SGR 30 and `38;2;0;0;0` or `38;5;16`, can keep the earlier color when they are adjacent or one replaces the other, including an OSC 4 redefinition of an entry to its xterm default value or an OSC 104 reset from it. On a 16-color profile, only entries 7 and 8 are affected: when an entry and a color of its xterm value (`#c0c0c0`, `#808080`) replace each other, including an OSC 4 redefinition to that value or an OSC 104 reset from it, the earlier color can remain.

Use `.InheritTerminal(false)` or the demo's `--no-terminal-inheritance` flag before `--` to use emulator and PTY defaults. Capability and restoration probes still run. Font, shaping, opacity, window settings, and terminal key mappings belong to the outer terminal and cannot be disabled inside a viewport. Screen contents, scrollback, margins, and application mouse tracking belong to the new child session; graphics and clipboard permissions remain subject to the documented endpoint capabilities. Preferences are captured at startup; live changes to the outer terminal's theme are not queried again.

Region reservations stay fixed. Outer resize recomputes their canvases and the child viewport. `SetBorder` is safe concurrently before and during `Run`; it coalesces pending requests and resizes the child's native terminal and PTY when applied, preserving measured cell pixels. It returns `ErrSessionClosed` after shutdown begins. Acceptance does not acknowledge painting; an inset that cannot fit ends the session with `ErrViewportTooSmall`.

`Result.ProcessState` contains the child exit outcome after a successful start and wait. A nonzero child exit is a result; configuration, startup, transport, observation, drain, and restoration failures are session errors. `DrainError` and `CleanupError` preserve their respective outcomes. Inspect the result as well as the returned error.

Cancellation sends SIGTERM and SIGCONT to observed groups in the owned child session, then SIGKILL after one second if needed. If the launch leader has not exited two seconds after SIGKILL, the session closes the child PTY, discarding output it has not read, and waits for the leader; the discarded output is not reported as an error. This bound matters on macOS, where an exiting session leader waits for its queued output to be read, and output that flow control stopped, as Ctrl+S does, cannot be read. Output drain has a two-second deadline after the launch leader exits. Descendants that create another session can escape cleanup; suspend/resume of the wrapper's terminal session is unsupported. A context canceled before the child exits makes `Run` return the context's cause (`context.Cause`). So does a context canceled after the child exits but before the output drain ends, such as by a capture handler that receives input the child never read; `Result.ProcessState` then holds the child's own exit status. If the context is already canceled when `Run` is called, `Run` returns the cause by itself, without touching the terminal or starting the child, unless the controller has already run or its configuration is invalid, which `Run` reports instead. If the context is canceled during `Run`, before the output drain ends, the cause is joined with any errors from the shutdown that follows; a session error that ended the session first is returned without the cause. Either way, a cancellation returns an error even when nothing failed; cancel with a cause of your own to recognize a requested stop, as [Keyboard Capture](#keyboard-capture) does.

# Keyboard Capture

Capture is opt-in. To add Ctrl+Q to the Quick Start, add `errors` to its imports and replace its `Run` call with the following, keeping the error and exit-status checks after it:

```go
ctx, cancel := context.WithCancelCause(context.Background())
defer cancel(nil)

result, err := app.Capture(func(input frame.Input) frame.Disposition {
	if !input.Key.Key().MatchString("ctrl+q") {
		return frame.Pass
	}
	if _, pressed := input.Key.(uv.KeyPressEvent); pressed {
		cancel(errQuit)
	}
	return frame.Consume
}).Run(ctx)
if quitOnly(err) {
	err = nil
}
```

Then add the quit cause and its check at package level:

```go
var errQuit = errors.New("quit requested")

// quitOnly reports whether err holds errQuit and nothing else. Run joins the
// cancellation cause with any errors from the shutdown that follows.
func quitOnly(err error) bool {
	joined, ok := err.(interface{ Unwrap() []error })
	if !ok {
		return err == errQuit
	}
	errs := joined.Unwrap()
	for _, e := range errs {
		if !quitOnly(e) {
			return false
		}
	}
	return len(errs) > 0
}
```

When nothing else fails, a quit makes `Run` return an error that holds only `errQuit`, the cancellation cause. The quit is a request rather than a failure, so the example clears that error, and the Quick Start's exit-status check reports how the child ended. A child that the quit's SIGTERM ends is reported as `signal: terminated` and `main` exits 1; a child that exits 0 on SIGTERM lets `main` return with status 0. Capture can also receive Ctrl+Q after the child has already exited, such as a Ctrl+Q the frame held while the child was not reading its input; the quit then ends nothing, and the check reports the child's own exit status. Errors from the shutdown that follows, such as a terminal that could not be restored, are joined with the cause; `errors.Is(err, errQuit)` would match them too, while `quitOnly` leaves them to the error check, which reports them and exits 1.

Use `uv "github.com/charmbracelet/ultraviolet"`. `Input.Key` carries the recognized key event; `Input.Raw` is an owned copy of its original bytes. A Kitty report fills the key's `BaseCode` and `ShiftedCode` only while the child requests alternate keys. `String()` returns the key's text when it has any and otherwise falls back to `Keystroke()`, which prefers `BaseCode`. For a binding with Ctrl or Super, such as `ctrl+q`, `MatchString` compares only the reported key code and modifiers, which alternate keys do not change. On a layout other than US, whether such a binding matches depends on the child's keyboard mode. With a legacy child, a legacy terminal sends Russian Ctrl+й, the key in the US Q position, as the control byte 0x11, which decodes as Ctrl+Q and matches `ctrl+q`. While a child requests Kitty disambiguation, the terminal reports the key's own code: `String()` reads `ctrl+q` with alternate keys and `ctrl+й` without them, and `MatchString("ctrl+q")` matches it in neither case. Lock state does not count as a modifier unless the binding names it: a Kitty report carries the bit of an enabled Caps Lock or Num Lock, and `ctrl+q` still matches it. Caps Lock counts only for a key whose text has a character that `unicode.ToUpper` and `unicode.ToLower` map to different runes, so not `ß`; such a key matches through its text, so Caps Lock with `a` matches `A` but not `a`. An Alt binding holds the same way only where Alt does not type a character: on macOS, an Option key that types a character arrives as that text unless the outer terminal reports every key as an escape code. `MatchString` also compares text, which alternate keys can change, so a Shift-only printable binding such as `!` can depend on the child's flags: when the outer terminal reports every key as an escape code, US Shift+1 decodes with the text `1` without alternate keys or associated text and with `!` when either is on. `Pass` forwards input through normal child routing; `Consume` withholds it. Capture runs inline and must return promptly.

When releases are reported, a press decides how its reported repeats and its release are routed: the handler still runs for them, but its answer is ignored.

This pairing is incomplete while the child does not request every key as an escape code. The terminal then sends a key that types text as that text, and unmodified Enter, Tab or Backspace as its legacy byte, so their repeats arrive as new presses that the handler routes. It reports no release for Enter, Tab or Backspace, and reports a text key's release by the key rather than its text. Such a release follows its press only when the key is not on the keypad and the press typed the key's unshifted character or, for an ASCII letter, its capital. Any other release, such as that of Shift+1 typing `!`, Shift+й typing `Й`, a keypad digit or a macOS Option character, is not paired with its press: it can reach the child after a consumed press, or be withheld after a passed one.

Recognized paste payloads bypass key capture and reach the child unchanged, including text shaped like a terminal's reply to the frame's startup queries; unmarked paste cannot be distinguished from typing. Observation does not consume input.

Capture does not change what the outer terminal sends. The terminal runs the child's keyboard modes, and a key the handler passes reaches the child exactly as the terminal sent it, so the handler can tell apart only the keys the child's protocol distinguishes. With a legacy child, Ctrl+digit arrives as the digit or control character a legacy terminal sends for it, such as `1` for Ctrl+1 and NUL for Ctrl+2; Ctrl+I arrives as Tab, Ctrl+M as Enter, and Ctrl+[ as Escape. Each pair decodes to the same key. A child that requests [Kitty disambiguation](https://sw.kovidgoyal.net/kitty/keyboard-protocol/#disambiguate-escape-codes) or modifyOtherKeys mode 2 makes these keys distinct while it keeps that mode. Bind keys that legacy input already distinguishes, such as `ctrl+q`, when the binding must work with every child.

Upstream Ultraviolet decodes an alternate-key report's US-layout key as its shifted character, so on a layout other than US capture handlers would see the US letter in the same position as `Input.Key`'s `ShiftedCode`, and a Shift-only report without associated text would decode to it as text: Russian Shift+И would read as `b`. Upstream `MatchString` also counts every reported lock as a modifier, so `ctrl+q` fails while Caps Lock or Num Lock is on. This module's `go.mod` replaces Ultraviolet with a fork that keeps the two keys apart and leaves lock state out of `MatchString` as described above. Go applies `replace` directives only in the main module, so add the same directive to your application's `go.mod` until upstream Ultraviolet fixes its alternate-key decoder and its lock matching. The directive names no version on its left side, so it replaces every Ultraviolet version in your build with this fork commit, including for a dependency that requires a newer Ultraviolet:

```go.mod
replace github.com/charmbracelet/ultraviolet => github.com/alexgorbatchev/ultraviolet v0.0.0-20261006132318-0ff1fafbd555
```

# Child Information

Drawing callbacks receive `Snapshot{Child, Terminal, Viewport, Outer, ObservedAt}`. Use `Observe` for ordered event delivery outside the drawing path.

`Observe` subscribes to every event kind. `ObserveEvents` selects the kinds your
handler needs, such as `ChildOutput` and `ChildInput` for transport logging. An
empty selection disables observations; unknown kinds make `Run` fail before
touching the terminal. Event sequence numbers count delivered events, and byte
offsets remain local to each selected transport stream.

`Started` carries the child's first snapshot once the session has entered the
outer terminal and painted its first frame. `Exited` follows it exactly once,
before `Run` returns, when `Wait` reports the launch leader: after the child
exits, after cancellation, or after a session error that ends the session while
the child runs. Its snapshot's `Child.ProcessState` is the state
`Result.ProcessState` reports. When the session closes the child PTY to release
a launch leader that cannot finish exiting, that snapshot's PTY data is the
sample taken just before the close. A session that fails before emitting
`Started`, including a `Started` that overflows the queue, emits no `Exited`.
`Exited` is lost only when observation fails: a callback panic or
`runtime.Goexit` discards it with every later event, and an `Exited` that
overflows the queue is dropped, with `ErrObservationOverflow` in `Run`'s error.

Selecting `StateChanged` captures an owned terminal snapshot after every child
output read, including reads coalesced into one repaint. A subscription without
`StateChanged` captures the viewport when painting or delivering a selected
lifecycle snapshot. A child synchronized update also checkpoints its last
complete frame. Use transport events when per-read display state is
unnecessary to avoid those captures and copies.

| Information | Available data |
| :--- | :--- |
| Child lifecycle | Executable, argv, launch environment/directory, PID, start time, process state, exit and cleanup observations. |
| Terminal display | Active viewport cells, colors, styles, hyperlinks, cursor, title, cwd hints, alternate-screen state, named modes, keyboard protocol state, mouse tracking mode, and scrollback counts. |
| Protocol activity | Bells, clipboard requests, generated replies, notifications, progress, shell marks, and native unknown-sequence observations. |
| OS and PTY | Available process identity, session members, runtime cwd/executable, kernel argv/environment views, CPU/memory/thread data, and actual PTY window size, settings, and foreground group. |
| Transport | Outer input, child output before parsing, and successful child-input writes, with sequence numbers, offsets, timestamps, origins, and routing decisions. |

`Terminal.Native.MouseTrackingMode` is the child's active mouse tracking mode, the one its mouse encoder reports in. Setting mode 9, 1000, 1002, or 1003 makes that mode active, and resetting any of them turns tracking off. `Terminal.Native.MouseTracking` is libghostty's getter, which is true while any of those mode bits is set, so it stays true after a child sets 1000 and 1002 and then resets 1002, although tracking is off.

`Child.OperatingSystem`, `SessionProcesses`, `SessionError`, and `PTY` contain OS observations. Native fields use `Observation[T]{Value, Available, Source, Error}`; check `Available` before treating an empty value as observed. Launch configuration remains separate from runtime state. Fields are sequential samples with acquisition times, not an atomic process transaction.

Process and PTY data are sampled before `Started`, before every `Resized` (an outer resize, a border change, or a new cell size), when Wait reaps the launch leader, on explicit region invalidation, and when the child changes its title, directory, size, alternate screen, or terminal modes. Each sample enumerates every process on the host. A redraw caused only by scrolling reuses the previous sample until that sample is 250 ms old, so a child that streams output refreshes it about four times a second. Outside the startup, resize, and reap samples, a session takes none when no region is configured and no `Started`, `Resized`, `Exited`, or `StateChanged` observation is selected, because nothing reads the result. Invalidate a region when you need a fresh sample from an idle child. The launch leader's last running sample remains available after it is reaped; session members and PTY state continue to be sampled during drain.

Snapshots and events own their slices, maps, and cell storage. Keep error values and `ProcessState` read-only. Transport chunks do not reproduce application write boundaries, identify each writer, or separate stdout from stderr sharing a PTY. Complete scrollback contents and both screen buffers are not exposed. OS inspection is subject to platform and permission limits.

Observer callbacks must return promptly and honor your cancellation context. The queue permits 64 waiting events plus one executing callback, within a 16 MiB weighted metadata budget. The budget bounds that backlog rather than a single event: an event that arrives while nothing is waiting or executing is admitted at any weight, so a large terminal's snapshot still reaches the observer when it alone exceeds 16 MiB. Until that event's callback returns, every further observed event overflows. Overflow ends the session with `ErrObservationOverflow`. A callback panic ends the session as cancellation does, and the dispatcher discards every remaining event; `Run` returns an error with the panic value and stack, wrapping the value when it is an `error`. A callback that calls `runtime.Goexit`, as `t.FailNow`, `t.Fatal`, and `t.SkipNow` do, ends the session the same way; `Run` returns an error that names `runtime.Goexit`, with the callback's stack. Shutdown restores the terminal, drains admitted observations, and joins the dispatcher; a callback that never returns prevents completion.

# Compatibility

The child receives `TERM=xterm-256color` and `COLORTERM=truecolor` in place of the outer terminal's values. The frame also removes these outer-terminal variables, each read by at least one known program to choose something the child's endpoint does not provide, such as Kitty graphics, Sixel, iTerm2 inline images, VTE's shell-integration sequences, Ghostty's terminfo on ssh hosts, or OSC 52 clipboard writes: `TERM_PROGRAM`, `LC_TERMINAL`, `KONSOLE_VERSION`, `VTE_VERSION`, `KITTY_WINDOW_ID`, `KITTY_PID`, `GHOSTTY_RESOURCES_DIR`, `GHOSTTY_BIN_DIR`, `GHOSTTY_SHELL_FEATURES`, `WEZTERM_EXECUTABLE`, `WEZTERM_PANE`, `ITERM_SESSION_ID`, `ITERM_PROFILE`, `TERM_SESSION_ID`, `XTERM_VERSION`, `MLTERM`, `TERMINAL_NAME`, `EAT_SHELL_INTEGRATION_DIR`, `WARP_HONOR_PS1`, `WARP_SESSION_ID`, `WARP_TERMINAL_SESSION_UUID`, `WARP_IS_LOCAL_SHELL_SESSION`, `VSCODE_INJECTION`, `TABBY_CONFIG_DIRECTORY`, `CURSOR_TRACE_ID`, and `WT_SESSION`. The version or feature report of a removed identity goes with it, since it describes the same terminal: `TERM_PROGRAM_VERSION`, `LC_TERMINAL_VERSION`, and iTerm2's `TERM_FEATURES`. The list is not exhaustive, and other inherited variables can still steer programs. Variables that only name the outer terminal, such as `TERMINAL_EMULATOR`, pass through, and so does `VSCODE_GIT_ASKPASS_MAIN`: some programs read it to trust OSC 52 writes, but VS Code's git askpass helper runs it. `COLUMNS` and `LINES` are removed too: they hold the outer terminal's size, and ncurses prefers them to the viewport's PTY size and then ignores resizes. Multiplexer and remote-control handles such as `TMUX`, `TMUX_PANE`, `WEZTERM_UNIX_SOCKET`, `KITTY_LISTEN_ON`, and `ALACRITTY_SOCKET` pass through, so `tmux`, `wezterm cli`, `kitten @`, and `alacritty msg` in the child can still reach the outer multiplexer or terminal; programs can also read them to choose tmux- or Alacritty-specific output. Because `WEZTERM_PANE`, `KITTY_WINDOW_ID`, and `ITERM_SESSION_ID` are removed, remote-control commands in the child no longer default to the frame's own pane or window and need an explicit target, such as `wezterm cli --pane-id`. Every other variable passes through unchanged.

Startup requires DEC mode-query replies and an outer alternate screen (mode 1049) that the terminal reports as reset, meaning inactive and switchable. An existing alternate screen is rejected because its contents cannot be recovered from the TTY. A terminal that reports mode 1049 as permanently set, permanently reset, or not recognized is rejected because the frame cannot switch its screen with that mode: the session would draw over the screen the terminal shows and could not restore it.

| Capability | Behavior and limits |
| :--- | :--- |
| Keyboard, paste, focus | The outer terminal runs the child's keyboard modes, and keys reach the child as the terminal sends them. A mode the terminal cannot switch, such as Kitty keyboard flags or modifyOtherKeys without reported support or a mode reported as permanently set or reset, reaches the child in the terminal's form, and keys typed while the child changes modes arrive in the previous form. While the terminal has not reported Kitty keyboard support, the child's Kitty keyboard query (`CSI ? u`) gets no reply and emits no `Protocol` event, so the child does not enable keys the terminal never sends; a Kitty reply that arrives after the startup probe makes later queries answered. Legacy input lacks some physical key identities and release phases. Bare Escape ambiguity uses a 50 ms deadline. Focus reaches children that request it. |
| Mouse | Coordinates are localized to the child. The outer terminal reports the mouse in the child's active tracking mode, `Terminal.Native.MouseTrackingMode`: exactly one of modes 9, 1000, 1002, and 1003 is on, or none. While tracking is on, it reports in SGR (mode 1006) or, where pixel reports are available, SGR-pixel (mode 1016) format. Each of these mutually exclusive groups, tracking modes and report formats (1005, 1006, 1015, and 1016), is written with its resets first and the active mode last, because a terminal such as Ghostty turns tracking off or falls back to X10 reports on any reset in the group. On exit, the tracking mode and report format the terminal reported as set at startup are set again. A terminal such as Ghostty can report several modes of a group as set, because it keeps the mode a later set replaced; which of them was active cannot be queried, so the frame leaves the highest-numbered one active on exit. A terminal that received `CSI ? 1002 h` and then `CSI ? 1000 h` before startup gets button-event tracking (1002) back rather than normal tracking (1000). Mouse events reach the child only while it has an active tracking mode. The outer terminal turns wheel steps into arrow keys (alternate scroll, mode 1007) only while the child is on its alternate screen with alternate scroll set and no active tracking mode, so the wheel over a primary-screen child sends no arrow keys. While that conversion is on, the outer terminal applies it anywhere in its window, so wheel steps over the header, footer, side regions, or border also become arrow keys for the child. A terminal that does not report mode 1007 as switchable keeps its own wheel behavior. Pixel reports require measured geometry and verified support. A terminal that always reports pixels gets no mouse tracking until its cell size is measured; its pixel reports that arrive before then are dropped like outside reports. Frame-origin gestures and outside releases are not clamped into the child; a dropped outside release can leave a held button there. Without SGR or SGR-pixel reports (modes 1006 and 1016), the outer terminal's releases name no button, and neither does an SGR release with button code 3. Each such release ends every held gesture; inside the child, the child receives a release for each button it holds. Highlight tracking is unsupported. A mouse event the child's encoding cannot represent is withheld from the child, and the session continues: buttons 10 and 11, the back and forward buttons (8 and 9) for UTF-8 encoding (mode 1005), a coordinate beyond 223 for X10 encoding or 2015 for UTF-8 encoding, and a cell report for a child that requested pixel reports (mode 1016) while the outer terminal reports cells. Observers receive a `Routed` event with origin `unroutable-input`, the outer terminal's bytes, and the reason in `Event.Error`. A withheld release can leave a held button in the child. |
| Text display | Text, colors, supported rendition, hyperlinks, cursor, alternate screens, and synchronized updates are composed. The child starts with, and resets to, the width rule the session uses on the outer terminal, with or without terminal inheritance: grapheme clusters when the terminal reports mode 2027 as set, reset, or permanently set, and wcwidth otherwise. The session switches a reset mode 2027 on and restores it on exit. Font/shaping differences and unavailable grapheme-width agreement limit Unicode fidelity. Overline cannot be rendered by the selected cell style; hyperlink URLs are available, but OSC 8 parameter/id metadata is not exposed. |
| Child geometry | Resizes update the viewport and PTY. A viewport that cannot fit returns `ErrViewportTooSmall`. A child-requested grid outside its allocation returns `*frame.GeometryError`, matching `frame.ErrGeometry`. |
| Graphics and effects | Kitty graphics is disabled and Sixel is unsupported. Clipboard requests are observed and receive unsupported replies. Notifications and progress remain observations; bells ring the outer terminal. Child title and cwd hints remain metadata. |

The library confines a text terminal display; it does not reproduce every terminal extension or application's internal state. See the [architecture reference](reports/TUI%20frame%20architecture%20research.md) for protocol boundaries and the [verification record](docs/internal/references/verification.md) for runtime and packaging evidence. Local runtime checks cover macOS; Linux binaries are cross-built and audited, with no local Linux runtime-test claim.

# Demo App

The [demo app](cmd/tui-frame) wraps the executable and complete argument list after `--`:

```sh
./bin/tui-frame -- nvim --clean
```

It reserves a three-row header and two-row footer, showing child metadata alongside interactive Lip Gloss layouts.

The GIF above shows the frame's three layouts, red/navy/teal header backgrounds, and live border changes around [Yazi](https://yazi-rs.github.io/). Its bottom-right keycaps show actual key-down (`↓`) and key-up (`↑`) events, highlighting held keys and dimming released keys. The [VHS tape](demos/yazi.tape) sends actual shortcuts and uses the bundled [sample workspace](demos/fixtures/workspace) and an isolated [Yazi configuration](demos/yazi). See the [recording setup](docs/internal/references/demo-recording.md) to reproduce it.

Use `--showcase` to play the frame changes automatically, one action every two seconds:

```sh
./bin/tui-frame --showcase -- yazi
```

Playback runs once and leaves the child running. It uses the same frame actions as keyboard capture, which remains active throughout. It also demonstrates application-driven updates without keyboard input.

| Key | Behavior |
| :--- | :--- |
| Ctrl+B, then 1 | Cycle the Signal bar, Layered badge, and Bordered card layouts. |
| Ctrl+B, then 2 | Cycle the header background through red, navy, and teal. |
| Ctrl+B, then 3 | Toggle the child border and resize its viewport. |
| Ctrl+B, then Ctrl+B | Send one Ctrl+B to the child. |
| Ctrl+Q | Exit the demo. |

These bindings belong to the demo's explicit capture handler and work in every terminal, since legacy input already tells Ctrl+B, Ctrl+Q, and digits apart. After Ctrl+B, Ctrl+Q still quits, since a key with no binding after the prefix falls through to the bindings without it, as in tmux; any other key ends the prefix and is discarded. Reported key releases and repeats and lone modifier or lock keys leave it waiting. Keys outside the prefix, other than Ctrl+Q, pass to the child. `AGENT=1` uses plain frame text and starts without a border; Ctrl+B, then 3, can enable it.

Read the [demo usage guide](cmd/tui-frame/SKILL.md), [drawing code](cmd/tui-frame/demo.go), and [session/key-handling code](cmd/tui-frame/session.go). Executable build instructions are in the [native build setup](docs/internal/references/native-build.md).

# License

[MIT License](LICENSE) © 2026 Alex Gorbatchev.
