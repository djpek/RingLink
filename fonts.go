package main

// Bundled UI font: SN Pro (SIL Open Font License 1.1, see fonts/OFL.txt).

import (
	"embed"

	"gioui.org/font"
	"gioui.org/font/opentype"
)

//go:embed fonts/*.ttf
var fontFS embed.FS

func uiFonts() []font.FontFace {
	var out []font.FontFace
	for _, f := range []struct {
		file string
		w    font.Weight
	}{{"SNPro-Regular.ttf", font.Normal}, {"SNPro-Medium.ttf", font.Medium}, {"SNPro-SemiBold.ttf", font.SemiBold}, {"SNPro-Bold.ttf", font.Bold}} {
		b, err := fontFS.ReadFile("fonts/" + f.file)
		if err != nil {
			continue
		}
		face, err := opentype.Parse(b)
		if err != nil {
			continue
		}
		out = append(out, font.FontFace{Font: font.Font{Typeface: "SN Pro", Weight: f.w}, Face: face})
	}
	return out
}
