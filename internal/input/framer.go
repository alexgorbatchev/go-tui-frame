// Package input frames original terminal bytes before native event decoding.
package input

import (
	"bytes"
	"errors"
	"slices"
	"unicode/utf8"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
)

const maxSequenceBytes = 1 << 20

var (
	pasteStart          = []byte("\x1b[200~")
	pasteEnd            = []byte("\x1b[201~")
	x10Prefix           = []byte("\x1b[M")
	ErrSequenceTooLarge = errors.New("terminal input sequence exceeds framing limit")
)

// Packet retains exact source bytes alongside the native semantic event.
// Paste payload packets have Paste set and deliberately have no key event.
type Packet struct {
	Raw   []byte
	Event uv.Event
	Paste bool
}

// Framer is owned by one input-routing goroutine. Read boundaries never define
// escape-sequence boundaries. The caller controls ambiguous-key deadlines.
type Framer struct {
	buf     []byte
	paste   bool
	decoder uv.EventDecoder
}

func New() *Framer { return &Framer{} }

// Pending reports buffered bytes waiting for continuation or an explicit flush.
func (f *Framer) Pending() bool { return len(f.buf) > 0 }

// NeedsDeadline identifies a bare Escape or ambiguous Alt control introducer.
// Other fragmented controls and paste terminators continue across reads.
func (f *Framer) NeedsDeadline() bool {
	if f.paste || len(f.buf) == 0 || len(f.buf) > 2 || f.buf[0] != ansi.ESC {
		return false
	}
	n, ev := f.decoder.Decode(f.buf)
	_, key := ev.(uv.KeyEvent)
	return n == len(f.buf) && key
}

func (f *Framer) Feed(raw []byte) ([]Packet, error) {
	f.buf = append(f.buf, raw...)
	var packets []Packet
	for len(f.buf) > 0 {
		if f.paste {
			packet, ok := f.nextPaste()
			if !ok {
				break
			}
			packets = append(packets, packet)
			continue
		}
		n := f.completeLength()
		if n > maxSequenceBytes {
			return packets, ErrSequenceTooLarge
		}
		if n == 0 {
			if len(f.buf) > maxSequenceBytes {
				return packets, ErrSequenceTooLarge
			}
			break
		}
		packet := f.take(n)
		if bytes.Equal(packet.Raw, pasteStart) {
			f.paste = true
			packet.Event = uv.PasteStartEvent{}
		} else {
			consumed, event := f.decoder.Decode(packet.Raw)
			if consumed == len(packet.Raw) {
				packet.Event = event
			}
		}
		packets = append(packets, packet)
	}
	return packets, nil
}

func (f *Framer) completeLength() int {
	if f.buf[0] == ansi.SS3 {
		return f.ss3Length(1)
	}
	if f.buf[0] == 0x1b {
		if len(f.buf) == 1 || !utf8.FullRune(f.buf[1:]) {
			return 0
		}
		switch f.buf[1] {
		case 'O':
			return f.ss3Length(2)
		case 'P', '[', ']', '_', '^', 'X':
			// Native terminal input control introducers need complete framing
			// before EventDecoder sees them; it accepts truncated controls.
		default:
			// Use the input decoder's Alt-prefix grammar, rather than the
			// output parser's escape-intermediate grammar for these keys.
			n, _ := f.decoder.Decode(f.buf)
			return n
		}
	}
	if bytes.HasPrefix(f.buf, x10Prefix) {
		// X10 appends three coordinate/action bytes after the CSI final byte.
		const x10Length = 6
		if len(f.buf) < x10Length {
			return 0
		}
		return x10Length
	}
	if !utf8.FullRune(f.buf) {
		return 0
	}
	// An Alt+Unicode sequence needs the full UTF-8 code point after Escape.
	if len(f.buf) > 1 && f.buf[0] == 0x1b && f.buf[1] >= utf8.RuneSelf && !utf8.FullRune(f.buf[1:]) {
		return 0
	}
	// Framing needs no collected parameters/data. A nil native Parser avoids
	// its fixed parameter storage while preserving its sequence state machine.
	_, _, n, state := ansi.DecodeSequence(f.buf, ansi.NormalState, nil)
	if state != ansi.NormalState {
		return 0
	}
	if n == len(f.buf)-1 && f.buf[n] == ansi.ESC {
		// The native output grammar cancels a string on a non-ST Escape. A
		// trailing Escape can still be the first half of a fragmented ST.
		switch f.buf[0] {
		case ansi.OSC, ansi.DCS, ansi.APC, ansi.SOS, ansi.PM:
			return 0
		case ansi.ESC:
			if len(f.buf) > 1 {
				switch f.buf[1] {
				case ']', 'P', '_', 'X', '^':
					return 0
				}
			}
		}
	}
	return n
}

func (f *Framer) ss3Length(prefix int) int {
	// The native decoder accepts SS3 plus optional numeric modifiers and one
	// GL final byte. An output parser sees ESC O itself as a complete escape.
	i := prefix
	for i < len(f.buf) && f.buf[i] >= '0' && f.buf[i] <= '9' {
		i++
	}
	if i == len(f.buf) {
		return 0
	}
	n, _ := f.decoder.Decode(f.buf[:i+1])
	return n
}

func (f *Framer) nextPaste() (Packet, bool) {
	if i := bytes.Index(f.buf, pasteEnd); i >= 0 {
		if i > 0 {
			packet := f.take(i)
			packet.Paste = true
			return packet, true
		}
		packet := f.take(len(pasteEnd))
		packet.Event = uv.PasteEndEvent{}
		f.paste = false
		return packet, true
	}
	// Retain only a possible terminator prefix, not the entire pasted body.
	keep := 0
	for n := 1; n < len(pasteEnd) && n <= len(f.buf); n++ {
		if bytes.Equal(f.buf[len(f.buf)-n:], pasteEnd[:n]) {
			keep = n
		}
	}
	if keep == len(f.buf) {
		return Packet{}, false
	}
	packet := f.take(len(f.buf) - keep)
	packet.Paste = true
	return packet, true
}

func (f *Framer) take(n int) Packet {
	packet := Packet{Raw: slices.Clone(f.buf[:n])}
	f.buf = f.buf[n:]
	return packet
}

// Flush passes incomplete controls as original bytes. At the caller's explicit
// deadline a bare Escape or two-byte Alt introducer becomes a native key event.
func (f *Framer) Flush() (Packet, bool) {
	if len(f.buf) == 0 {
		return Packet{}, false
	}
	keyDeadline := f.NeedsDeadline()
	packet := f.take(len(f.buf))
	packet.Paste = f.paste
	if keyDeadline {
		_, packet.Event = f.decoder.Decode(packet.Raw)
	}
	return packet, true
}
