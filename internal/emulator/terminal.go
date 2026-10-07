// Package emulator owns a libghostty terminal and copies its display and effects.
// The session goroutine serializes every operation; this package starts no
// goroutines and never writes either the PTY or the outer terminal.
package emulator

import (
	"errors"
	"fmt"
	"image/color"

	"github.com/charmbracelet/colorprofile"
	uv "github.com/charmbracelet/ultraviolet"
	ghostty "go.mitchellh.com/libghostty"
)

const (
	maxDimension           = 1<<16 - 1
	defaultUnknownMaxBytes = 64 << 10
	defaultVersion         = "go-tui-frame"
)

var (
	ErrClosed     = errors.New("emulator is closed")
	ErrProcessing = errors.New("native terminal reported a VT processing error")
)

// Size records the child grid and optional physical cell dimensions. Zero
// pixel dimensions mean that the outer terminal has not supplied them.
type Size struct {
	Cols, Rows                int
	CellWidthPx, CellHeightPx uint32
}

// Options configure the native terminal's virtual identity and storage.
// No device-attributes handler is registered, so DA queries receive
// libghostty's defaults: CSI ? 62 ; 22 c for DA1, CSI > 1 ; 0 ; 0 c for DA2
// and DCS ! | 00000000 ST for DA3 at the pinned revision. Programs end probe
// batches with DA1 and stop reading at its reply, so the child must get one.
// A handler cannot suppress the replies: despite the binding's documentation,
// libghostty answers with the same defaults when a handler declines a query.
// Clipboard handlers run synchronously during Write and must not reenter the
// terminal. Nil handlers produce Unsupported replies and observable requests.
type Options struct {
	Profile      *Profile
	Size         Size
	TerminfoName string
	Version      string
	// GraphemeWidth is the value and reset default of grapheme clustering
	// (mode 2027): true measures text by grapheme clusters, false with
	// wcwidth. It must match the outer screen's width rule, so the child's
	// cursor and wrapping agree with where its text is shown.
	GraphemeWidth                          bool
	UnknownMaxBytes                        uint
	ScrollbackMaxBytes, ScrollbackMaxLines *uint
	ClipboardRead                          func(ghostty.ClipboardRead) ghostty.ClipboardReadReply
	ClipboardWrite                         func(ghostty.ClipboardWrite) ghostty.ClipboardWriteReply
	// OuterColorProfile is the profile the outer renderer encodes colors
	// with. Where that renderer would downsample a host-reported palette
	// entry the child has not redefined, the entry stays a palette index, so
	// the outer terminal paints its own theme: every entry on ANSI256, entries
	// 0-15 on ANSI. Other entries and profiles keep the resolved RGB. The
	// renderer compares colors only by RGBA (charmbracelet/ultraviolet#205):
	// on ANSI256 an index and a color with the same xterm value can keep the
	// earlier color when adjacent or when one replaces the other; on ANSI only
	// entries 7 and 8 and a color of their xterm value can, when either
	// replaces the other. Both cases include an OSC 4 redefinition to the xterm
	// value and an OSC 104 reset from it.
	OuterColorProfile colorprofile.Profile
}

// Terminal owns native terminal, render state and reusable iterators.
// Its owner must serialize calls, including Close and Effects.
type Terminal struct {
	native                         *ghostty.Terminal
	render                         *ghostty.RenderState
	rows                           *ghostty.RenderStateRowIterator
	cells                          *ghostty.RenderStateRowCells
	keys                           *ghostty.KeyEncoder
	probe                          *ghostty.KeyEvent
	mouse                          *ghostty.MouseEncoder
	mouseProbe                     *ghostty.MouseEvent
	size                           Size
	visual                         visualState
	held                           bool
	effects                        []Effect
	callbackErr                    error
	scheme                         *ghostty.ColorScheme
	hostForeground, hostBackground *ghostty.ColorRGB
	hostPalette                    ghostty.Palette
	hostPaletteReported            [ghostty.PaletteSize]bool
	indexedEntries                 int
	text                           []byte
	layout                         cellLayout
	styles                         map[uint16]capturedStyle
	plainStyle                     capturedStyle
	style                          ghostty.Style
	cachedStyle                    uv.Style
	styleValid                     bool
	defaultStyle                   *ghostty.Style
	// Converting a color value to color.Color allocates, so captures reuse
	// converted colors. palette holds resolved entries, filled on first use;
	// foreground and background hold the default colors, nil where the
	// default rendition is kept. Both are reset when the render colors
	// change. lastRGB caches the most recent direct RGB color, which erased
	// runs and styled text repeat.
	palette                [ghostty.PaletteSize]color.Color
	foreground, background color.Color
	colorsInterned         bool
	lastRGB                ghostty.ColorRGB
	lastRGBColor           color.Color
}

func New(opts Options) (*Terminal, error) {
	if err := validSize(opts.Size); err != nil {
		return nil, err
	}
	if opts.UnknownMaxBytes == 0 {
		opts.UnknownMaxBytes = defaultUnknownMaxBytes
	}
	if opts.Version == "" {
		opts.Version = defaultVersion
	}
	layout, err := nativeCellLayout()
	if err != nil {
		return nil, err
	}
	t := &Terminal{
		size: opts.Size, layout: layout, defaultStyle: ghostty.DefaultStyle(),
		indexedEntries: indexedPaletteEntries(opts.OuterColorProfile),
	}
	if err := t.newRenderState(); err != nil {
		t.Close()
		return nil, err
	}
	nativeOpts := t.nativeOptions(opts)
	nativeOpts = append(nativeOpts, ghostty.WithSize(uint16(opts.Size.Cols), uint16(opts.Size.Rows)), ghostty.WithModeDefault(ghostty.ModeGraphemeCluster, opts.GraphemeWidth), ghostty.WithUnknownMaxBytes(opts.UnknownMaxBytes))
	if opts.TerminfoName != "" {
		nativeOpts = append(nativeOpts, ghostty.WithTerminfoName(opts.TerminfoName))
	}
	if opts.ScrollbackMaxBytes != nil {
		nativeOpts = append(nativeOpts, ghostty.WithMaxScrollbackBytes(*opts.ScrollbackMaxBytes))
	}
	if opts.ScrollbackMaxLines != nil {
		nativeOpts = append(nativeOpts, ghostty.WithMaxScrollbackLines(*opts.ScrollbackMaxLines))
	}
	t.native, err = ghostty.NewTerminal(nativeOpts...)
	if err != nil {
		t.Close()
		return nil, fmt.Errorf("creating native terminal: %w", err)
	}
	if err := t.initializeProfile(opts.Profile); err != nil {
		t.Close()
		return nil, err
	}
	// Storage also controls the native graphics protocol's capability replies.
	// The compositor renders text cells, so acknowledging image support would
	// tell the child to select a renderer this endpoint does not provide.
	if err := t.native.SetKittyImageStorageLimit(nil); err != nil {
		t.Close()
		return nil, fmt.Errorf("disabling unrendered native graphics: %w", err)
	}
	if err := t.Resize(opts.Size); err != nil {
		t.Close()
		return nil, err
	}
	return t, nil
}

func (t *Terminal) newRenderState() error {
	var err error
	t.render, err = ghostty.NewRenderState()
	if err != nil {
		return fmt.Errorf("creating native render state: %w", err)
	}
	t.rows, err = ghostty.NewRenderStateRowIterator()
	if err != nil {
		return fmt.Errorf("creating native row iterator: %w", err)
	}
	t.cells, err = ghostty.NewRenderStateRowCells()
	if err != nil {
		return fmt.Errorf("creating native cell iterator: %w", err)
	}
	return nil
}

func validSize(size Size) error {
	if size.Cols < 1 || size.Cols > maxDimension || size.Rows < 1 || size.Rows > maxDimension {
		return fmt.Errorf("terminal dimensions %dx%d are outside 1..%d", size.Cols, size.Rows, maxDimension)
	}
	return nil
}

// Write consumes exact child-output bytes in libghostty's incremental parser.
// Native semantic failures are checked separately from the binding's io.Writer
// method, which always reports success. Generated replies remain in Effects.
func (t *Terminal) Write(data []byte) (int, error) {
	if t == nil || t.native == nil {
		return 0, ErrClosed
	}
	t.callbackErr = nil
	t.native.VTWrite(data)
	failed, err := t.native.VTProcessingError()
	if err != nil {
		return len(data), errors.Join(t.callbackErr, fmt.Errorf("checking native parser: %w", err))
	}
	if failed {
		return len(data), errors.Join(t.callbackErr, ErrProcessing)
	}
	return len(data), errors.Join(t.callbackErr, t.checkGeometry())
}

func (t *Terminal) Resize(size Size) error {
	if t == nil || t.native == nil {
		return ErrClosed
	}
	if err := validSize(size); err != nil {
		return err
	}
	previous := t.size
	t.callbackErr = nil
	// Resize can synchronously query SizeFn for mode 2048's report.
	t.size = size
	if err := t.native.Resize(uint16(size.Cols), uint16(size.Rows), size.CellWidthPx, size.CellHeightPx); err != nil {
		t.size = previous
		return fmt.Errorf("resizing native terminal: %w", err)
	}
	return t.callbackErr
}

// ReleaseHold lets the session's deadline end an unclosed synchronized update.
// Native SetMode does not invoke the render-hold callback, so mirror the mode
// change explicitly and expose the same effect as a child-requested release.
func (t *Terminal) ReleaseHold() error {
	if t == nil || t.native == nil {
		return ErrClosed
	}
	if err := t.native.SetMode(ghostty.ModeSyncOutput, false); err != nil {
		return fmt.Errorf("releasing native render hold: %w", err)
	}
	if t.held {
		t.held = false
		t.effects = append(t.effects, Effect{Kind: RenderHold, Held: false})
	}
	return nil
}

func (t *Terminal) Close() {
	if t == nil {
		return
	}
	if t.keys != nil {
		t.keys.Close()
		t.keys = nil
	}
	if t.probe != nil {
		t.probe.Close()
		t.probe = nil
	}
	if t.mouse != nil {
		t.mouse.Close()
		t.mouse = nil
	}
	if t.mouseProbe != nil {
		t.mouseProbe.Close()
		t.mouseProbe = nil
	}
	if t.cells != nil {
		t.cells.Close()
		t.cells = nil
	}
	if t.rows != nil {
		t.rows.Close()
		t.rows = nil
	}
	if t.render != nil {
		t.render.Close()
		t.render = nil
	}
	if t.native != nil {
		t.native.Close()
		t.native = nil
	}
	t.visual = visualState{}
	t.text = nil
	t.styles = nil
	t.defaultStyle = nil
}
