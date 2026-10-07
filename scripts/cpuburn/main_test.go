package main

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestCPUBurn(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, os.Args[0], "-cpus=2", "-timeout=100ms")
	cmd.Env = append(os.Environ(), "RUN_CPU_BURN=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("cpuburn failed: %v, output: %s", err, string(out))
	}
}

func init() {
	if os.Getenv("RUN_CPU_BURN") == "1" {
		main()
		os.Exit(0)
	}
}
