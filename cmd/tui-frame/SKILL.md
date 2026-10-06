---
name: tui-frame
description: >-
  REQUIRED when operating tui-frame, wrapping a child terminal application with
  tui-frame -- command, changing frame demos with the Ctrl+B prefix, or reading
  tui-frame help, version, skill, or shell completion output. Read this
  operating reference for this executable's exact arguments and keyboard
  capture. Use Go project instructions for developing the library instead.
author: alexgorbatchev
metadata:
  created_on: 2026-10-01 13:23
  last_modified: 2026-10-06 16:49
  status: current
---

## Run a child application

Use `tui-frame -- <command> [args...]` in a Linux or macOS terminal. Put the child
executable and all its arguments after `--`. Cobra ends wrapper option parsing
at that separator; the executable receives the remaining argument values
unchanged, including flags, spaces within an argument, and further `--` values.
The child runs directly through `exec.Command` with the inherited environment
and working directory. Shell expansion and pipelines belong to the invoking
shell; use an explicit child shell when shell syntax is required.

```sh
tui-frame -- nvim
tui-frame -- less README.md
tui-frame -- sh -c 'printf "hello\n"; read answer'
```

The initial demo is **Signal bar**, with a red header. **Layered badge** uses
positioned native Lip Gloss layers. **Bordered card** uses a native rounded
border. Each demo also changes the footer's native style or composition. Human
mode starts with three header rows, two footer rows, and a one-cell child border.
Agent mode starts with the same header and footer rows with plain text and no
border. The layout and header background have independent three-step cycles;
the background starts red, then cycles navy, teal, and back to red. Agent chrome
remains plain when cycling the background. Regions report
the child PID, executable name, terminal title, and child viewport size. Region
header/footer geometry stays fixed for the session. A child-border toggle adds
or removes a one-cell inset, resizes the child PTY, and repaints the frame. The
footer's first row shows the executable name, child viewport dimensions, current
border state, and terminal title. Its second row always shows the Ctrl+B prefix
and Ctrl+Q key hints. A row whose text does not fit ends in an ellipsis; the
Bordered card layout's left border leaves its rows one column narrower than the
footer.

| Key | Wrapper action |
| --- | --- |
| Ctrl+B, then 1 | Advance to the next layout, wrapping after the third |
| Ctrl+B, then 2 | Advance the header background independently: red, navy, teal |
| Ctrl+B, then 3 | Toggle the child frame border and resize the child PTY |
| Ctrl+B, then Ctrl+B | Send one Ctrl+B to the child |
| Ctrl+Q | Cancel the session and terminate observed process groups in the owned child session |

The wrapper consumes Ctrl+B, the digit that follows it, and Ctrl+Q, including
their reported key-release events. After Ctrl+B, a second Ctrl+B reaches the
child exactly as the terminal sent it, and Ctrl+Q still quits: as in tmux, a key
with no binding after the prefix falls through to the bindings without it. Any
other key ends the prefix and is discarded. Reported key releases and repeats
and lone modifier or lock keys leave the prefix waiting. Lock state does not
count as a modifier, so the controls work while Caps Lock or Num Lock is on.
Other keyboard events, such as plain digits and F5/F6 outside the prefix, follow
the library's child input route. Paste, mouse input, and unknown controls follow
ordinary routing. Child mouse coordinates follow the child viewport geometry.
The child can change terminal modes and responds to terminal resizing through
its PTY. The terminal runs the child's keyboard modes, and keys reach the child
exactly as the terminal sends them. The controls use keys every terminal reports
distinctly, so they work with legacy input. While the terminal has not reported
Kitty keyboard support, the child's Kitty keyboard query gets no reply. The
viewport paints terminal cells; it does not render inline graphics. Native Kitty
graphics storage is disabled, so its capability query does not advertise
successful image support.

## Play the frame showcase

Use `tui-frame --showcase -- yazi` to demonstrate the wrapper with an idle child.
After the child starts, apply one frame action every two seconds, in this order:
layout, background, background, border, layout, border, background, layout.
From the defaults this shows Layered badge in red, navy, and teal; removes the
child border; switches to Bordered card; restores the border; returns the header
to red; and finishes on Signal bar. Each border change resizes the actual child
PTY. Playback submits application updates through the same actions as capture;
it does not synthesize keyboard input or add a periodic rendering timer.

Run this sequence once per session. Leave the child running after playback;
use Ctrl+Q or the child's exit command to finish. Keyboard capture remains
active during playback. Actions advance from the current state, so manual
controls can change the sequence's resulting layouts and colors. Child exit,
startup failure, and cancellation stop and join playback before returning.
Agent mode keeps plain chrome during playback, including when changing colors.

## Terminal preferences

Terminal preference inheritance is enabled by default. At startup, reported
foreground/background colors, all 256 palette entries, cursor color/style/blink,
supported preference modes, keyboard settings, and light/dark color scheme seed
the child emulator. The child PTY receives the outer terminal's original line
discipline, including control characters, echo, and flow control. Missing or
invalid probe replies retain native defaults after a 300 ms deadline.
Default-colored cells use the outer terminal's default rendition. On a
256-color outer terminal, cells using an inherited palette entry the child has
not redefined use the outer terminal's palette index; a 16-color terminal does
this for entries 0-15. True-color terminals receive resolved RGB colors, and
colorless terminals, such as `NO_COLOR` or `TERM=dumb`, receive no color. On a
256-color terminal, two colors with the same xterm RGB value, such as SGR 30
and `38;2;0;0;0`, can keep the earlier color when adjacent or when one replaces
the other, including an OSC 4 redefinition to the xterm value or an OSC 104
reset from it. On a 16-color terminal this affects only entries 7 and 8 and a
color of their xterm values `#c0c0c0` and `#808080`, when either replaces the
other, including an OSC 4 redefinition to that value or an OSC 104 reset from
it (`charmbracelet/ultraviolet#205`).

Use `tui-frame --no-terminal-inheritance -- <command> [args...]` to select
emulator and PTY defaults. Capability and restoration probes still run. Fonts,
shaping, opacity, window settings, and terminal key mappings remain owned by the
outer terminal and cannot be disabled inside a viewport. Screen contents,
scrollback, margins, and application mouse tracking belong to the child session;
graphics and clipboard permissions retain the documented endpoint limits.
Preferences are captured at startup; live outer theme changes are not queried
again.

## Commands and options

| Command | Positional arguments | Result |
| --- | --- | --- |
| `tui-frame -- <command> [args...]` | Required executable; optional arbitrary child arguments | Interactive child PTY and frame |
| `tui-frame skill` | None | Entire embedded Markdown operating guide, including frontmatter |
| `tui-frame help [command...]` | Optional command path such as `completion bash` | Help for the selected command |
| `tui-frame completion` | None | Help for the shell completion group |
| `tui-frame completion bash` | None | Bash completion script on stdout |
| `tui-frame completion zsh` | None | Zsh completion script on stdout |
| `tui-frame completion fish` | None | Fish completion script on stdout |
| `tui-frame completion powershell` | None | PowerShell completion script on stdout; this does not add Windows runtime support |

| Option | Type | Default | Accepted values and scope |
| --- | --- | --- | --- |
| `--help`, `-h` | Boolean | `false` | `true`/`false`; every public command; show help without starting a child |
| `--version`, `-v` | Boolean | `false` | `true`/`false`; root only; print raw version and newline; development builds report `dev` |
| `--showcase` | Boolean | `false` | `true`/`false`; root only, before `--`; play the eight frame actions once with two-second spacing |
| `--no-terminal-inheritance` | Boolean | `false` | `true`/`false`; root only, before `--`; disable terminal preference and PTY inheritance |
| `--no-descriptions` | Boolean | `false` | `true`/`false`; each completion shell command; omit completion descriptions |

`skill` accepts no positional arguments or command-specific flags. It operates
offline using bytes embedded in the executable, with identical output in both
modes. Public help and completion commands are generated by Cobra. Hidden
`__complete [command-line]` and `__completeNoDesc [command-line]` are Cobra shell
protocol endpoints; their stdout contains completion candidates and a final
`:N` directive, with descriptions omitted by `__completeNoDesc`.

## Environment and output

| Variable | Effective behavior |
| --- | --- |
| `AGENT` | Trim whitespace and ignore case; `1`, `true`, or `yes` selects agent mode; every other value selects human mode |
| `COLUMNS` | A positive integer caps human help width; otherwise use detected stdout terminal width, falling back to 80 cells; agent help remains untruncated |
| `PATH` | Standard executable search for a child name without a path separator |
| `TERM`, `COLORTERM` | Child receives `xterm-256color` and `truecolor` to describe its virtual endpoint |
| `TERM_PROGRAM`, `TERM_PROGRAM_VERSION`, `KITTY_WINDOW_ID`, `GHOSTTY_RESOURCES_DIR`, `GHOSTTY_BIN_DIR`, `WEZTERM_PANE`, `ITERM_SESSION_ID` | Removed from the child environment because physical-terminal vendor hints misdescribe the virtual endpoint |
| Other variables | Inherited by the child process |

Human help uses a command tree and hides generated completion commands from
that tree. Agent help describes the full public interface and starts with
ALERT: Agents must read `AGENT=1 tui-frame skill` before using this tool.
Requested help, version, skill, and completion scripts go to stdout. Argument
and flag diagnostics with usage go to stderr. Agent diagnostics use `ERR:`;
human diagnostics use `[ERROR]`. The interactive viewport contains the child's
terminal output; agent mode changes wrapper chrome rather than child output.

## Exit status, errors, and side effects

Successful metadata commands return 0. Missing child arguments, child arguments
before `--`, extra `skill` arguments, and unknown flags return 2 with usage.
Generated completion commands use Cobra's validation; invalid positional
arguments return 1 with a diagnostic. Cobra's generated `help` resolves command
paths and displays the selected command's help, including the root when a
trailing topic does not resolve to a subcommand.
A child exiting normally returns its own exit code;
a signal termination maps to `128 + signal number`. The status alone reports
the child's result; the wrapper writes no diagnostic for it. Ctrl+Q follows the
same rule. It sends SIGTERM to the child's process groups, then SIGKILL one
second later if needed, and returns however the child ends: 143 when SIGTERM
ends it, 137 after SIGKILL, or the child's own code when it handles SIGTERM and
exits. A Ctrl+Q handled after the child has already exited returns that exit's
status. A failure while Ctrl+Q ends the session, such as painting the child's
final output, signalling its process groups, draining, or restoring the
terminal, is reported on stderr as `run child frame: <error>` and returns a
nonzero status. SIGHUP, SIGINT, or SIGTERM sent
to the wrapper cancels the session and returns `128 + signal number`: 129 for
SIGHUP, 130 for SIGINT, and 143 for SIGTERM. Stderr reports
`run child frame: <name> signal received`, where `<name>` is `hangup`,
`interrupt`, or `terminated`. Startup, terminal, drawing, drain,
restoration, cancellation, and output-write failures report their error once
on stderr after the `ERR:` or `[ERROR]` prefix and return a nonzero status.
Help rendering errors are reported on stderr through Cobra's help callback. A
terminal too small for the reserved regions produces the library's
viewport-size error.

The terminal must answer the alternate-screen status probe and report DEC mode
1049 as reset, meaning inactive and switchable, before the session starts. A
missing reply; a not-recognized, set (already active), permanently set, or
permanently reset report; mismatched input/output terminals; or another frame
owning the same terminal produces a startup error.

An interactive run starts a child in a PTY, changes terminal modes, paints the
frame, and restores terminal state on exit. While the child's cursor matches
what the terminal reported at startup, the frame writes no cursor style or
color sequences. A cursor style or color that differs is reset to the
terminal's defaults with `CSI 0 SP q` or `OSC 112`, so a cursor override that
was active before the run is lost. With `--no-terminal-inheritance`, the child
starts with the emulator's default cursor style, which differs from any other
reported style even when the child never changes its cursor. Cancellation
terminates observed process groups in the owned child session and waits for the
child. Closing the terminal window or tab ends terminal input and typically
delivers SIGHUP; either cancels the session, and restoration failures on the
closed terminal are added to the reported error. A SIGHUP received during that
shutdown returns 129 even when ended input stopped the session first. Normal
teardown also terminates remaining observed groups after the leader exits;
detached processes in new sessions are excluded. The child retains its ordinary
permissions and filesystem or network side effects. Metadata commands start no
child process. Completion commands print scripts; apply shell redirection or
sourcing explicitly when installing those scripts.

Read `AGENT=1 tui-frame skill`, then inspect `tui-frame --version` when matching
this reference to a deployed executable. Run `tui-frame -- <command> [args...]`
with a terminal available. Press Ctrl+B, then 1 for layout, 2 for header colour,
or 3 for the child border, or press Ctrl+Q to close the session.
