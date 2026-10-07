package frame

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"testing"

	"github.com/alexgorbatchev/go-tui-frame/v2/internal/emulator"
	"github.com/alexgorbatchev/go-tui-frame/v2/internal/input"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
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
	return r, em
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
	for _, raw := range []string{"é\x1b界\x11", "\x1b[27;6;65~", "\x1b]9999;unknown\x1b\\", "\x1bOP"} {
		t.Run(raw, func(t *testing.T) {
			if got := routeBytes(t, r, s, raw); !bytes.Equal(got, []byte(raw)) {
				t.Fatalf("delivered %q, want exact %q", got, raw)
			}
		})
	}
}

// The outer terminal runs the child's keyboard modes, so a key it reports is
// already in the child's protocol and reaches the child as the terminal sent
// it. The modes can still differ: a key typed between a child mode change and
// the next synchronization arrives in the previous form, and a mode the
// terminal cannot switch keeps the terminal's form. Neither is rewritten,
// including a key no native key code describes.
func TestRoutingForwardsKeysAsSent(t *testing.T) {
	for _, tt := range []struct {
		name, modes, raw string
		// hostKitty is the Kitty flags the outer terminal still applies.
		hostKitty ghostty.KittyKeyFlags
	}{
		{name: "normal cursor key to application-cursor child", modes: "\x1b[?1h", raw: "\x1b[A"},
		{name: "SS3 cursor key to normal-cursor child", raw: "\x1bOA"},
		{name: "C1 SS3 cursor key to normal-cursor child", raw: "\x8fA"},
		{name: "SS3 keypad digit to numeric-keypad child", raw: "\x1bOp"},
		{name: "legacy Ctrl+Q to Kitty child", modes: "\x1b[>1u", raw: "\x11"},
		{name: "legacy text to Kitty child reporting all keys", modes: "\x1b[>8u", raw: "界"},
		{name: "legacy Ctrl+Q to modifyOtherKeys child", modes: "\x1b[>4;2m", raw: "\x11"},
		{name: "modifyOtherKeys Ctrl+Q to legacy child", raw: "\x1b[27;5;113~"},
		{name: "Kitty Ctrl+Q after the child left Kitty", raw: "\x1b[113;5u", hostKitty: ghostty.KittyKeyDisambiguate},
		{name: "Kitty Alt+Shift+comma to legacy child", raw: "\x1b[44:60;4u", hostKitty: ghostty.KittyKeyDisambiguate | ghostty.KittyKeyReportAlternates},
		{name: "Russian Ctrl+и to legacy child", raw: "\x1b[1080::98;5u", hostKitty: ghostty.KittyKeyDisambiguate | ghostty.KittyKeyReportAlternates},
		{name: "Option-composed text to modifyOtherKeys child", modes: "\x1b[>4;2m", raw: "\x1b[98;3;8747u", hostKitty: ghostty.KittyKeyDisambiguate | ghostty.KittyKeyReportAll | ghostty.KittyKeyReportAssociated},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r, em := newRouterTest(t, nil)
			s := routerState(t, em, tt.modes)
			r.host.KittyFlags = tt.hostKitty
			packets := routerPackets(t, tt.raw)
			if len(packets) != 1 {
				t.Fatalf("decoded %d packets from %q, want one key", len(packets), tt.raw)
			}
			if _, ok := packets[0].Event.(uv.KeyEvent); !ok {
				t.Fatalf("%q decoded as %T, want a key", tt.raw, packets[0].Event)
			}
			got, err := r.route(packets[0], s)
			if err != nil || string(got.Bytes) != tt.raw || got.Disposition != Pass || got.Origin != "user-input" {
				t.Fatalf("routed %q as %q (%v, %q), %v; want the exact bytes passed as user input", tt.raw, got.Bytes, got.Disposition, got.Origin, err)
			}
		})
	}
	r, em := newRouterTest(t, nil)
	s := routerState(t, em, "\x1b[>1u")
	got, err := r.route(input.Packet{Raw: []byte("unsupported"), Event: uv.KeyPressEvent{Code: uv.KeyF63}}, s)
	if err != nil || string(got.Bytes) != "unsupported" {
		t.Fatalf("key without a native key code routed as %q, %v; want its exact bytes", got.Bytes, err)
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

// A press the handler passes owns its repeats as it owns its release. A
// handler whose answer changes while the key is held still sees every repeat,
// but the child receives the whole gesture rather than a press and release
// with repeats missing between them.
func TestRoutingPassedPressKeepsRepeatsAndReleaseInChild(t *testing.T) {
	var handled []uv.KeyEvent
	r, em := newRouterTest(t, func(in Input) Disposition {
		handled = append(handled, in.Key)
		if len(handled) == 1 {
			return Pass
		}
		return Consume
	})
	s := routerState(t, em, "\x1b[>3u")
	r.host.KittyFlags = s.KittyKeyboardFlags
	gesture := []struct {
		raw    string
		repeat bool
	}{
		{raw: "\x1b[113;5u"},
		{raw: "\x1b[113;5:2u", repeat: true},
		{raw: "\x1b[113;5:2u", repeat: true},
		{raw: "\x1b[113;5:3u"},
	}
	for _, phase := range gesture {
		packets := routerPackets(t, phase.raw)
		if len(packets) != 1 {
			t.Fatalf("decoded %d packets from %q, want one key", len(packets), phase.raw)
		}
		if press, ok := packets[0].Event.(uv.KeyPressEvent); ok && press.IsRepeat != phase.repeat {
			t.Fatalf("%q decoded with IsRepeat %v, want %v", phase.raw, press.IsRepeat, phase.repeat)
		}
		got, err := r.route(packets[0], s)
		if err != nil || got.Disposition != Pass || string(got.Bytes) != phase.raw {
			t.Fatalf("passed gesture phase %q routed as %q (%v), %v; want its exact bytes passed", phase.raw, got.Bytes, got.Disposition, err)
		}
	}
	if len(handled) != len(gesture) {
		t.Fatalf("handler observed %d gesture phases, want %d", len(handled), len(gesture))
	}
	press := gesture[0].raw
	if got := routeBytes(t, r, s, press); len(got) != 0 {
		t.Fatalf("press after the release delivered %q; the handler consumes it", got)
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

// The child's encoder reports in its single active tracking mode, so the
// router delivers mouse events only while that mode is set, whatever mode bits
// remain set after a reset.
func TestRoutingFollowsActiveMouseTrackingMode(t *testing.T) {
	for _, tt := range []struct {
		name, controls, want, origin string
	}{
		{"normal", "\x1b[?1000h", "\x1b[M #\"", "mouse-protocol"},
		{"reset after upgrade", "\x1b[?1000h\x1b[?1002h\x1b[?1002l", "", "mouse-disabled"},
		{"set and reset", "\x1b[?1000h\x1b[?1000l", "", "mouse-disabled"},
		{"later mode replaces earlier", "\x1b[?1003h\x1b[?1000h", "\x1b[M #\"", "mouse-protocol"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r, em := newRouterTest(t, nil)
			s := routerState(t, em, tt.controls)
			got, err := r.route(routerPackets(t, "\x1b[<0;5;5M")[0], s)
			if err != nil || string(got.Bytes) != tt.want || got.Origin != tt.origin {
				t.Fatalf("press routed %q as %q, %v; want %q as %q", got.Bytes, got.Origin, err, tt.want, tt.origin)
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

// Without SGR or SGR-pixel reports (modes 1006 and 1016) the outer terminal
// reports every release as X10 button code 3, which names no button, as does
// an SGR release with button code 3. That release ends every gesture: the
// child receives a release of each button it owns, in its own encoding, and
// nothing for a frame-owned press or a release outside the child. A later drag
// then belongs to no child gesture.
func TestRoutingButtonlessReleaseEndsEveryGesture(t *testing.T) {
	// Native button-event tracking (1002) reports no press without a button;
	// normal tracking (1000) does.
	const sgr, legacy, normalSGR = "\x1b[?1002;1006h", "\x1b[?1002h", "\x1b[?1000;1006h"
	// X10 reports: left, middle and right presses inside the child at column
	// 6, row 8, a right press outside it, and the anonymous release.
	const left, middle, outsideRight, release = "\x1b[M &(", "\x1b[M!&(", "\x1b[M\"!!", "\x1b[M#&("
	for _, tt := range []struct{ name, modes, gesture, want string }{
		{"SGR child", sgr, left + release, "\x1b[<0;4;5M\x1b[<0;4;5m"},
		{"legacy child", legacy, left + release, "\x1b[M $%\x1b[M#$%"},
		{"release modifiers", sgr, left + "\x1b[M'&(", "\x1b[<0;4;5M\x1b[<4;4;5m"},
		{"every child button", sgr, middle + left + release, "\x1b[<1;4;5M\x1b[<0;4;5M\x1b[<0;4;5m\x1b[<1;4;5m"},
		{"frame-owned press", sgr, outsideRight + release, ""},
		{"child and frame presses", sgr, outsideRight + left + release, "\x1b[<0;4;5M\x1b[<0;4;5m"},
		{"release outside the child", sgr, left + "\x1b[M#!!", "\x1b[<0;4;5M"},
		{"SGR release with button code 3", sgr, "\x1b[<0;6;8M\x1b[<3;6;8m", "\x1b[<0;4;5M\x1b[<0;4;5m"},
		{"SGR press and release with button code 3", normalSGR, "\x1b[<3;6;8M\x1b[<3;6;8m", "\x1b[<3;4;5M\x1b[<3;4;5m"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r, em := newRouterTest(t, nil)
			s := routerState(t, em, tt.modes)
			if got := routeBytes(t, r, s, tt.gesture); string(got) != tt.want {
				t.Errorf("gesture delivered %q, want %q", got, tt.want)
			}
			// X10 drags of the left, middle and right buttons.
			for _, drag := range []string{"\x1b[M@&(", "\x1b[MA&(", "\x1b[MB&("} {
				got, err := r.route(routerPackets(t, drag)[0], s)
				if err != nil || len(got.Bytes) != 0 || got.Origin != "unowned-mouse-gesture" {
					t.Errorf("drag %q after the release delivered %q as %q, %v", drag, got.Bytes, got.Origin, err)
				}
			}
		})
	}
}

func TestRoutingFocusRequiresChildMode(t *testing.T) {
	r, em := newRouterTest(t, nil)
	s := routerState(t, em, "")
	if got := routeBytes(t, r, s, "\x1b[I\x1b[O"); len(got) != 0 {
		t.Fatalf("disabled focus delivered %q", got)
	}
	s = routerState(t, em, "\x1b[?1004h")
	if got := routeBytes(t, r, s, "\x1b[I\x1b[O"); string(got) != "\x1b[I\x1b[O" {
		t.Fatalf("enabled focus = %q", got)
	}
}

// Run closes the router when the session ends, and a test may close it again.
func TestRoutingCloseReleasesNativeEventOnce(t *testing.T) {
	r, _ := newRouterTest(t, nil)
	r.close()
	r.close()
	if r.mouse != nil {
		t.Fatal("closed router kept its native mouse event")
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
			if _, err := r.route(routerPackets(t, tt.input)[0], s); !errors.Is(err, errUnroutable) {
				t.Fatalf("out-of-protocol mouse coordinate routed with %v, want an unroutable event", err)
			}
		})
	}
}

// A click the child's X10 encoding cannot place is withheld from the child and
// reported to observers; the session keeps routing the input that follows.
func TestSessionWithholdsUnroutableMouseEvent(t *testing.T) {
	s, _, slave := newRepaintSessionSize(t, Size{Cols: 240, Rows: 6}, ansi.ModeReset, nil)
	router, err := newInputRouter(s.terminal, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(router.close)
	s.router = router
	var routed []Event
	s.events = newEventDispatcher(eventBit(Routed), sessionCancel(t), func(ev Event) { routed = append(routed, ev) })
	t.Cleanup(s.events.close)
	readRepaintChunk(t, s, slave, "\x1b[?1000h")
	const click = "\x1b[<0;230;3M"
	for _, raw := range []string{click, "a"} {
		if err := s.route(routerPackets(t, raw)[0]); err != nil {
			t.Fatalf("routing %q ended the session: %v", raw, err)
		}
	}
	var queued []byte
	for _, p := range s.queue {
		queued = append(queued, p.bytes[p.offset:]...)
	}
	if string(queued) != "a" {
		t.Fatalf("child input queue holds %q, want only the key", queued)
	}
	s.events.close()
	if len(routed) != 1 {
		t.Fatalf("observed %d routed events, want 1: %#v", len(routed), routed)
	}
	ev := routed[0]
	if string(ev.Bytes) != click || ev.Origin != "unroutable-input" || ev.Disposition != Pass || !errors.Is(ev.Error, errUnroutable) {
		t.Fatalf("withheld click observed as %q from %q (%v), error %v", ev.Bytes, ev.Origin, ev.Disposition, ev.Error)
	}
}

func TestRoutingRejectsNativeInvalidUTF8MouseButton(t *testing.T) {
	r, em := newRouterTest(t, nil)
	s := routerState(t, em, "\x1b[?1000;1005h")
	packet := routerPackets(t, "\x1b[<128;6;8M")[0]
	if _, err := r.route(packet, s); !errors.Is(err, errUnroutable) {
		t.Fatalf("high-byte button code in UTF-8 mouse mode routed with %v, want an unroutable event", err)
	}
}

// The pinned decoder reads SGR button codes 130 and 131 as buttons 10 and 11,
// which the native encoder does not encode. The press stays with the frame,
// so its release and drags are not delivered either.
func TestRoutingRejectsButtonsTenAndEleven(t *testing.T) {
	for _, tt := range []struct{ name, press, drag, release string }{
		{"button 10", "\x1b[<130;6;8M", "\x1b[<162;7;8M", "\x1b[<130;7;8m"},
		{"button 11", "\x1b[<131;6;8M", "\x1b[<163;7;8M", "\x1b[<131;7;8m"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r, em := newRouterTest(t, nil)
			s := routerState(t, em, "\x1b[?1002;1006h")
			_, err := r.route(routerPackets(t, tt.press)[0], s)
			if !errors.Is(err, errUnroutable) || !errors.Is(err, emulator.ErrMouseButton) {
				t.Fatalf("press routed with %v, want an unroutable native button", err)
			}
			for _, raw := range []string{tt.drag, tt.release} {
				got, err := r.route(routerPackets(t, raw)[0], s)
				if err != nil || len(got.Bytes) != 0 || got.Origin != "unowned-mouse-gesture" {
					t.Fatalf("%q after the withheld press = %#v, %v", raw, got, err)
				}
			}
		})
	}
}

// otherMouseEvent is a uv.MouseEvent of a kind the native encoder has no
// action for.
type otherMouseEvent uv.Mouse

func (e otherMouseEvent) String() string  { return uv.Mouse(e).String() }
func (e otherMouseEvent) Mouse() uv.Mouse { return uv.Mouse(e) }

// Events the decoder never produces from the outer terminal's reports still
// fail as single unroutable events: modifier bits and buttons outside the
// native mouse model, and an event kind with no native action.
func TestRoutingRejectsMouseEventsOutsideNativeModel(t *testing.T) {
	at := uv.Mouse{X: 5, Y: 7, Button: uv.MouseLeft}
	with := func(change func(*uv.Mouse)) uv.Mouse {
		m := at
		change(&m)
		return m
	}
	for _, tt := range []struct {
		name  string
		event uv.MouseEvent
	}{
		{"meta", uv.MouseClickEvent(with(func(m *uv.Mouse) { m.Mod = uv.ModMeta }))},
		{"hyper", uv.MouseClickEvent(with(func(m *uv.Mouse) { m.Mod = uv.ModCtrl | uv.ModHyper }))},
		{"scroll lock", uv.MouseClickEvent(with(func(m *uv.Mouse) { m.Mod = uv.ModScrollLock }))},
		{"button", uv.MouseClickEvent(with(func(m *uv.Mouse) { m.Button = uv.MouseButton11 + 1 }))},
		{"event kind", otherMouseEvent(at)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r, em := newRouterTest(t, nil)
			s := routerState(t, em, "\x1b[?1003;1006h")
			packet := input.Packet{Raw: []byte("\x1b[<0;6;8M"), Event: tt.event}
			if _, err := r.route(packet, s); !errors.Is(err, errUnroutable) {
				t.Fatalf("%v routed with %v, want an unroutable event", tt.event, err)
			}
		})
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
	if _, err := r.route(routerPackets(t, "\x1b[<0;6;8M")[0], s); !errors.Is(err, errUnroutable) {
		t.Fatalf("cell report for a pixel child routed with %v, want an unroutable event", err)
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
	// The session resizes the child to every measured cell size before it
	// routes another report, so a mismatch is broken session state, not an
	// event the child's protocol cannot represent.
	r.host.CellWidthPx = 12
	if _, err := r.route(routerPackets(t, "\x1b[<0;61;91M")[0], s); err == nil || errors.Is(err, errUnroutable) {
		t.Fatalf("pixel report with a cell size the child emulator does not have routed with %v, want a session error", err)
	}
}

// The session admits outer input by routedLimit, so it must bound what routing
// delivers for every keyboard protocol and mouse format a child can select.
// Keys reach the child as sent; the native mouse encoders write the rest,
// including SGR pixel reports at four-digit coordinates and an X10 release
// that releases every button the child owns. The widest expansion must also
// reach the bound, keeping its documented worst case real.
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
	// Every button a press can name, pressed with every modifier at the widest
	// X10 coordinates, then the X10 release that names no button and so
	// releases each one the child owns. Buttons 10 and 11 keep the bound
	// checked if the native encoder starts accepting them. An SGR press with
	// no button, Cb 3, adds no release beside the named ones.
	var releasedMice []string
	for _, cb := range []byte{0, 1, 2, 128, 129, 130, 131} {
		releasedMice = append(releasedMice, "\x1b[M"+string([]byte{32 + 28 + cb, 0xff, 0xff}))
	}
	releasedMice = append(releasedMice, "\x1b[<31;223;223M", "\x1b[M?\xff\xff")
	keyModes := []string{"", "\x1b[?2004h", "\x1b[>4;2m", "\x1b[?1h\x1b[?66h\x1b[?67h\x1b[?1036h", "\x1b[?1035h\x1b[?66h"}
	for flags := 1; flags < 32; flags++ {
		keyModes = append(keyModes, fmt.Sprintf("\x1b[>%du", flags))
	}
	var mouseModes []string
	for _, format := range []string{"", "\x1b[?1005h", "\x1b[?1006h", "\x1b[?1015h", "\x1b[?1016h"} {
		mouseModes = append(mouseModes, "\x1b[?1003h"+format)
	}
	widest := 0.0
	// A 500x300 grid holds the widest X10 coordinates. With 10x20-pixel cells,
	// pixel reports from the outer terminal reach four-digit coordinates.
	wide, measured := emulator.Size{Cols: 500, Rows: 300}, emulator.Size{Cols: 500, Rows: 300, CellWidthPx: 10, CellHeightPx: 20}
	for _, group := range []struct {
		modes, inputs []string
		grid          emulator.Size // The zero size keeps the 8x5 test grid.
	}{{keyModes, keys, emulator.Size{}}, {mouseModes, cellMice, emulator.Size{}}, {mouseModes, releasedMice, wide}, {mouseModes, pixelMice, measured}} {
		delivered := 0
		for _, modes := range group.modes {
			r, em := newRouterTest(t, nil)
			if group.grid != (emulator.Size{}) {
				if err := em.Resize(group.grid); err != nil {
					t.Fatal(err)
				}
				r.viewport = image.Rect(2, 3, 2+group.grid.Cols, 3+group.grid.Rows)
				r.host.MousePixels, r.host.CellWidthPx, r.host.CellHeightPx = group.grid.CellWidthPx > 0, group.grid.CellWidthPx, group.grid.CellHeightPx
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
