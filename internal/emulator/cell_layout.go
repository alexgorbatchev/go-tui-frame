package emulator

import (
	"encoding/json"
	"fmt"
	"sync"

	ghostty "go.mitchellh.com/libghostty"
)

const (
	cellManifestSchema = 1
	packedCellBits     = 64
)

type packedDescriptor struct {
	Kind, Underlying string
	Size, Width      uint
	Bits             map[string]packedField
}

type packedField struct {
	LSB, Width      uint
	Kind, Tag, Type string
	Arms            map[string]packedDescriptor
}

type cellBits struct {
	shift uint
	mask  uint64
}

func (b cellBits) value(raw uint64) uint64 { return raw >> b.shift & b.mask }

type cellLayout struct {
	tag, content, wide, style, linked cellBits
	codepoint, grapheme               cellBits
	palette, red, green, blue         cellBits
}

type cellData struct {
	codepoint uint32
	tag       ghostty.CellContentTag
	wide      ghostty.CellWide
	styleID   uint16
	linked    bool
	palette   uint8
	rgb       ghostty.ColorRGB
}

// The manifest belongs to the linked native library and is immutable for the
// process. Parse once so capture has no JSON work or per-property cgo calls.
var nativeCellLayout = sync.OnceValues(func() (cellLayout, error) {
	return parseCellLayout(ghostty.TypeJSON())
})

func parseCellLayout(data string) (cellLayout, error) {
	var manifest struct {
		Schema uint
		Types  struct {
			Cell packedDescriptor `json:"GhosttyCell"`
		}
	}
	if err := json.Unmarshal([]byte(data), &manifest); err != nil {
		return cellLayout{}, fmt.Errorf("decoding Ghostty type manifest: %w", err)
	}
	if manifest.Schema != cellManifestSchema {
		return cellLayout{}, fmt.Errorf("unsupported Ghostty type manifest schema %d", manifest.Schema)
	}
	cell := manifest.Types.Cell
	if cell.Kind != "packed" || cell.Underlying != "u64" || cell.Size != packedCellBits/8 {
		return cellLayout{}, fmt.Errorf("unsupported GhosttyCell storage: %s %s (%d bytes)", cell.Kind, cell.Underlying, cell.Size)
	}
	var layout cellLayout
	for _, field := range []struct {
		name, typeName string
		maxWidth       uint
		dst            *cellBits
	}{
		{"content_tag", "GhosttyCellContentTag", 32, &layout.tag}, {"content", "", packedCellBits, &layout.content},
		{"wide", "GhosttyCellWide", 32, &layout.wide}, {"style_id", "GhosttyStyleId", 16, &layout.style}, {"hyperlink", "bool", 1, &layout.linked},
	} {
		descriptor := cell.Bits[field.name]
		if descriptor.Type != field.typeName || descriptor.Width > field.maxWidth {
			return cellLayout{}, fmt.Errorf("unsupported GhosttyCell %s type %s (%d bits)", field.name, descriptor.Type, descriptor.Width)
		}
		bits, err := parseCellBits(cell.Bits, field.name, packedCellBits)
		if err != nil {
			return cellLayout{}, err
		}
		*field.dst = bits
	}
	content := cell.Bits["content"]
	if content.Kind != "union" || content.Tag != "content_tag" {
		return cellLayout{}, fmt.Errorf("unsupported GhosttyCell content union: %s tagged by %s", content.Kind, content.Tag)
	}
	for _, arm := range []struct {
		name, field, typeName string
		width                 uint
		dst                   *cellBits
	}{
		{"CODEPOINT", "codepoint", "u21", 21, &layout.codepoint},
		{"CODEPOINT_GRAPHEME", "codepoint", "u21", 21, &layout.grapheme},
		{"BG_COLOR_PALETTE", "index", "GhosttyColorPaletteIndex", 8, &layout.palette},
		{"BG_COLOR_RGB", "r", "u8", 8, &layout.red},
		{"BG_COLOR_RGB", "g", "u8", 8, &layout.green},
		{"BG_COLOR_RGB", "b", "u8", 8, &layout.blue},
	} {
		descriptor := content.Arms[arm.name]
		if descriptor.Kind != "packed" || descriptor.Width == 0 || descriptor.Width > content.Width {
			return cellLayout{}, fmt.Errorf("unsupported GhosttyCell %s content arm", arm.name)
		}
		field := descriptor.Bits[arm.field]
		if field.Type != arm.typeName || field.Width != arm.width {
			return cellLayout{}, fmt.Errorf("unsupported GhosttyCell %s %s type", arm.name, arm.field)
		}
		bits, err := parseCellBits(descriptor.Bits, arm.field, descriptor.Width)
		if err != nil {
			return cellLayout{}, fmt.Errorf("reading GhosttyCell %s: %w", arm.name, err)
		}
		*arm.dst = bits
	}
	return layout, nil
}

func parseCellBits(fields map[string]packedField, name string, limit uint) (cellBits, error) {
	field, ok := fields[name]
	if !ok || field.Width == 0 || field.Width > limit || field.LSB > limit-field.Width {
		return cellBits{}, fmt.Errorf("invalid GhosttyCell %s bit range (%d + %d in %d bits)", name, field.LSB, field.Width, limit)
	}
	return cellBits{shift: field.LSB, mask: ^uint64(0) >> (packedCellBits - field.Width)}, nil
}

func (l cellLayout) decode(packed uint64) cellData {
	data := cellData{
		tag: ghostty.CellContentTag(l.tag.value(packed)), wide: ghostty.CellWide(l.wide.value(packed)),
		styleID: uint16(l.style.value(packed)), linked: l.linked.value(packed) != 0,
	}
	switch data.tag {
	case ghostty.CellContentCodepoint:
		data.codepoint = uint32(l.codepoint.value(l.content.value(packed)))
	case ghostty.CellContentCodepointGrapheme:
		data.codepoint = uint32(l.grapheme.value(l.content.value(packed)))
	case ghostty.CellContentBgColorPalette:
		data.palette = uint8(l.palette.value(l.content.value(packed)))
	case ghostty.CellContentBgColorRGB:
		content := l.content.value(packed)
		data.rgb = ghostty.ColorRGB{R: uint8(l.red.value(content)), G: uint8(l.green.value(content)), B: uint8(l.blue.value(content))}
	}
	return data
}
