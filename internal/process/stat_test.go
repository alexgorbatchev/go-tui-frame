package process

import "testing"

func TestParseProcStatPreservesNativeFields(t *testing.T) {
	const suffix = " R 12 34 56 0 -1 0 0 0 0 0 123 456 0 0 20 0 7 0 999 4096 8"
	for _, name := range []string{"ordinary", "name with spaces", "closing)paren\nname"} {
		t.Run(name, func(t *testing.T) {
			s, err := parseStat("100 (" + name + ")" + suffix)
			if err != nil {
				t.Fatal(err)
			}
			if s.name != name || s.parent != 12 || s.group != 34 || s.session != 56 || s.foreground != -1 || s.user != 123 || s.system != 456 || s.threads != 7 || s.start != 999 || s.virtual != 4096 || s.resident != 8 {
				t.Fatalf("parsed: %+v", s)
			}
		})
	}
	for _, raw := range []string{"", "100 (x) R 0", "100 (x)" + suffix + " )"} {
		if _, err := parseStat(raw); err == nil {
			t.Fatalf("malformed stat accepted: %q", raw)
		}
	}
}

// The identity re-check reads only the start field, so it must agree with
// parseStat without converting or splitting the whole record.
func TestParseStatStartMatchesParseStat(t *testing.T) {
	const prefix = " R 12 34 56 0 -1 0 0 0 0 0 123 456 0 0 20 0 7 0"
	tests := []struct {
		name string
		raw  string
		want uint64
		ok   bool
	}{
		{"ordinary", "100 (sh)" + prefix + " 999 4096 8\n", 999, true},
		{"comm with spaces and parens", "100 (a ) b)" + prefix + " 18446744073709551615 4096 8", 1<<64 - 1, true},
		{"start is last field", "100 (sh)" + prefix + " 999", 999, true},
		{"truncated before start", "100 (sh)" + prefix, 0, false},
		{"non-numeric start", "100 (sh)" + prefix + " x9 4096 8", 0, false},
		{"missing comm", "100 sh" + prefix + " 999", 0, false},
		{"empty", "", 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseStatStart([]byte(tt.raw))
			if (err == nil) != tt.ok || got != tt.want {
				t.Fatalf("parseStatStart = %d, %v; want %d, ok %v", got, err, tt.want, tt.ok)
			}
			if !tt.ok {
				return
			}
			if full, err := parseStat(tt.raw + " 0 0 0"); err != nil || full.start != got {
				t.Fatalf("parseStat start = %d, %v; parseStatStart %d", full.start, err, got)
			}
		})
	}
	raw := []byte("100 (sh)" + prefix + " 999 4096 8\n")
	if allocs := testing.AllocsPerRun(100, func() {
		if _, err := parseStatStart(raw); err != nil {
			t.Fatal(err)
		}
	}); allocs != 0 {
		t.Fatalf("parseStatStart allocates %v times per record", allocs)
	}
}
