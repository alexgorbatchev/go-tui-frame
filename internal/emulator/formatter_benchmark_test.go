package emulator

import (
	"fmt"
	"io"
	"strings"
	"testing"

	ghostty "go.mitchellh.com/libghostty"
)

// This measures native screen export, including the child write but excluding
// viewport placement, clearing, composition and outer-terminal delivery.
func BenchmarkGhosttyVTFormatter(b *testing.B) {
	for _, tt := range []struct {
		name string
		size Size
		line string
	}{
		{"sparse20x6", Size{Cols: 20, Rows: 6}, ""},
		{"plain120x40", Size{Cols: 120, Rows: 40}, strings.Repeat("x", 119)},
		{"styled120x40", Size{Cols: 120, Rows: 40}, "\x1b[1;38;5;33mstatus\x1b[0m 日本語 e\u0301 \x1b]8;;https://example.test/report\x1b\\report\x1b]8;;\x1b\\"},
	} {
		b.Run(tt.name, func(b *testing.B) {
			for _, method := range []string{"buffer", "writer"} {
				b.Run(method, func(b *testing.B) {
					em, err := New(Options{Size: tt.size})
					if err != nil {
						b.Fatal(err)
					}
					defer em.Close()
					var initial strings.Builder
					if tt.line != "" {
						for y := range tt.size.Rows {
							fmt.Fprintf(&initial, "\x1b[%d;1H%s", y+1, tt.line)
						}
					}
					if _, err := em.Write([]byte(initial.String())); err != nil {
						b.Fatal(err)
					}
					formatter, err := ghostty.NewFormatter(em.native, ghostty.WithFormatterFormat(ghostty.FormatterFormatVT), ghostty.WithFormatterTrim(false))
					if err != nil {
						b.Fatal(err)
					}
					defer formatter.Close()
					buf := make([]byte, 128<<10)
					format := func() (int64, error) {
						if method == "writer" {
							return formatter.WriteTo(io.Discard)
						}
						n, err := formatter.FormatBuf(buf)
						return int64(n), err
					}
					text := []byte("\x1b[Hupdated")
					if _, err := em.Write(text); err != nil {
						b.Fatal(err)
					}
					written, err := format()
					if err != nil {
						b.Fatal(err)
					}
					b.ReportAllocs()
					b.ResetTimer()
					for i := range b.N {
						text[3] = byte('a' + i%2)
						if _, err := em.Write(text); err != nil {
							b.Fatal(err)
						}
						written, err = format()
						if err != nil {
							b.Fatal(err)
						}
					}
					b.StopTimer()
					b.ReportMetric(float64(written), "output-B/op")
				})
			}
		})
	}
}
