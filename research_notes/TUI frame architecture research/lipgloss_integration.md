---
created_on: 2026-10-01 11:37
last_modified: 2026-10-01 12:06
status: current
---

# Native Lip Gloss integration research

This note gives the README author verified third-party contracts for the proposed consumer callbacks. It is research only: the frame runtime, typed controller, snapshots, and update mechanisms are not implemented. No dependencies are added to the repository. The [README](../../README.md) specifies `New(cmd, initialData)` returning a live typed `*Frame[T]`, with `Header(rows, draw)` receiving a last-argument callback of type `func(frame.DrawContext[T])`. The proposed drawing value contains `Term frame.Snapshot`, `View *lipgloss.Canvas`, and `Data T`. The application submits header data directly with `app.InvalidateHeader(data)`. These frame names remain design vocabulary.

## Versions and native contract

The latest stable Lip Gloss release is [v2.0.6](https://github.com/charmbracelet/lipgloss/releases/tag/v2.0.6), released August 11, 2026. Its [module manifest](https://github.com/charmbracelet/lipgloss/blob/v2.0.6/go.mod) declares `charm.land/lipgloss/v2`, Go 1.25.0, and Ultraviolet `github.com/charmbracelet/ultraviolet v0.0.0-20260811164956-006e29f97886`. The current main branches are inspected separately; stable API claims below use the release and its pinned UV commit. Both projects use MIT licenses according to their package documentation: [Lip Gloss](https://pkg.go.dev/charm.land/lipgloss/v2) and [Ultraviolet](https://pkg.go.dev/github.com/charmbracelet/ultraviolet). UV's repository currently has no tagged releases and warns its API may change in its [README](https://github.com/charmbracelet/ultraviolet).

Lip Gloss v2 already supplies a real native cell buffer. `NewCanvas(width, height int) *Canvas` creates an internal UV screen buffer. `Canvas` implements both `uv.Screen` and `uv.Drawable`; the native methods are `Bounds() uv.Rectangle`, `Width() int`, `Height() int`, `CellAt(x, y int) *uv.Cell`, `SetCell(x, y int, cell *uv.Cell)`, `WidthMethod() uv.WidthMethod`, `Compose(drawer uv.Drawable) *Canvas`, and `Draw(scr uv.Screen, area uv.Rectangle)`. `Compose` invokes the drawable on the whole canvas bounds. `Render() string` serializes styled text and trims trailing spaces; it does not expose a separate cell painting contract. A native callback therefore needs no parallel `frame.Style`, `frame.Color`, or custom text canvas. [Canvas source](https://github.com/charmbracelet/lipgloss/blob/v2.0.6/canvas.go).

UV's [Screen and Drawable interfaces](https://github.com/charmbracelet/ultraviolet/blob/006e29f97886/uv.go) use cell access and `Draw(scr Screen, area Rectangle)`. A Screen is a paint target; implementing it does not acquire terminal stdin, raw mode, or an input loop. Native Lip Gloss styling and offscreen composition can be used without running Bubble Tea.

## Source-grounded callback body

The following is an illustrative native Lip Gloss function, checked against source signatures, not compiled or executed during this research. The proposed last-argument Header or Footer callback receives one `frame.DrawContext[T]` value; it can pass `ctx.View` to this helper and obtain text from `ctx.Data` or the child state in `ctx.Term`. The same primitive can paint a footer with a different native Style and metadata text.

```go
func paintBar(c *lipgloss.Canvas, style lipgloss.Style, text string) {
	b := c.Bounds()
	if b.Empty() {
		return
	}
	text = style.
		Width(b.Dx()).Height(b.Dy()).
		MaxWidth(b.Dx()).MaxHeight(b.Dy()).
		Render(text)
	c.Compose(lipgloss.NewLayer(text))
}

headerStyle := lipgloss.NewStyle().
	Background(lipgloss.Color("#B91C1C")).
	Foreground(lipgloss.Color("#FFFFFF")).
	Bold(true).Padding(0, 1).Align(lipgloss.Left)
```

`Background` and `Foreground` accept `color.Color`; `lipgloss.Color` supplies that value. Native Style's value-returning fluent setters include Bold, Width, Height, Align, Padding, MaxWidth, and MaxHeight. Width describes the block before margins, including padding/borders. Height enlarges a short block; it is not a height cap. `Padding(0, 1)` means zero vertical and one horizontal column. [Setters](https://github.com/charmbracelet/lipgloss/blob/v2.0.6/set.go).

`Style.Render` returns ANSI styled text. Width wrapping and height alignment happen before final MaxWidth/MaxHeight truncation. Both caps apply only when greater than zero, hence the empty-region guard. Padding, aligned whitespace, and background styling occur in the native render path. [Render implementation](https://github.com/charmbracelet/lipgloss/blob/v2.0.6/style.go).

An origin-local `NewLayer(text)` composed onto the entire header/footer canvas is valid. For positioning, use `c.Compose(lipgloss.NewCompositor(lipgloss.NewLayer(text).X(x).Y(y).Z(z)))`. `NewLayer(content string, layers ...*Layer) *Layer` and `NewCompositor(layers ...*Layer) *Compositor` are current signatures. Layer X/Y positions are relative to its parent. A Layer's own Draw does not apply its X/Y/Z or child hierarchy; the Compositor applies those positions. Call Compositor.Refresh after changing a layer tree or positions. Its Draw checks overlap but does not intersect layer bounds with the supplied area or translate positions by that area's origin. [Layer/compositor source](https://github.com/charmbracelet/lipgloss/blob/v2.0.6/layer.go). The official [canvas example](https://github.com/charmbracelet/lipgloss/blob/main/examples/canvas/main.go) uses NewCompositor; the README's `compositor.Compose(...)` fragment is inconsistent with this verified API.

## Engine composition and ownership implications

Recommended integration, inferred from the native contracts: allocate a zero-origin region-sized Canvas for each consumer callback; paint only that local buffer; draw the completed buffer into the outer UV surface using its exact destination rectangle. Keep the child emulator's cell grid as cells through final composition. This uses LG/UV's intended primitives rather than serializing the child grid into an ANSI string and reparsing it for a custom proxy canvas.

`uv.Rectangle` aliases `image.Rectangle`; `uv.Rect(x, y, w, h)` sets Min to `(x,y)` and Max to `(x+w,y+h)`. Buffer.Draw maps source `(0,0)` to destination `area.Min`, reads source `(x-area.Min.X, y-area.Min.Y)`, and writes native cells with SetCell. For a local canvas of dimensions `w,h`, `c.Draw(screen, uv.Rect(x,y,w,h))` places it at `(x,y)`. Buffer.Draw skips nil/zero cells and does not clear the target first; initialize/clear the destination surface each full composition or maintain an explicit damage strategy. SetCell stores cells by value and handles wide-cell continuations and buffer-edge overflow. Passing a narrow region rectangle on a larger shared screen is not by itself a complete clipping boundary for every drawable. [UV buffer source](https://github.com/charmbracelet/ultraviolet/blob/006e29f97886/buffer.go).

A [UV Cell](https://github.com/charmbracelet/ultraviolet/blob/006e29f97886/cell.go) contains a grapheme string, display width, native Style, and hyperlink. Preserve these values when mapping child emulator cells. CellAt returns a pointer into live storage; Cell.Clone returns a value copy. Recommended callback ownership is borrowed for the duration of the paint call, serialized with resizing/composition, without retaining pointers or concurrent mutation. This ownership policy is an integration recommendation, not a claimed synchronization guarantee from Lip Gloss.

[UV StyledString](https://github.com/charmbracelet/ultraviolet/blob/006e29f97886/styled.go) decomposes ANSI SGR styling and hyperlinks into cells. Its Draw clears the supplied area before painting. That supports a styled bar on a separate region canvas, but can erase unrelated content if the whole outer screen is passed. It is not a complete child VT emulator: the source handles styled strings and explicitly leaves other control-sequence behavior unfinished. Use the selected terminal emulator for raw child PTY output.

Canvas construction explicitly chooses `ansi.GraphemeWidth`, while the pinned UV NewScreenBuffer defaults to `ansi.WcWidth`. The engine must resolve cell-width policy and terminal grapheme-mode negotiation consistently with child emulation; composing incompatible widths can misplace the child or chrome. The hardcoded Canvas method has no public setter in this release. This is a cross-component contract requirement, not evidence that LG alone negotiates the outer terminal. [Canvas](https://github.com/charmbracelet/lipgloss/blob/v2.0.6/canvas.go), [UV defaults](https://github.com/charmbracelet/ultraviolet/blob/006e29f97886/buffer.go).

Do not call Lip Gloss BackgroundColor/HasDarkBackground from paint callbacks while the frame owns terminal input: their standalone implementation changes raw mode and issues terminal queries. The frame's own protocol router must own those requests and replies. Do not use Lip Gloss Print/Println inside callbacks because direct writes bypass the composed surface. Native Style.Render plus Canvas.Compose perform offscreen work and do not need a Bubble Tea input loop. [Background-query source](https://github.com/charmbracelet/lipgloss/blob/v2.0.6/query.go), [query transport](https://github.com/charmbracelet/lipgloss/blob/v2.0.6/terminal.go).

## Research evidence and limits

The following commands execute successfully and fetch upstream source into task-owned ignored scratch directories after reading the gitsnip skill and help reference:

```sh
gitsnip https://github.com/charmbracelet/lipgloss . .tmp/gitsnip/lipgloss/main/full -b main -q
gitsnip https://github.com/charmbracelet/ultraviolet . .tmp/gitsnip/ultraviolet/main/full -b main -q
```

Both snapshots are inspected with codegraph status/init/explore/node and targeted rg fallback. Codegraph reports parse/read failures (LG 56, UV 44); successful symbol/source reads and direct stable raw-source web reads ground the APIs above. The snapshots' main branches and the latest UV package pseudo-version are not substituted for LG v2.0.6's pinned dependency. No terminal experiment, integration test, compile check of the illustrative snippet, library selection, or module dependency change is performed. Existing scaffold verification remains in the local ignored [scaffold-checks.log](../../.tmp/scaffold-checks.log).
