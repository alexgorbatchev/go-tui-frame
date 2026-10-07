package frame

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	ghostty "go.mitchellh.com/libghostty"
	"golang.org/x/sys/unix"
)

// TestReadmeExamples builds each program the README's Go examples compose
// and runs the Keyboard Capture program on a native terminal.
func TestReadmeExamples(t *testing.T) {
	doc, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatal(err)
	}
	program := readmeGoExample(t, string(doc), "package main")
	call, imports := "result, err := app.Run(context.Background())", "import (\n"
	if !strings.Contains(program, call) || !strings.Contains(program, imports) {
		t.Fatal("README session call or imports changed; update example composition")
	}
	// Keyboard Capture adds the errors import, replaces the Run call, and adds
	// package-level declarations.
	capture := strings.Replace(program, imports, imports+"\t\"errors\"\n", 1)
	capture = strings.Replace(capture, call, readmeGoExample(t, string(doc), "ctx, cancel"), 1) +
		"\n\n" + readmeGoExample(t, string(doc), "var errQuit") + "\n"
	for _, tt := range []struct {
		name, code string
		// run checks the built program's behavior; nil only builds it.
		run func(t *testing.T, binary string)
	}{
		{"showcase", program, nil},
		{"capture", capture, runReadmeCapture},
		{"plain", strings.Replace(program, call, readmeGoExample(t, string(doc), "app.Header")+"\n"+call, 1), nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := readmeTempDir(t)
			source := filepath.Join(dir, "main.go")
			if err := os.WriteFile(source, []byte(tt.code), 0o600); err != nil {
				t.Fatal(err)
			}
			binary := filepath.Join(dir, "example")
			args := []string{"build", "-o", binary}
			if runtime.GOOS == "linux" {
				// The native build setup links Linux consumers statically with musl.
				args = append(args, "-ldflags=-linkmode=external -extldflags=-static")
			}
			cmd := exec.CommandContext(t.Context(), "go", append(args, source)...)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("build README example: %v\n%s", err, out)
			}
			if tt.run != nil {
				tt.run(t, binary)
			}
		})
	}
}

// runReadmeCapture runs the Keyboard Capture program as a user does, with
// standard input and output on a native terminal. Each case installs its
// child as the nvim the program finds on PATH, so the documented source runs
// unchanged.
func runReadmeCapture(t *testing.T, binary string) {
	for _, tt := range []struct {
		name string
		// child is the sh script the program runs as nvim. It prints started
		// once it runs, and finds its own directory, which it shares with
		// readmeProgram.dir, as ${0%/*}.
		child string
		// quit presses Ctrl+Q; nil lets the child exit on its own.
		quit   func(ctx context.Context, t *testing.T, h *terminalHarness, p *readmeProgram)
		code   int
		stderr *regexp.Regexp
	}{
		{
			// The quit's SIGTERM ends the child, and the program reports that
			// outcome rather than the quit.
			name:   "Ctrl+Q",
			child:  "printf started; exec sleep 30",
			quit:   quitWhenStarted,
			code:   1,
			stderr: regexp.MustCompile(`^signal: terminated\n$`),
		},
		{
			// sh runs a trap only after its foreground command ends, which a
			// sleep forked as the SIGTERM arrives can delay past the frame's
			// SIGKILL. The wait builtin returns for a trapped signal at once.
			name:   "Ctrl+Q to a child that exits 0 on SIGTERM",
			child:  "trap 'exit 0' TERM; printf started; sleep 30 & wait",
			quit:   quitWhenStarted,
			stderr: regexp.MustCompile(`^$`),
		},
		{
			name:   "Ctrl+Q held until the child exited",
			child:  `stty raw -echo; printf started; while [ ! -e "${0%/*}/exit" ]; do sleep 0.01; done; exit 7`,
			quit:   quitAfterChildExit,
			code:   1,
			stderr: regexp.MustCompile(`^exit status 7\n$`),
		},
		{
			name:   "child exit status",
			child:  "exit 7",
			code:   1,
			stderr: regexp.MustCompile(`^exit status 7\n$`),
		},
		{
			// The child answers the quit's SIGTERM by switching to 132 columns
			// (DECCOLM), which the viewport cannot fit, so a session error
			// follows the quit.
			name:   "Ctrl+Q with a later session error",
			child:  `trap 'printf "\033[?40h\033[?3h"' TERM; printf started; while :; do sleep 1 & wait $!; done`,
			quit:   quitWhenStarted,
			code:   1,
			stderr: regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(ErrGeometry.Error()) + `: `),
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			// Fd makes the descriptor blocking, so it is taken before the
			// program, which shares the terminal's file description, starts.
			p := &readmeProgram{cmd: exec.CommandContext(ctx, binary), exited: make(chan struct{}), terminal: int(h.slave.Fd())}
			p.cmd.Env, p.dir = readmeChildEnv(t, tt.child)
			p.cmd.Stdin, p.cmd.Stdout, p.cmd.Stderr = h.slave, h.slave, &p.stderr
			if err := p.cmd.Start(); err != nil {
				t.Fatal(err)
			}
			go func() {
				defer close(p.exited)
				p.err = p.cmd.Wait()
			}()
			t.Cleanup(func() {
				cancel()
				<-p.exited
				if t.Failed() {
					t.Logf("README program %s", p.outcome())
				}
			})
			if tt.quit != nil {
				tt.quit(ctx, t, h, p)
			}
			<-p.exited
			if code := p.cmd.ProcessState.ExitCode(); code != tt.code || !tt.stderr.MatchString(p.stderr.String()) {
				t.Fatalf("README program %s, want status %d and stderr matching %q", p.outcome(), tt.code, tt.stderr)
			}
		})
	}
}

// readmeProgram is a README program running on a harness terminal.
type readmeProgram struct {
	cmd    *exec.Cmd
	stderr strings.Builder
	// exited is closed when Wait returns; err and stderr are final from then.
	exited chan struct{}
	err    error
	// terminal is the descriptor of the program's terminal, the harness slave.
	terminal int
	// dir holds the child's script.
	dir string
}

// quitWhenStarted presses Ctrl+Q once the child has started.
func quitWhenStarted(ctx context.Context, t *testing.T, h *terminalHarness, p *readmeProgram) {
	t.Helper()
	p.awaitText(ctx, t, h, "started")
	pressCtrlQ(t, h)
}

// quitAfterChildExit presses Ctrl+Q while the child runs, but the frame holds
// the key until the child has exited and been reaped, and only then passes it
// to capture.
//
// The child puts its terminal in raw mode and never reads, so the frame's
// input queue fills. A paste leaves less room in that queue than the APC
// string that follows it, and the frame frames that string only once its
// terminator arrives. The terminator and Ctrl+Q are written together and so
// read together: both are held behind the string, and outer reads pause until
// the child exits. The child exits once the frame has read them.
func quitAfterChildExit(ctx context.Context, t *testing.T, h *terminalHarness, p *readmeProgram) {
	t.Helper()
	p.awaitText(ctx, t, h, "started")
	const room = 8 << 10
	fill := bracketedPaste(inputQueueLimit - room)
	held := append([]byte("\x1b_"), bytes.Repeat([]byte("x"), 32*room)...)
	// String Terminator, then Ctrl+Q.
	quit := append([]byte("\x1b\\"), ctrlQReport(t, h)...)
	for _, input := range [][]byte{fill, held, quit} {
		p.send(ctx, t, h, input)
	}
	if err := os.WriteFile(filepath.Join(p.dir, "exit"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
}

// send makes the outer terminal send input and waits until the program has
// read all of it. It fails as soon as the program exits or ctx ends.
func (p *readmeProgram) send(ctx context.Context, t *testing.T, h *terminalHarness, input []byte) {
	t.Helper()
	_, written := feedOuter(t, h.master, input)
	select {
	case err := <-written:
		if err != nil {
			t.Fatal(err)
		}
	case <-p.exited:
		t.Fatalf("README program %s before its terminal sent its input", p.outcome())
	case <-ctx.Done():
		t.Fatalf("terminal could not send its input when the case ended: %v", ctx.Err())
	}
	poll := time.NewTicker(time.Millisecond)
	defer poll.Stop()
	// A zero-timeout poll reports unread input without consuming it.
	for inputWaiting(t, p.terminal, 0) {
		select {
		case <-p.exited:
			t.Fatalf("README program %s before it read its input", p.outcome())
		case <-ctx.Done():
			t.Fatalf("README program left its input unread when the case ended: %v", ctx.Err())
		case <-poll.C:
		}
	}
}

// outcome describes how the program ended. The caller has received from
// exited.
func (p *readmeProgram) outcome() string {
	return fmt.Sprintf("exited %d (%v) with stderr %q", p.cmd.ProcessState.ExitCode(), p.err, p.stderr.String())
}

// awaitText waits until the display of h, the program's terminal, shows want.
// It fails as soon as the program exits, reporting how it ended, or ctx ends.
func (p *readmeProgram) awaitText(ctx context.Context, t *testing.T, h *terminalHarness, want string) {
	t.Helper()
	poll := time.NewTicker(10 * time.Millisecond)
	defer poll.Stop()
	for !strings.Contains(h.text(), want) {
		select {
		case <-p.exited:
			t.Fatalf("README program %s while the case waited for %q on the outer display: %q", p.outcome(), want, h.text())
		case <-ctx.Done():
			t.Fatalf("outer display missing %q when the case ended (%v): %q", want, ctx.Err(), h.text())
		case <-poll.C:
		}
	}
}

// readmeChildEnv returns this process's environment with a PATH whose nvim
// runs script with sh, ignoring the arguments it receives, and the directory
// that holds that nvim.
func readmeChildEnv(t *testing.T, script string) (env []string, dir string) {
	t.Helper()
	// exec.LookPath rejects a relative PATH entry's result.
	dir, err := filepath.Abs(readmeTempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "nvim"), []byte("#!/bin/sh\n"+script+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	// The last PATH entry wins in an exec.Cmd environment.
	return append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH")), dir
}

// pressCtrlQ makes the outer terminal report a Ctrl+Q press.
func pressCtrlQ(t *testing.T, h *terminalHarness) {
	t.Helper()
	if _, err := unix.Write(h.fd, ctrlQReport(t, h)); err != nil {
		t.Fatal(err)
	}
}

// ctrlQReport returns the outer terminal's report of a Ctrl+Q press under the
// keyboard modes the frame mirrored from its child.
func ctrlQReport(t *testing.T, h *terminalHarness) []byte {
	t.Helper()
	event, err := ghostty.NewKeyEvent()
	if err != nil {
		t.Fatal(err)
	}
	defer event.Close()
	event.SetAction(ghostty.KeyActionPress)
	event.SetKey(ghostty.KeyQ)
	event.SetMods(ghostty.ModCtrl)
	event.SetUTF8("q")
	event.SetUnshiftedCodepoint('q')
	return outerKeyReport(t, h, event)
}

// outerKeyReport encodes event as the harness terminal reports it in its
// current keyboard modes, using the native key encoder.
func outerKeyReport(t *testing.T, h *terminalHarness, event *ghostty.KeyEvent) []byte {
	t.Helper()
	h.mu.Lock()
	s, err := h.em.State()
	h.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	encoder, err := ghostty.NewKeyEncoder()
	if err != nil {
		t.Fatal(err)
	}
	defer encoder.Close()
	encoder.SetOptKittyFlags(s.KittyKeyboardFlags)
	for _, opt := range []struct {
		option ghostty.KeyEncoderOption
		on     bool
	}{
		{ghostty.KeyEncoderOptModifyOtherKeysState2, s.ModifyOtherKeys2},
		{ghostty.KeyEncoderOptCursorKeyApplication, s.Modes[ghostty.ModeDECCKM]},
		{ghostty.KeyEncoderOptKeypadKeyApplication, s.Modes[ghostty.ModeKeypadKeys]},
		{ghostty.KeyEncoderOptIgnoreKeypadWithNumlock, s.Modes[ghostty.ModeNumlockKeypad]},
		{ghostty.KeyEncoderOptAltEscPrefix, s.Modes[ghostty.ModeAltEscPrefix]},
		{ghostty.KeyEncoderOptBackarrowKeyMode, s.Modes[ghostty.ModeBackarrowKeyMode]},
	} {
		encoder.SetOptBool(opt.option, opt.on)
	}
	report, err := encoder.Encode(event)
	if err != nil {
		t.Fatal(err)
	}
	return report
}

// readmeTempDir returns a new directory under the repository's .tmp that is
// removed when the test ends.
func readmeTempDir(t *testing.T) string {
	t.Helper()
	if err := os.MkdirAll(".tmp", 0o700); err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(".tmp", "readme-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Error(err)
		}
	})
	return dir
}

func readmeGoExample(t *testing.T, doc, prefix string) string {
	t.Helper()
	for _, part := range strings.Split(doc, "```go\n")[1:] {
		code, _, ok := strings.Cut(part, "```")
		if ok && strings.HasPrefix(code, prefix) {
			return strings.TrimSpace(code)
		}
	}
	t.Fatalf("README has no Go example starting with %q", prefix)
	return ""
}

func TestCoreDoesNotRequireLipGloss(t *testing.T) {
	cmd := exec.CommandContext(t.Context(), "go", "list", "-deps", ".")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("resolve core dependencies: %v\n%s", err, out)
	}
	for line := range strings.SplitSeq(string(out), "\n") {
		if strings.HasPrefix(line, "charm.land/lipgloss/") {
			t.Fatalf("core requires Lip Gloss: %s", line)
		}
	}
}
