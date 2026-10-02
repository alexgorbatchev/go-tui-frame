package process

import (
	"bufio"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func TestReadNativeEmptyFirstArgument(t *testing.T) {
	s := nativeMember(t, "", append(os.Environ(), "FRAME_PROCESS_ROLE=member"))
	if !s.Args.Available || len(s.Args.Value) != 2 || s.Args.Value[0] != "" || s.Args.Value[1] != "-test.run=^TestProcessChild$" {
		t.Fatalf("empty first argument incorrectly observed (available=%v, count=%d, error=%v)", s.Args.Available, len(s.Args.Value), s.Args.Error)
	}
}

func nativeMember(t *testing.T, first string, env []string) Snapshot {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestProcessChild$")
	cmd.Args[0] = first
	cmd.Env = env
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := input.Close(); err != nil {
			t.Error(err)
		}
		if err := cmd.Wait(); err != nil {
			t.Error(err)
		}
	})
	if _, err := bufio.NewReader(output).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	return Read(cmd.Process.Pid)
}

func TestReadNativeEnvironmentAlignment(t *testing.T) {
	const alignmentVariants = 8
	for n := range alignmentVariants {
		t.Run(strconv.Itoa(n), func(t *testing.T) {
			env := []string{"FRAME_PROCESS_ROLE=member", "FRAME_PAD=" + strings.Repeat("x", n)}
			s := nativeMember(t, os.Args[0], env)
			if !s.Environment.Available || !slices.Equal(s.Environment.Value, env) {
				t.Fatalf("native environment differs from exact controlled vector: available=%v count=%d, expected2, error=%v", s.Environment.Available, len(s.Environment.Value), s.Environment.Error)
			}
		})
	}
}
