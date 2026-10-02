package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

func executeCommand(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	cmd, err := newRootCommand()
	if err != nil {
		t.Fatal(err)
	}
	var out, diagnostic bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&diagnostic)
	cmd.SetArgs(args)
	err = cmd.ExecuteContext(context.Background())
	return out.String(), diagnostic.String(), err
}

func TestHelpModesAndGeneratedCommands(t *testing.T) {
	tests := []struct {
		name  string
		mode  string
		args  []string
		agent bool
	}{
		{"human root", "", []string{"--help"}, false},
		{"human skill", "0", []string{"skill", "--help"}, false},
		{"agent root", "1", []string{"--help"}, true},
		{"agent skill", "true", []string{"skill", "--help"}, true},
		{"agent generated help", "yes", []string{"help", "skill"}, true},
		{"agent generated completion", " TRUE ", []string{"completion", "--help"}, true},
		{"agent generated shell", "1", []string{"completion", "bash", "--help"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("AGENT", tt.mode)
			t.Setenv("COLUMNS", "80")
			out, diagnostic, err := executeCommand(t, tt.args...)
			if err != nil || diagnostic != "" {
				t.Fatalf("help: err=%v stderr=%q", err, diagnostic)
			}
			if strings.HasPrefix(out, agentHelpAlert+"\n") != tt.agent {
				t.Fatalf("wrong help mode: %q", out)
			}
			if tt.agent && (!strings.Contains(out, "command: tui-frame") || strings.Contains(out, "╰─")) {
				t.Fatalf("agent help replaced native rendering: %q", out)
			}
			if tt.name == "human root" && (!strings.Contains(out, "╰─ skill") || strings.Contains(out, "╰─ completion")) {
				t.Fatalf("wrong human command tree: %q", out)
			}
		})
	}
}

func TestVersionIsRawInBothModes(t *testing.T) {
	for _, mode := range []string{"", "1"} {
		t.Run("AGENT="+mode, func(t *testing.T) {
			t.Setenv("AGENT", mode)
			out, diagnostic, err := executeCommand(t, "--version")
			if err != nil || diagnostic != "" || out != version+"\n" {
				t.Fatalf("version: stdout=%q stderr=%q err=%v", out, diagnostic, err)
			}
		})
	}
}

func TestMissingCommandAndFlagsReportUsageOnStderr(t *testing.T) {
	for _, args := range [][]string{nil, {"--"}, {"nvim"}, {"--bogus"}, {"skill", "extra"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			t.Setenv("AGENT", "1")
			out, diagnostic, err := executeCommand(t, args...)
			if err == nil || out != "" || !strings.Contains(diagnostic, agentHelpAlert) {
				t.Fatalf("invalid args: stdout=%q stderr=%q err=%v", out, diagnostic, err)
			}
		})
	}
}

func TestNativeHelpHonorsTerminalWidth(t *testing.T) {
	for _, width := range []int{1, 3, 20, 64} {
		t.Run(strconv.Itoa(width), func(t *testing.T) {
			t.Setenv("AGENT", "0")
			t.Setenv("COLUMNS", strconv.Itoa(width))
			out, diagnostic, err := executeCommand(t, "--help")
			if err != nil || diagnostic != "" {
				t.Fatalf("help: err=%v stderr=%q", err, diagnostic)
			}
			for _, line := range strings.Split(out, "\n") {
				if got := lipgloss.Width(line); got > width {
					t.Errorf("help line uses %d cells: %q", got, line)
				}
			}
		})
	}
	t.Setenv("AGENT", "1")
	t.Setenv("COLUMNS", "1")
	out, _, err := executeCommand(t, "--help")
	if err != nil || !strings.Contains(out, "Child arguments, passed unchanged") {
		t.Fatalf("agent help was clipped: %v, %q", err, out)
	}
}

func TestSkillOutputAndWriteFailures(t *testing.T) {
	want, err := os.ReadFile("SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"0", "1"} {
		t.Run("AGENT="+mode, func(t *testing.T) {
			t.Setenv("AGENT", mode)
			out, diagnostic, err := executeCommand(t, "skill")
			if err != nil || diagnostic != "" || out != string(want) {
				t.Fatalf("skill mismatch: err=%v stderr=%q", err, diagnostic)
			}
		})
	}
	f, err := os.CreateTemp(projectTempDir(t), "closed-output")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	cmd, err := newRootCommand()
	if err != nil {
		t.Fatal(err)
	}
	cmd.SetOut(f)
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{"skill"})
	if err := cmd.Execute(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("skill hid output failure: %v", err)
	}
}

func projectTempDir(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", filepath.Join(root, ".tmp"))
	return t.TempDir()
}

func TestHelpAndUsageReportActualWriteFailures(t *testing.T) {
	t.Setenv("AGENT", "1")
	f, err := os.CreateTemp(projectTempDir(t), "closed-help-output")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	for _, usage := range []bool{false, true} {
		cmd, err := newRootCommand()
		if err != nil {
			t.Fatal(err)
		}
		var diagnostic bytes.Buffer
		cmd.SetOut(f)
		cmd.SetErr(&diagnostic)
		if usage {
			cmd.SetErr(f)
			if err := cmd.Usage(); !errors.Is(err, os.ErrClosed) {
				t.Fatalf("usage hid alert write failure: %v", err)
			}
		} else {
			cmd.SetArgs([]string{"--help"})
			if err := cmd.Execute(); err != nil || !strings.Contains(diagnostic.String(), "write agent help alert") {
				t.Fatalf("help callback did not report write failure: %v, %q", err, diagnostic.String())
			}
		}
	}
}

func TestCobraPreservesTheChildCommandLine(t *testing.T) {
	for _, child := range []string{"skill", "help", "nvim"} {
		t.Run(child, func(t *testing.T) {
			cmd, err := newRootCommand()
			if err != nil {
				t.Fatal(err)
			}
			want := []string{child, "--help", "word two", "", "--", "-v", "$literal"}
			selected, args, err := cmd.Find(append([]string{"--"}, want...))
			if err != nil || selected != cmd {
				t.Fatalf("child was dispatched as a wrapper command: %v", err)
			}
			if err := selected.ParseFlags(args); err != nil {
				t.Fatal(err)
			}
			got := selected.Flags().Args()
			if err := selected.ValidateArgs(got); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, want) {
				t.Fatalf("child args = %#v, want %#v", got, want)
			}
		})
	}
}

func TestShowcaseFlagPreservesChildFlags(t *testing.T) {
	cmd, err := newRootCommand()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.ParseFlags([]string{"--showcase", "--", "yazi", "--showcase", "word two"}); err != nil {
		t.Fatal(err)
	}
	args := cmd.Flags().Args()
	if err := cmd.ValidateArgs(args); err != nil {
		t.Fatal(err)
	}
	showcase, err := cmd.Flags().GetBool("showcase")
	if err != nil || !showcase || !slices.Equal(args, []string{"yazi", "--showcase", "word two"}) {
		t.Fatalf("showcase=%v, child args=%q, error=%v", showcase, args, err)
	}
}

func TestEmbeddedReferenceCoversTheLiveInterface(t *testing.T) {
	cmd, err := newRootCommand()
	if err != nil {
		t.Fatal(err)
	}
	cmd.InitDefaultHelpCmd()
	cmd.InitDefaultCompletionCmd()
	cmd.InitDefaultVersionFlag()
	var walk func(*cobra.Command)
	walk = func(c *cobra.Command) {
		if c.Hidden {
			return
		}
		if !strings.Contains(skill, c.CommandPath()) {
			t.Errorf("operating guide omits %q", c.CommandPath())
		}
		c.InitDefaultHelpFlag()
		c.LocalNonPersistentFlags().VisitAll(func(f *pflag.Flag) {
			for _, text := range []string{"--" + f.Name, "`" + f.DefValue + "`"} {
				if !strings.Contains(skill, text) {
					t.Errorf("operating guide omits %q for %s", text, c.CommandPath())
				}
			}
			if f.Shorthand != "" && !strings.Contains(skill, "-"+f.Shorthand) {
				t.Errorf("operating guide omits shorthand for %s --%s", c.CommandPath(), f.Name)
			}
			if !strings.Contains(strings.ToLower(skill), f.Value.Type()) {
				t.Errorf("operating guide omits type for %s --%s", c.CommandPath(), f.Name)
			}
		})
		for _, child := range c.Commands() {
			walk(child)
		}
	}
	walk(cmd)
}

func TestSkillIsEmbeddedAndWorksWithoutRepositoryFiles(t *testing.T) {
	dir := projectTempDir(t)
	binary := buildExample(t)
	for _, mode := range []string{"0", "1"} {
		t.Setenv("AGENT", mode)
		cmd := exec.Command(binary, "skill")
		cmd.Dir = dir
		out, err := cmd.Output()
		if err != nil || string(out) != skill {
			t.Fatalf("offline skill AGENT=%s: err=%v output=%q", mode, err, out)
		}
	}
}

func buildExample(t *testing.T) string {
	t.Helper()
	binary := filepath.Join(projectTempDir(t), "tui-frame")
	args := []string{"build", "-o", binary}
	if os.Getenv("GOOS") == "linux" || runtime.GOOS == "linux" {
		args = append(args, "-ldflags=-linkmode=external -extldflags=-static")
	}
	build := exec.Command("go", append(args, ".")...)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build example executable: %v\n%s", err, out)
	}
	return binary
}
