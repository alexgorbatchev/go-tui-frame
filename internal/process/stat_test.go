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
