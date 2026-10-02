package process

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestReadCurrentNativeProcess(t *testing.T) {
	leader, member := processFamily(t)
	s := Read(member)
	if !s.SessionID.Available || s.SessionID.Value != leader {
		t.Fatalf("session: %+v", s.SessionID)
	}
	if !s.ProcessGroup.Available || s.ProcessGroup.Value != member {
		t.Fatalf("group: %+v", s.ProcessGroup)
	}
	if !s.ParentPID.Available || s.ParentPID.Value != leader {
		t.Fatalf("parent: %+v", s.ParentPID)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if !s.Directory.Available || s.Directory.Value != cwd {
		t.Fatalf("cwd: %+v, want %s", s.Directory, cwd)
	}
	if !s.Executable.Available || !filepath.IsAbs(s.Executable.Value) {
		t.Fatalf("executable: %+v", s.Executable)
	}
	if !s.Args.Available || !slices.Contains(s.Args.Value, "-test.run=^TestProcessChild$") {
		t.Fatalf("arguments: %+v", s.Args)
	}
	if !s.Environment.Available || !slices.Contains(s.Environment.Value, "FRAME_PROCESS_ROLE=member") {
		t.Fatalf("environment availability=%v, error=%v; missing test member marker", s.Environment.Available, s.Environment.Error)
	}
	if !s.Resources.ResidentBytes.Available || s.Resources.ResidentBytes.Value == 0 {
		t.Fatalf("resident bytes: %+v", s.Resources.ResidentBytes)
	}
	if !s.Resources.Threads.Available || s.Resources.Threads.Value <= 0 {
		t.Fatalf("threads: %+v", s.Resources.Threads)
	}
	if !s.Resources.CPUUser.Available || !s.Resources.CPUSystem.Available {
		t.Fatalf("CPU: %+v", s.Resources)
	}
	if s.StartIdentity.Value == "" || !s.StartIdentity.Available {
		t.Fatalf("identity: %+v", s.StartIdentity)
	}
	members, err := List(leader)
	if err != nil {
		t.Fatal(err)
	}
	pids := make([]int, len(members))
	for i := range members {
		pids[i] = members[i].PID
	}
	if !slices.Contains(pids, leader) || !slices.Contains(pids, member) {
		t.Fatalf("session members: %v", pids)
	}
}

func TestReadMissingProcessPreservesErrors(t *testing.T) {
	s := Read(-1)
	if s.SessionID.Available || s.SessionID.Error == nil {
		t.Fatalf("missing session: %+v", s.SessionID)
	}
	if s.Directory.Available || s.Directory.Error == nil {
		t.Fatalf("missing cwd: %+v", s.Directory)
	}
	if s.Args.Available || s.Args.Error == nil {
		t.Fatalf("missing args: %+v", s.Args)
	}
}

func TestReadDistinguishesCurrentDirectoryAndOwnsCopies(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(filepath.Join(cwd, "..", "..", ".tmp"), "process-cwd-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Error(err)
		}
	})
	_, member := processFamilyDir(t, dir)
	s := Read(member)
	if !s.Directory.Available || s.Directory.Value != dir || s.Directory.Value == cwd {
		t.Fatalf("runtime cwd: %+v, launch cwd %s", s.Directory, cwd)
	}
	copy := Clone(s)
	copy.Args.Value[0] = "changed"
	copy.Environment.Value[0] = "changed"
	copy.Resources.Native.Value["new"] = 1
	if s.Args.Value[0] == "changed" || s.Environment.Value[0] == "changed" || s.Resources.Native.Value["new"] != 0 {
		t.Fatal("clone aliases retained observation storage")
	}
}
