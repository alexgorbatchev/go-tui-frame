package frame

import (
	"bytes"
	"fmt"
	"image"
	"runtime"
	"strings"
	"testing"

	"github.com/alexgorbatchev/go-tui-frame/internal/emulator"
	"github.com/alexgorbatchev/go-tui-frame/internal/input"
	uv "github.com/charmbracelet/ultraviolet"
	ghostty "go.mitchellh.com/libghostty"
)

func newRouterTest(t *testing.T, handler func(Input) Disposition) (*inputRouter, *emulator.Terminal) {
	t.Helper()
	em, err := emulator.New(emulator.Options{Size: emulator.Size{Cols: 8, Rows: 5}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(em.Close)
	r, err := newInputRouter(em, handler)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.close)
	r.viewport = image.Rect(2, 3, 10, 8)
	s := routerState(t, em, "")
	r.host = matchingRouterProfile(s)
	return r, em
}

func matchingRouterProfile(s emulator.State) hostInputProfile {
	return hostInputProfile{
		KittyFlags: s.KittyKeyboardFlags, ModifyOtherKeys2: s.ModifyOtherKeys2, ModifyOtherKeysKnown: true,
		ApplicationCursor: s.Modes[ghostty.ModeDECCKM], ApplicationKeypad: s.Modes[ghostty.ModeKeypadKeys],
		Backarrow: s.Modes[ghostty.ModeBackarrowKeyMode], Numlock: s.Modes[ghostty.ModeNumlockKeypad],
		AltEscPrefix: s.Modes[ghostty.ModeAltEscPrefix], AltSendsEsc: s.Modes[ghostty.ModeAltSendsEsc],
	}
}

func routerState(t *testing.T, em *emulator.Terminal, controls string) emulator.State {
	t.Helper()
	if controls != "" {
		if _, err := em.Write([]byte(controls)); err != nil {
			t.Fatal(err)
		}
	}
	s, err := em.State()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func routerPackets(t *testing.T, raw string) []input.Packet {
	t.Helper()
	f := input.New()
	packets, err := f.Feed([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if packet, ok := f.Flush(); ok {
		packets = append(packets, packet)
	}
	return packets
}

func routeBytes(t *testing.T, r *inputRouter, s emulator.State, raw string) []byte {
	t.Helper()
	var result []byte
	for _, packet := range routerPackets(t, raw) {
		got, err := r.route(packet, s)
		if err != nil {
			t.Fatal(err)
		}
		result = append(result, got.Bytes...)
	}
	return result
}

func TestRoutingPreservesMatchingKeyboardAndUnknownBytes(t *testing.T) {
	r, em := newRouterTest(t, nil)
	s := routerState(t, em, "")
	r.host = matchingRouterProfile(s)
	for _, raw := range []string{"é\x1b界\x11", "\x1b[27;6;65~", "\x1b]9999;unknown\x1b\\", "\x1bOP"} {
		t.Run(raw, func(t *testing.T) {
			if got := routeBytes(t, r, s, raw); !bytes.Equal(got, []byte(raw)) {
				t.Fatalf("delivered %q, want exact %q", got, raw)
			}
		})
	}
}

func TestRoutingUsesNativeKeyboardEncodingForDifferentModes(t *testing.T) {
	for _, tt := range []struct{ name, modes, input, want string }{
		{"application cursor", "\x1b[?1h", "\x1b[A", "\x1bOA"},
		{"Kitty ctrl", "\x1b[>1u", "\x11", "\x1b[113;5u"},
		{"Kitty Unicode", "\x1b[>8u", "界", "\x1b[30028u"},
		{"modifyOtherKeys", "\x1b[>4;2m", "\x11", "\x1b[27;5;113~"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r, em := newRouterTest(t, nil)
			s := routerState(t, em, tt.modes)
			if got := routeBytes(t, r, s, tt.input); string(got) != tt.want {
				t.Fatalf("native delivery = %q, want %q", got, tt.want)
			}
		})
	}
}

// An outer terminal reports Option that means Alt as Alt with no text, or with
// the key's own or shifted character, so the child receives its Alt encoding.
// Host profiles mirror console.syncInput: Capture adds Kitty disambiguation,
// or modifyOtherKeys mode 2 when Kitty is unavailable.
// Ghostty applies option-as-alt only on macOS, so these cases go red only there.
func TestRoutingConvertsAltModifiedKeysToChildAltEncoding(t *testing.T) {
	const associatedHost = ghostty.KittyKeyDisambiguate | ghostty.KittyKeyReportAssociated
	for _, tt := range []struct {
		name, modes, input, want string
		hostKitty                ghostty.KittyKeyFlags
		hostModifyOtherKeys2     bool
	}{
		{name: "Kitty host to legacy child", input: "\x1b[98;3u", want: "\x1bb", hostKitty: ghostty.KittyKeyDisambiguate},
		{name: "modifyOtherKeys host to legacy child", input: "\x1b[27;3;98~", want: "\x1bb", hostModifyOtherKeys2: true},
		{
			name: "Kitty host to modifyOtherKeys child", modes: "\x1b[>4;2m", input: "\x1b[98;3u", want: "\x1b[27;3;98~",
			hostKitty: ghostty.KittyKeyDisambiguate, hostModifyOtherKeys2: true,
		},
		{name: "Kitty host to associated-text child", modes: "\x1b[>16u", input: "\x1b[98;3u", want: "\x1b[98;3u", hostKitty: associatedHost},
		{name: "own text to associated-text child", modes: "\x1b[>16u", input: "\x1b[98;3;98u", want: "\x1b[98;3u", hostKitty: associatedHost},
		{name: "reported shifted text to associated-text child", modes: "\x1b[>16u", input: "\x1b[98:66;4;66u", want: "\x1b[98;4u", hostKitty: associatedHost},
		{name: "upper-case shifted text to associated-text child", modes: "\x1b[>16u", input: "\x1b[98;4;66u", want: "\x1b[98;4u", hostKitty: associatedHost},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r, em := newRouterTest(t, nil)
			s := routerState(t, em, tt.modes)
			r.host.KittyFlags, r.host.ModifyOtherKeys2 = tt.hostKitty, tt.hostModifyOtherKeys2
			if got := routeBytes(t, r, s, tt.input); string(got) != tt.want {
				t.Fatalf("converted Alt+b = %q, want %q", got, tt.want)
			}
		})
	}
}

// With all keys and associated text reported, an outer terminal whose Option
// is not Alt still sets the Alt bit and attaches the text Option composed. A
// child requesting Kitty flags 24 gets host flags 25 under Capture. Every child
// protocol receives that text, as it did before Option was treated as Alt.
func TestRoutingKeepsOptionComposedText(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("the native encoder honors option-as-alt only on macOS")
	}
	// Option+L on a German layout composes "@"; Option+b on a US layout, "∫".
	const optionL, optionB = "\x1b[108;3;64u", "\x1b[98;3;8747u"
	for _, tt := range []struct{ name, modes, input, want string }{
		{"Kitty child, Option+L", "\x1b[>24u", optionL, optionL},
		{"Kitty child, Option+b", "\x1b[>24u", optionB, optionB},
		{"legacy child, Option+L", "", optionL, "@"},
		{"legacy child, Option+b", "", optionB, "∫"},
		{"modifyOtherKeys child, Option+L", "\x1b[>4;2m", optionL, "@"},
		{"modifyOtherKeys child, Option+b", "\x1b[>4;2m", optionB, "∫"},
		{"Kitty child, text starting with the key", "\x1b[>24u", "\x1b[98;3;98:769u", "\x1b[98;3;98:769u"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r, em := newRouterTest(t, nil)
			s := routerState(t, em, tt.modes)
			r.host.KittyFlags = ghostty.KittyKeyDisambiguate | ghostty.KittyKeyReportAll | ghostty.KittyKeyReportAssociated
			if got := routeBytes(t, r, s, tt.input); string(got) != tt.want {
				t.Fatalf("converted Option-composed text = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRoutingUnknownHostModifyOtherKeysUsesWireProvenance(t *testing.T) {
	r, em := newRouterTest(t, nil)
	r.host.ModifyOtherKeysKnown = false
	s := routerState(t, em, "")
	modified := "\x1b[27;5;113~"
	if got := routeBytes(t, r, s, modified); string(got) != "\x11" {
		t.Fatalf("unknown outer modified-key mode delivered %q to legacy child", got)
	}
	s = routerState(t, em, "\x1b[>4;2m")
	if got := routeBytes(t, r, s, modified); string(got) != modified {
		t.Fatalf("matching modified-key wire changed to %q", got)
	}
	s = routerState(t, em, "\x1b[>3u")
	r.host.KittyFlags = s.KittyKeyboardFlags
	kittypacket := "\x1b[113;17u"
	if got := routeBytes(t, r, s, kittypacket); string(got) != kittypacket {
		t.Fatalf("Kitty takes precedence over modified-key mode: %q", got)
	}
}

func TestRoutingCursorUsesObservedWireWhenHostModeIsUnavailable(t *testing.T) {
	for _, tt := range []struct{ name, raw, want string }{
		{"SS3 arrow", "\x1bOA", "\x1b[A"},
		{"C1 SS3 arrow", "\x8fA", "\x1b[A"},
		{"SS3 home", "\x1bOH", "\x1b[H"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r, em := newRouterTest(t, nil)
			s := routerState(t, em, "")
			if got := routeBytes(t, r, s, tt.raw); string(got) != tt.want {
				t.Fatalf("observed application-cursor input = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRoutingKeypadUsesObservedWireWhenHostModeIsUnavailable(t *testing.T) {
	for _, tt := range []struct{ name, modes, raw, want string }{
		{"SS3 digit", "", "\x1bOp", "0"},
		{"C1 SS3 digit", "", "\x8fp", "0"},
		{"SS3 enter", "", "\x1bOM", "\r"},
		{"matching application keypad", "\x1b[?66h\x1b[?1035l", "\x8fp", "\x8fp"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r, em := newRouterTest(t, nil)
			s := routerState(t, em, tt.modes)
			if got := routeBytes(t, r, s, tt.raw); string(got) != tt.want {
				t.Fatalf("observed application-keypad input = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRoutingCaptureOwnsConsumedReleaseAndLeavesPassedBytes(t *testing.T) {
	consume := true
	r, em := newRouterTest(t, func(in Input) Disposition {
		if consume && in.Key.Key().Code == 'q' {
			return Consume
		}
		return Pass
	})
	s := routerState(t, em, "\x1b[>3u")
	r.host.KittyFlags = s.KittyKeyboardFlags
	press := "\x1b[113;5u"
	if got := routeBytes(t, r, s, press); len(got) != 0 {
		t.Fatalf("captured press delivered %q", got)
	}
	consume = false
	for _, raw := range []string{"\x1b[113;1:2u", "\x1b[113;1:3u"} {
		if got := routeBytes(t, r, s, raw); len(got) != 0 {
			t.Fatalf("captured gesture delivered %q", got)
		}
	}
	if got := routeBytes(t, r, s, press); string(got) != press {
		t.Fatalf("next passed press = %q", got)
	}
}

func TestRoutingPasteChangesOnlyEnvelopeAndNeverCapturesPayload(t *testing.T) {
	for _, bracketed := range []bool{false, true} {
		t.Run(map[bool]string{false: "unbracketed", true: "bracketed"}[bracketed], func(t *testing.T) {
			calls := 0
			r, em := newRouterTest(t, func(Input) Disposition { calls++; return Consume })
			modes := ""
			if bracketed {
				modes = "\x1b[?2004h"
			}
			s := routerState(t, em, modes)
			payload := "\x11\n\x1b[31mraw\x00"
			raw := "\x1b[200~" + payload + "\x1b[201~"
			want := payload
			if bracketed {
				want = raw
			}
			if got := routeBytes(t, r, s, raw); string(got) != want || calls != 0 {
				t.Fatalf("paste = %q, capture calls = %d; want %q", got, calls, want)
			}
		})
	}
	r, em := newRouterTest(t, nil)
	s := routerState(t, em, "\x1b[?2004h")
	start := routeBytes(t, r, s, "\x1b[200~")
	s = routerState(t, em, "\x1b[?2004l")
	end := routeBytes(t, r, s, "\x1b[201~")
	if string(start)+string(end) != "\x1b[200~\x1b[201~" {
		t.Fatalf("mode change orphaned paste envelope: %q, %q", start, end)
	}
}

func TestRoutingLocalizesMouseWithNativeEncoders(t *testing.T) {
	for _, tt := range []struct{ name, modes, want string }{
		{"SGR", "\x1b[?1000;1006h", "\x1b[<4;4;5M"},
		{"X10", "\x1b[?1000h", "\x1b[M$$%"},
		{"UTF8", "\x1b[?1000;1005h", "\x1b[M$$%"},
		{"URxvt", "\x1b[?1000;1015h", "\x1b[36;4;5M"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r, em := newRouterTest(t, nil)
			s := routerState(t, em, tt.modes)
			if got := routeBytes(t, r, s, "\x1b[<4;6;8M"); string(got) != tt.want {
				t.Fatalf("localized delivery = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRoutingMouseGestureAndOutsideCoordinates(t *testing.T) {
	r, em := newRouterTest(t, nil)
	s := routerState(t, em, "\x1b[?1002;1006h")
	outside := routerPackets(t, "\x1b[<0;1;1M")[0]
	got, err := r.route(outside, s)
	if err != nil || len(got.Bytes) != 0 || got.Origin != "outside-child" {
		t.Fatalf("outside press = %#v, %v", got, err)
	}
	if got := routeBytes(t, r, s, "\x1b[<0;6;8m"); len(got) != 0 {
		t.Fatalf("release of frame-owned press delivered %q", got)
	}
	if got := routeBytes(t, r, s, "\x1b[<0;6;8M\x1b[<32;7;8M\x1b[<0;7;8m"); string(got) != "\x1b[<0;4;5M\x1b[<32;5;5M\x1b[<0;5;5m" {
		t.Fatalf("child gesture = %q", got)
	}
	got, err = r.route(routerPackets(t, "\x1b[<0;1;1m")[0], s)
	if err != nil || len(got.Bytes) != 0 || got.Origin != "outside-child" {
		t.Fatalf("outside release = %#v, %v", got, err)
	}
}

func TestRoutingFocusRequiresChildModeAndUnknownKeysAreExplicit(t *testing.T) {
	r, em := newRouterTest(t, nil)
	s := routerState(t, em, "")
	if got := routeBytes(t, r, s, "\x1b[I\x1b[O"); len(got) != 0 {
		t.Fatalf("disabled focus delivered %q", got)
	}
	s = routerState(t, em, "\x1b[?1004h")
	if got := routeBytes(t, r, s, "\x1b[I\x1b[O"); string(got) != "\x1b[I\x1b[O" {
		t.Fatalf("enabled focus = %q", got)
	}
	s = routerState(t, em, "\x1b[>1u")
	_, err := r.route(input.Packet{Raw: []byte("unsupported"), Event: uv.KeyPressEvent{Code: uv.KeyF63}}, s)
	if err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("unsupported native key = %v", err)
	}
}

func TestRoutingRejectsUnrepresentableMouseCoordinates(t *testing.T) {
	for _, tt := range []struct{ name, modes, input string }{
		{"X10", "\x1b[?1000h", "\x1b[<0;226;8M"},
		{"UTF8", "\x1b[?1000;1005h", "\x1b[<0;2018;8M"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r, em := newRouterTest(t, nil)
			if err := em.Resize(emulator.Size{Cols: 2040, Rows: 5}); err != nil {
				t.Fatal(err)
			}
			r.viewport = image.Rect(2, 3, 2042, 8)
			s := routerState(t, em, tt.modes)
			if _, err := r.route(routerPackets(t, tt.input)[0], s); err == nil {
				t.Fatal("silently lost or emitted out-of-protocol mouse coordinate")
			}
		})
	}
}

func TestRoutingRejectsNativeInvalidUTF8MouseButton(t *testing.T) {
	r, em := newRouterTest(t, nil)
	s := routerState(t, em, "\x1b[?1000;1005h")
	packet := routerPackets(t, "\x1b[<128;6;8M")[0]
	if _, err := r.route(packet, s); err == nil {
		t.Fatal("emitted a raw high-byte button code in UTF-8 mouse mode")
	}
}

func TestRoutingMeasuredPixelsPreserveWireOffsets(t *testing.T) {
	for _, tt := range []struct{ name, modes, want string }{
		{"pixels", "\x1b[?1000;1016h", "\x1b[<0;41;31M"},
		{"cells", "\x1b[?1000;1006h", "\x1b[<0;5;2M"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r, em := newRouterTest(t, nil)
			if err := em.Resize(emulator.Size{Cols: 8, Rows: 5, CellWidthPx: 10, CellHeightPx: 20}); err != nil {
				t.Fatal(err)
			}
			r.host.MousePixels, r.host.CellWidthPx, r.host.CellHeightPx = true, 10, 20
			s := routerState(t, em, tt.modes)
			if got := routeBytes(t, r, s, "\x1b[<0;61;91M"); string(got) != tt.want {
				t.Fatalf("pixel delivery = %q, want %q", got, tt.want)
			}
		})
	}
	r, em := newRouterTest(t, nil)
	s := routerState(t, em, "\x1b[?1000;1016h")
	if _, err := r.route(routerPackets(t, "\x1b[<0;6;8M")[0], s); err == nil {
		t.Fatal("fabricated pixel precision from a cell report")
	}
}

func TestRoutingDropsPixelReportsWithoutMeasuredCells(t *testing.T) {
	r, em := newRouterTest(t, nil)
	measure := func(width, height uint32) emulator.State {
		t.Helper()
		if err := em.Resize(emulator.Size{Cols: 8, Rows: 5, CellWidthPx: width, CellHeightPx: height}); err != nil {
			t.Fatal(err)
		}
		r.host.MousePixels, r.host.CellWidthPx, r.host.CellHeightPx = true, width, height
		return routerState(t, em, "")
	}
	routerState(t, em, "\x1b[?1002;1006h")
	s := measure(10, 20)
	if got := routeBytes(t, r, s, "\x1b[<0;61;91M"); string(got) != "\x1b[<0;5;2M" {
		t.Fatalf("measured press = %q", got)
	}
	// A pixel-less resize clears the cell size while the release is in flight.
	s = measure(0, 0)
	got, err := r.route(routerPackets(t, "\x1b[<0;61;91m")[0], s)
	if err != nil || len(got.Bytes) != 0 || got.Origin != "unmeasured-pixel-mouse" {
		t.Fatalf("unmeasured release = %#v, %v", got, err)
	}
	// The dropped release ends the child's ownership of the button, as an
	// outside release does, so a later drag is not delivered.
	s = measure(10, 20)
	got, err = r.route(routerPackets(t, "\x1b[<32;61;91M")[0], s)
	if err != nil || len(got.Bytes) != 0 || got.Origin != "unowned-mouse-gesture" {
		t.Fatalf("drag after unmeasured release = %#v, %v", got, err)
	}
	r.host.CellWidthPx = 12
	if _, err := r.route(routerPackets(t, "\x1b[<0;61;91M")[0], s); err == nil {
		t.Fatal("pixel report used a cell size the child emulator does not have")
	}
}

// The session admits outer input by routedLimit, so it must bound what the
// native encoders write for every keyboard protocol and mouse format a child
// can select, including SGR pixel reports at four-digit coordinates. The
// widest expansion must also reach the bound, keeping its documented worst
// case real.
func TestRoutedLimitBoundsNativeEncoding(t *testing.T) {
	var keys []string
	for b := range 256 {
		for _, prefix := range []string{"", "\x1b", "\x8f", "\x9b", "\x1bO", "\x1b["} {
			keys = append(keys, prefix+string([]byte{byte(b)}))
		}
	}
	for r := rune(0x80); r < 0x800; r++ {
		keys = append(keys, string(r), "\x1b"+string(r))
	}
	keys = append(keys, "\U00000800", "\U0000fffd", "\U00010000", "\U0010ffff", "\U0001f469\U0000200d\U0001f4bb", "\x1b[97;5u", "\x1b[122:90:122;2;90u", "\x1b[27;2;90~", "\x1b[200~paste\x1b[201~", "\x1b]9999;unknown\x1b\\")
	var cellMice, pixelMice []string
	for b := range 256 {
		// The cell viewport starts at column 2, row 3: these are inside it.
		cellMice = append(cellMice, "\x1b[M"+string([]byte{byte(b), '$', '%'}), fmt.Sprintf("\x1b[<%d;4;5M", b), fmt.Sprintf("\x1b[<%d;4;5m", b), fmt.Sprintf("\x1b[%d;4;5M", b+32))
		// The first and last pixels of the measured viewport's first and last cells.
		for _, at := range []string{"21;61", "5020;6060"} {
			pixelMice = append(pixelMice, fmt.Sprintf("\x1b[<%d;%sM", b, at), fmt.Sprintf("\x1b[<%d;%sm", b, at))
		}
	}
	keyModes := []string{"", "\x1b[?2004h", "\x1b[>4;2m", "\x1b[?1h\x1b[?66h\x1b[?67h\x1b[?1036h", "\x1b[?1035h\x1b[?66h"}
	for flags := 1; flags < 32; flags++ {
		keyModes = append(keyModes, fmt.Sprintf("\x1b[>%du", flags))
	}
	var mouseModes []string
	for _, format := range []string{"", "\x1b[?1005h", "\x1b[?1006h", "\x1b[?1015h", "\x1b[?1016h"} {
		mouseModes = append(mouseModes, "\x1b[?1003h"+format)
	}
	widest := 0.0
	for _, group := range []struct {
		modes, inputs []string
		measured      bool
	}{{keyModes, keys, false}, {mouseModes, cellMice, false}, {mouseModes, pixelMice, true}} {
		delivered := 0
		for _, modes := range group.modes {
			r, em := newRouterTest(t, nil)
			if group.measured {
				// A 500x300 grid of 10x20-pixel cells: pixel reports from the
				// outer terminal reach four-digit coordinates.
				if err := em.Resize(emulator.Size{Cols: 500, Rows: 300, CellWidthPx: 10, CellHeightPx: 20}); err != nil {
					t.Fatal(err)
				}
				r.viewport = image.Rect(2, 3, 502, 303)
				r.host.MousePixels, r.host.CellWidthPx, r.host.CellHeightPx = true, 10, 20
			}
			s := routerState(t, em, modes)
			for _, raw := range group.inputs {
				for _, p := range routerPackets(t, raw) {
					got, err := r.route(p, s)
					if err != nil {
						continue // Unconvertible input delivers nothing.
					}
					if len(got.Bytes) > routedLimit(p) {
						t.Fatalf("modes %q: %q routed to %d bytes %q, over its %d-byte limit", modes, p.Raw, len(got.Bytes), got.Bytes, routedLimit(p))
					}
					widest = max(widest, float64(len(got.Bytes))/float64(len(p.Raw)))
					delivered += len(got.Bytes)
				}
			}
		}
		if delivered == 0 {
			t.Fatalf("modes %q delivered nothing to check", group.modes)
		}
	}
	if widest != maxRoutedExpansion {
		t.Fatalf("widest routed expansion %.2f, want the %d-byte bound", widest, maxRoutedExpansion)
	}
}

func TestRoutingNativeTextSurvivesGCAndReleasesBorrow(t *testing.T) {
	r, em := newRouterTest(t, nil)
	text := strings.Repeat("界", 256)
	if err := r.setKey(uv.KeyPressEvent{Code: uv.KeyExtended, Text: text}); err != nil {
		t.Fatal(err)
	}
	runtime.GC()
	got, err := em.EncodeKey(r.key, ghostty.OptionAsAltTrue)
	if err != nil || string(got) != text {
		t.Fatalf("borrowed native text = %q, %v", got, err)
	}
	r.releaseKeyText()
	if retained := r.key.UTF8(); retained != "" {
		t.Fatalf("native event retained completed text: %q", retained)
	}
	r.close()
	r.close()
}
