package frame

import (
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

	"github.com/alexgorbatchev/go-tui-frame/internal/emulator"
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
		// child is the sh script the program runs as nvim.
		child string
		// quitAfter is child output after which the case presses Ctrl+Q; empty
		// lets the child exit on its own.
		quitAfter string
		code      int
		stderr    *regexp.Regexp
	}{
		{
			name:      "Ctrl+Q",
			child:     "printf started; exec sleep 30",
			quitAfter: "started",
			stderr:    regexp.MustCompile(`^$`),
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
			name:      "Ctrl+Q with a later session error",
			child:     `trap 'printf "\033[?40h\033[?3h"' TERM; printf started; while :; do sleep 1; done`,
			quitAfter: "started",
			code:      1,
			stderr:    regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(ErrGeometry.Error()) + `: `),
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			p := &readmeProgram{cmd: exec.CommandContext(ctx, binary), exited: make(chan struct{})}
			p.cmd.Env = readmeChildEnv(t, tt.child)
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
			if tt.quitAfter != "" {
				p.awaitText(ctx, t, h, tt.quitAfter)
				pressCtrlQ(t, h)
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
// runs script with sh, ignoring the arguments it receives.
func readmeChildEnv(t *testing.T, script string) []string {
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
	return append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// pressCtrlQ makes the outer terminal report a Ctrl+Q press under the
// keyboard protocol the frame's capture negotiated.
func pressCtrlQ(t *testing.T, h *terminalHarness) {
	t.Helper()
	awaitOuter(t, h, "the capture keyboard flags", func(s emulator.State) bool {
		return s.KittyKeyboardFlags&ghostty.KittyKeyDisambiguate != 0
	})
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
	h.mu.Lock()
	report, err := h.em.EncodeKey(event, ghostty.OptionAsAltTrue)
	h.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := unix.Write(h.fd, report); err != nil {
		t.Fatal(err)
	}
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
