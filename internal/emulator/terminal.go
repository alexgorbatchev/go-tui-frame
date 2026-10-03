// Package emulator owns a libghostty terminal and copies its display and effects.
// The session goroutine serializes every operation; this package starts no
// goroutines and never writes either the PTY or the outer terminal.
package emulator

import (
	"errors"
	"fmt"

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
// DeviceAttributes is supplied by the wrapper's supported capability profile;
// nil suppresses DA replies rather than advertising libghostty's renderer.
// Clipboard handlers run synchronously during Write and must not reenter the
// terminal. Nil handlers produce Unsupported replies and observable requests.
type Options struct {
	Profile                                *Profile
	Size                                   Size
	TerminfoName                           string
	Version                                string
	GraphemeWidth                          bool
	DeviceAttributes                       *ghostty.DeviceAttributes
	UnknownMaxBytes                        uint
	ScrollbackMaxBytes, ScrollbackMaxLines *uint
	ClipboardRead                          func(ghostty.ClipboardRead) ghostty.ClipboardReadReply
	ClipboardWrite                         func(ghostty.ClipboardWrite) ghostty.ClipboardWriteReply
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
	size                           Size
	visual                         visualState
	held                           bool
	effects                        []Effect
	callbackErr                    error
	scheme                         *ghostty.ColorScheme
	hostForeground, hostBackground *ghostty.ColorRGB
	text                           []byte
	style                          ghostty.Style
	cachedStyle                    uv.Style
	styleValid                     bool
	defaultStyle                   *ghostty.Style
}

func New(opts Options) (*Terminal, error) {
	if err := validSize(opts.Size); err != nil {
		return nil, err
	}
	if opts.DeviceAttributes != nil && (opts.DeviceAttributes.Primary.NumFeatures < 0 || opts.DeviceAttributes.Primary.NumFeatures > len(opts.DeviceAttributes.Primary.Features)) {
		return nil, errors.New("device attributes have an invalid feature count")
	}
	if opts.UnknownMaxBytes == 0 {
		opts.UnknownMaxBytes = defaultUnknownMaxBytes
	}
	if opts.Version == "" {
		opts.Version = defaultVersion
	}
	t := &Terminal{size: opts.Size, defaultStyle: ghostty.DefaultStyle()}
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
	var err error
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
	t.defaultStyle = nil
}
