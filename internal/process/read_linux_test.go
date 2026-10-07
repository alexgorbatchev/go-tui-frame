package process

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func TestReadStringsParsesProcfsVectors(t *testing.T) {
	tests := []struct {
		name      string
		raw       string
		available bool
		want      []string
	}{
		{"empty file", "", true, []string{}},
		{"unterminated vector", "a\x00b", false, nil},
		{"empty elements", "a\x00\x00b\x00", true, []string{"a", "", "b"}},
		{"single empty element", "\x00", true, []string{""}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeProcFixture(t, tt.raw)
			var r procReader
			got := r.readStrings(path, "fixture")
			if got.Available != tt.available {
				t.Fatalf("available = %v, want %v (error %v)", got.Available, tt.available, got.Error)
			}
			if tt.available && (got.Value == nil || !slices.Equal(got.Value, tt.want)) {
				t.Fatalf("value = %#v, want %#v", got.Value, tt.want)
			}
		})
	}
}

func TestReadStringsAllocationsDoNotGrowWithEntries(t *testing.T) {
	small := readStringsAllocs(t, 2)
	large := readStringsAllocs(t, 200)
	if large > small {
		t.Fatalf("readStrings allocations grew from %v for 2 entries to %v for 200 entries", small, large)
	}
	t.Logf("readStrings allocations: %v for 2 entries, %v for 200 entries", small, large)
}

func TestProcReaderAllocationsDoNotGrowWithFileSize(t *testing.T) {
	small := procReadAllocs(t, 64)
	large := procReadAllocs(t, 256*1024)
	if large > small {
		t.Fatalf("procReader.read allocations grew from %v for 64 bytes to %v for 256 KiB", small, large)
	}
	t.Logf("procReader.read allocations: %v for 64 bytes, %v for 256 KiB", small, large)
}

func TestReadOwnProcessMatchesRuntimeVectors(t *testing.T) {
	s := Read(os.Getpid())
	if !s.Args.Available || !slices.Equal(s.Args.Value, os.Args) {
		t.Fatalf("args = %q (available %v, error %v), want %q", s.Args.Value, s.Args.Available, s.Args.Error, os.Args)
	}
	if !s.Environment.Available || !slices.Equal(s.Environment.Value, os.Environ()) {
		t.Fatalf("environment has %d entries (available %v, error %v), want %d matching os.Environ", len(s.Environment.Value), s.Environment.Available, s.Environment.Error, len(os.Environ()))
	}
}

func readStringsAllocs(t *testing.T, entries int) float64 {
	t.Helper()
	var raw strings.Builder
	for i := range entries {
		raw.WriteString("VARIABLE_" + strconv.Itoa(i) + "=value\x00")
	}
	path := writeProcFixture(t, raw.String())
	var r procReader
	return testing.AllocsPerRun(100, func() {
		if got := r.readStrings(path, "fixture"); len(got.Value) != entries {
			t.Fatalf("read %d entries (error %v), want %d", len(got.Value), got.Error, entries)
		}
	})
}

func procReadAllocs(t *testing.T, size int) float64 {
	t.Helper()
	path := writeProcFixture(t, strings.Repeat("x", size))
	var r procReader
	return testing.AllocsPerRun(100, func() {
		if data, err := r.read(path); err != nil || len(data) != size {
			t.Fatalf("read %d bytes (error %v), want %d", len(data), err, size)
		}
	})
}

func writeProcFixture(t *testing.T, raw string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "vector")
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
