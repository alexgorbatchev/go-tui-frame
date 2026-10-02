package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestExecuteUsesNativeCobraAndChildExitStatuses(t *testing.T) {
	tests := []struct {
		name string
		args []string
		code int
	}{
		{"help", []string{"--help"}, 0},
		{"version", []string{"--version"}, 0},
		{"missing child", nil, 2},
		{"unknown flag", []string{"--bogus"}, 2},
		{"missing executable", []string{"--", "/nonexistent/go-tui-frame-child"}, 1},
		{"child success", []string{"--", "sh", "-c", "exit 0"}, 0},
		{"showcase child success", []string{"--showcase", "--", "sh", "-c", "exit 0"}, 0},
		{"showcase startup failure", []string{"--showcase", "--", "/nonexistent/go-tui-frame-child"}, 1},
		{"child failure", []string{"--", "sh", "-c", "exit 17"}, 17},
		{"child signal", []string{"--", "sh", "-c", "kill -TERM $$"}, 143},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, diagnostic := cliTTY(t, tt.args...)
			t.Setenv("AGENT", "1")
			if got := execute(); got != tt.code {
				message, readErr := os.ReadFile(diagnostic)
				t.Fatalf("execute returned %d, want %d: %s (%v)", got, tt.code, message, readErr)
			}
		})
	}
}

func TestExecuteCtrlQStopsItsRealChild(t *testing.T) {
	ready := filepath.Join(projectTempDir(t), "child-started")
	master, _ := cliTTY(t, "--showcase", "--", "sh", "-c", `printf started > "$1"; exec sleep 30`, "sh", ready)
	done := make(chan int, 1)
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		done <- execute()
	}()
	t.Cleanup(func() {
		if err := master.Close(); err != nil {
			t.Errorf("close CLI terminal: %v", err)
		}
		select {
		case <-exited:
		case <-time.After(sessionTimeout):
			t.Error("CLI did not stop after terminal closure")
		}
	})
	deadline := time.NewTimer(sessionTimeout)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-tick.C:
			if _, err := os.Stat(ready); err == nil {
				if _, err := master.Write([]byte{0x11}); err != nil {
					t.Fatal(err)
				}
				select {
				case got := <-done:
					if got != 0 {
						t.Fatalf("Ctrl+Q returned %d", got)
					}
					return
				case <-deadline.C:
					t.Fatal("Ctrl+Q did not finish the CLI")
				}
			} else if !os.IsNotExist(err) {
				t.Fatal(err)
			}
		case got := <-done:
			t.Fatalf("CLI exited before its child started: %d", got)
		case <-deadline.C:
			t.Fatal("CLI did not start its child")
		}
	}
}

func cliTTY(t *testing.T, args ...string) (*os.File, string) {
	t.Helper()
	master, slave := demoTTY(t)
	diagnostic, err := os.CreateTemp(projectTempDir(t), "diagnostic")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := diagnostic.Close(); err != nil {
			t.Errorf("close diagnostic: %v", err)
		}
	})
	originalIn, originalOut, originalErr, originalArgs := os.Stdin, os.Stdout, os.Stderr, os.Args
	os.Stdin, os.Stdout, os.Stderr = slave, slave, diagnostic
	os.Args = append([]string{"tui-frame"}, args...)
	t.Cleanup(func() { os.Stdin, os.Stdout, os.Stderr, os.Args = originalIn, originalOut, originalErr, originalArgs })
	return master, diagnostic.Name()
}

func TestExecutePreservesEveryChildArgument(t *testing.T) {
	output := filepath.Join(projectTempDir(t), "child-argv")
	want := []string{"--help", "word two", "", "--", "-v", "$literal"}
	args := []string{"--", "sh", "-c", `out=$1; shift; printf '%s\000' "$@" > "$out"`, "sh", output}
	cliTTY(t, append(args, want...)...)
	if code := execute(); code != 0 {
		t.Fatalf("argv child returned %d", code)
	}
	got, err := os.ReadFile(output)
	if err != nil || string(got) != strings.Join(want, "\x00")+"\x00" {
		t.Fatalf("child argv changed: %q, %v", got, err)
	}
}

func TestRunDemoReportsItsNativeStartError(t *testing.T) {
	_, diagnostic, err := executeCommand(t, "--", "/nonexistent/go-tui-frame-child")
	if err == nil || !strings.Contains(err.Error(), "run child frame") || !strings.Contains(diagnostic, "run child frame") {
		t.Fatalf("startup error did not preserve context: %v, %q", err, diagnostic)
	}
}
