package process

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

func TestProcessChild(t *testing.T) {
	role := os.Getenv("FRAME_PROCESS_ROLE")
	if role == "" {
		return
	}
	if role == "member" {
		if dir := os.Getenv("FRAME_PROCESS_CWD"); dir != "" {
			if err := os.Chdir(dir); err != nil {
				os.Exit(91)
			}
		}
		if _, err := fmt.Fprintln(os.Stdout, "ready"); err != nil {
			os.Exit(92)
		}
		if _, err := io.Copy(io.Discard, os.Stdin); err != nil {
			os.Exit(93)
		}
		os.Exit(0)
	}
	child := exec.Command(os.Args[0], "-test.run=^TestProcessChild$")
	child.Env = append(os.Environ(), "FRAME_PROCESS_ROLE=member")
	child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	child.Stdin = os.Stdin
	output, err := child.StdoutPipe()
	if err != nil {
		os.Exit(94)
	}
	if err := child.Start(); err != nil {
		os.Exit(95)
	}
	reader := bufio.NewReader(output)
	if _, err := reader.ReadString('\n'); err != nil {
		os.Exit(96)
	}
	if _, err := fmt.Fprintf(os.Stdout, "%d\n", child.Process.Pid); err != nil {
		os.Exit(97)
	}
	if role == "exiting-leader" {
		os.Exit(0)
	}
	// Both descendants finish when the parent closes the shared pipe.
	if err := child.Wait(); err != nil {
		os.Exit(98)
	}
	os.Exit(0)
}

func TestGroupsSurviveSessionLeaderExit(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestProcessChild$")
	cmd.Env = append(os.Environ(), "FRAME_PROCESS_ROLE=exiting-leader")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	waited := false
	t.Cleanup(func() {
		if err := reader.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
			t.Error(err)
		}
		if err := writer.Close(); err != nil {
			t.Error(err)
		}
		if cmd.Process != nil && !waited {
			if err := cmd.Wait(); err != nil {
				t.Error(err)
			}
		}
	})
	cmd.Stdin = reader
	output, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(output).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	member, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	waited = true
	groups, err := Groups(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(groups, []int{member}) {
		t.Fatalf("live groups after leader exit = %v, want member %d", groups, member)
	}
	members, err := List(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	if len(members) != 1 || members[0].PID != member || !members[0].SessionID.Available || members[0].SessionID.Value != cmd.Process.Pid {
		t.Fatal("surviving session member unavailable after leader exit")
	}
}

func processFamily(t *testing.T) (int, int) {
	return processFamilyDir(t, "")
}

func processFamilyDir(t *testing.T, dir string) (int, int) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestProcessChild$")
	cmd.Env = append(os.Environ(), "FRAME_PROCESS_ROLE=leader")
	if dir != "" {
		cmd.Env = append(cmd.Env, "FRAME_PROCESS_CWD="+dir)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
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
	line, err := bufio.NewReader(output).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	member, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil {
		t.Fatal(err)
	}
	return cmd.Process.Pid, member
}

func TestGroupsEnumeratesAllOwnedSessionGroups(t *testing.T) {
	leader, member := processFamily(t)
	sid, err := unix.Getsid(leader)
	if err != nil {
		t.Fatal(err)
	}
	if sid != leader {
		t.Fatalf("native session = %d, leader %d", sid, leader)
	}
	groups, err := Groups(sid)
	if err != nil {
		t.Fatal(err)
	}
	want := []int{leader, member}
	slices.Sort(want)
	if !slices.Equal(groups, want) {
		t.Fatalf("session groups = %v, want [%d %d]", groups, leader, member)
	}
	parentGroup, err := unix.Getpgid(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(groups, parentGroup) {
		t.Fatal("enumeration included parent group outside child session")
	}
}
