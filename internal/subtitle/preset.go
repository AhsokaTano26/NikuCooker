package subtitle

import (
	"fmt"
	"sort"
	"strings"
)

// Preset is the styling an ASS file is written with.
//
// A preset is data rather than code so that adding one is a table entry. The
// values themselves are the whole reason ASS exists as a format: SubRip has no
// styling, and a font that renders Simplified Chinese correctly is not the font
// that renders Japanese correctly, so the choice is per-language-work and has to
// be configurable.
type Preset struct {
	Name string

	FontName string
	FontSize int

	// Colours are ASS colour literals. See assColour for why they are written
	// as strings rather than as RGB triples.
	PrimaryColour string
	OutlineColour string
	BackColour    string

	Bold    bool
	Italic  bool
	Spacing float64

	// BorderStyle is 1 for an outline plus shadow, 3 for an opaque box.
	BorderStyle int
	Outline     float64
	Shadow      float64

	// Alignment is the ASS numpad layout: 2 is bottom centre, 8 is top centre.
	Alignment int

	MarginL int
	MarginR int

	// MarginV is the distance from the edge, and which edge depends on
	// Alignment: from the bottom for 1-3, from the top for 7-9.
	MarginV int

	// PlayResX and PlayResY are the canvas the styling is authored against.
	//
	// They are not the video's dimensions. A player scales the script from this
	// canvas to whatever the video is, which is what makes one file look right
	// at 720p and 1080p. Authoring at 1920x1080 and letting the player scale
	// down is the convention, and it is the one that survives contact with
	// players that ignore video resolution entirely.
	PlayResX int
	PlayResY int
}

// presets is the table of built-in styles.
var presets = map[string]Preset{
	"default": {
		Name:          "Default",
		FontName:      "Noto Sans CJK SC",
		FontSize:      64,
		PrimaryColour: "&H00FFFFFF", // opaque white
		OutlineColour: "&H00000000", // opaque black
		BackColour:    "&H80000000", // half-transparent black
		BorderStyle:   1,
		Outline:       3,
		Shadow:        0,
		Alignment:     2,
		MarginL:       60,
		MarginR:       60,
		MarginV:       40,
		PlayResX:      1920,
		PlayResY:      1080,
	},

	// Fansub is the default because it is what this project is for: a font that
	// is actually installed on machines that display Simplified Chinese, a
	// heavier outline for legibility over busy video, and a slight shadow.
	"fansub": {
		Name:          "Fansub",
		FontName:      "Noto Sans CJK SC",
		FontSize:      64,
		PrimaryColour: "&H00FFFFFF",
		OutlineColour: "&H00000000",
		BackColour:    "&H80000000",
		Bold:          false,
		BorderStyle:   1,
		Outline:       3.5,
		Shadow:        1,
		Alignment:     2,
		MarginL:       60,
		MarginR:       60,
		MarginV:       45,
		PlayResX:      1920,
		PlayResY:      1080,
	},

	// Broadcast follows the convention streaming services settled on: a
	// semi-transparent box rather than an outline, which is more legible over
	// arbitrary video at the cost of covering more of it.
	"broadcast": {
		Name:          "Broadcast",
		FontName:      "Noto Sans CJK SC",
		FontSize:      60,
		PrimaryColour: "&H00FFFFFF",
		OutlineColour: "&H00000000",
		BackColour:    "&HB4000000",
		BorderStyle:   3,
		Outline:       1,
		Shadow:        0,
		Alignment:     2,
		MarginL:       80,
		MarginR:       80,
		MarginV:       50,
		PlayResX:      1920,
		PlayResY:      1080,
	},
}

// DefaultPresetName is used when none is configured.
const DefaultPresetName = "fansub"

// PresetByName returns a built-in preset.
//
// The lookup is case-insensitive because the value comes from a configuration
// file a person edits, and "Fansub" failing where "fansub" works is a papercut
// with no upside.
func PresetByName(name string) (Preset, error) {
	key := strings.ToLower(strings.TrimSpace(name))
	if key == "" {
		key = DefaultPresetName
	}

	preset, ok := presets[key]
	if !ok {
		return Preset{}, fmt.Errorf("subtitle: %q is not a known style preset; use one of %s",
			name, strings.Join(PresetNames(), ", "))
	}
	return preset, nil
}

// PresetNames lists the built-in presets, sorted.
func PresetNames() []string {
	names := make([]string, 0, len(presets))
	for name := range presets {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// assColour converts a `#rrggbb` or `#rrggbbaa` colour into an ASS literal.
//
// ASS stores colours as `&HAABBGGRR` — alpha first and the colour channels in
// reverse order. Writing `#RRGGBB` straight into a style produces a file that
// renders, in the wrong colour, with no error anywhere, which is why the
// conversion is a function with a test rather than an fmt.Sprintf at the call
// site.
func assColour(hex string) (string, error) {
	trimmed := strings.TrimPrefix(strings.TrimSpace(hex), "#")

	var r, g, b, a uint8
	a = 0x00 // ASS alpha runs the other way: 00 is opaque, FF is transparent.

	switch len(trimmed) {
	case 6:
		if _, err := fmt.Sscanf(trimmed, "%02x%02x%02x", &r, &g, &b); err != nil {
			return "", fmt.Errorf("subtitle: %q is not a colour", hex)
		}
	case 8:
		// #rrggbbaa, matching CSS.
		if _, err := fmt.Sscanf(trimmed, "%02x%02x%02x%02x", &r, &g, &b, &a); err != nil {
			return "", fmt.Errorf("subtitle: %q is not a colour", hex)
		}
		// CSS alpha is opacity, ASS alpha is transparency.
		a = 0xFF - a
	default:
		return "", fmt.Errorf("subtitle: %q is not a colour; expected #rrggbb or #rrggbbaa", hex)
	}

	return fmt.Sprintf("&H%02X%02X%02X%02X", a, b, g, r), nil
}
