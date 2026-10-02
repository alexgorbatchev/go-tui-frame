package main

import (
	"debug/elf"
	"debug/macho"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Inspect the executable itself: a static Ghostty archive alone does not prove
// that cgo has avoided a runtime libc or another shared-library dependency.
func TestStandaloneBinary(t *testing.T) {
	artifact := os.Getenv("TUI_FRAME_ARTIFACT")
	if artifact == "" {
		artifact = buildExample(t)
	} else if !filepath.IsAbs(artifact) {
		artifact = filepath.Join("../..", artifact)
	}
	if f, err := macho.Open(artifact); err == nil {
		defer func() {
			if err := f.Close(); err != nil {
				t.Errorf("close Mach-O: %v", err)
			}
		}()
		if f.Type != macho.TypeExec {
			t.Fatalf("Mach-O type %s is not an executable", f.Type)
		}
		libs, err := f.ImportedLibraries()
		if err != nil {
			t.Fatal(err)
		}
		for _, lib := range libs {
			if !strings.HasPrefix(lib, "/usr/lib/") && !strings.HasPrefix(lib, "/System/Library/") {
				t.Errorf("Mach-O imports a non-system library: %s", lib)
			}
		}
		t.Logf("Mach-O imports only OS-provided libraries: %v", libs)
		return
	}
	f, err := elf.Open(artifact)
	if err != nil {
		t.Fatalf("artifact is neither Mach-O nor ELF: %v", err)
	}
	defer func() {
		if err := f.Close(); err != nil {
			t.Errorf("close ELF: %v", err)
		}
	}()
	if f.Type != elf.ET_EXEC && f.Type != elf.ET_DYN {
		t.Fatalf("ELF type %s is not an executable", f.Type)
	}
	libs, err := f.ImportedLibraries()
	if err != nil {
		t.Fatal(err)
	}
	if len(libs) != 0 {
		t.Errorf("ELF imports shared libraries: %v", libs)
	}
	for _, program := range f.Progs {
		if program.Type == elf.PT_INTERP {
			t.Error("ELF requires a dynamic loader")
		}
	}
	t.Logf("ELF imported libraries: %v", libs)
}
