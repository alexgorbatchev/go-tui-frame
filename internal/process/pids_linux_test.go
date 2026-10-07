package process

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
)

// procFixture builds a directory of subdirectories with the given names. In
// procfs the per-process entries are directories too; listPIDs selects entries
// by name alone, as numeric names are the only ones accepted.
func procFixture(t *testing.T, names ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range names {
		if err := os.Mkdir(filepath.Join(dir, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestListPIDsAcceptsOnlyPositiveDecimalNames(t *testing.T) {
	dir := procFixture(t,
		"1", "42", "4194304", "2147483647",
		"0", "self", "thread-self", "12a", "+5", "-3", " 7", "007x", "2147483648",
		"99999999999999999999", "sys", "cpuinfo",
	)
	got, err := listPIDs(dir)
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(got)
	want := []int{1, 42, 4194304, 2147483647}
	if !slices.Equal(got, want) {
		t.Fatalf("listPIDs = %v, want %v", got, want)
	}
}

func TestListPIDsReportsMissingDirectory(t *testing.T) {
	_, err := listPIDs(filepath.Join(t.TempDir(), "missing"))
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("listPIDs of a missing directory error = %v, want fs.ErrNotExist", err)
	}
}

// Non-numeric procfs entries such as "self", "sys" and "cpuinfo" must cost
// nothing beyond reading their directory records.
func TestListPIDsAllocationsDoNotGrowWithNonNumericEntries(t *testing.T) {
	small := listPIDsAllocs(t, procFixture(t, append([]string{"1", "2", "3"}, nonNumericNames(2)...)...))
	large := listPIDsAllocs(t, procFixture(t, append([]string{"1", "2", "3"}, nonNumericNames(500)...)...))
	if large != small {
		t.Fatalf("listPIDs allocations grew from %v with 2 non-numeric entries to %v with 500", small, large)
	}
	t.Logf("listPIDs allocations: %v with 2 or 500 non-numeric entries", small)
}

func nonNumericNames(n int) []string {
	names := make([]string, n)
	for i := range names {
		names[i] = "entry-" + strconv.Itoa(i)
	}
	return names
}

func listPIDsAllocs(t *testing.T, dir string) float64 {
	t.Helper()
	return testing.AllocsPerRun(20, func() {
		if _, err := listPIDs(dir); err != nil {
			t.Fatal(err)
		}
	})
}
