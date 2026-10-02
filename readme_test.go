package frame

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadmeExamplesBuild(t *testing.T) {
	doc, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatal(err)
	}
	program := readmeGoExample(t, string(doc), "package main")
	call := "result, err := app.Run(context.Background())"
	if !strings.Contains(program, call) {
		t.Fatal("README session call changed; update example composition")
	}
	for _, tt := range []struct{ name, code string }{
		{"showcase", program},
		{"capture", strings.Replace(program, call, readmeGoExample(t, string(doc), "ctx, cancel"), 1)},
		{"plain", strings.Replace(program, call, readmeGoExample(t, string(doc), "app.Header")+"\n"+call, 1)},
	} {
		t.Run(tt.name, func(t *testing.T) {
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
			source := filepath.Join(dir, "main.go")
			if err := os.WriteFile(source, []byte(tt.code), 0o600); err != nil {
				t.Fatal(err)
			}
			cmd := exec.CommandContext(t.Context(), "go", "build", "-o", filepath.Join(dir, "example"), source)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("build README example: %v\n%s", err, out)
			}
		})
	}
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
