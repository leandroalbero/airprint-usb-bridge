package ql

import (
	"encoding/json"
	"fmt"
	"os"
)

func loadProfile(path string) (*Profile, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("load profile %s: %w", path, err)
	}
	var p Profile
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, fmt.Errorf("load profile %s: %w", path, err)
	}
	if err := p.validate(); err != nil {
		return nil, fmt.Errorf("load profile %s: %w", path, err)
	}
	return &p, nil
}

func (p *Profile) validate() error {
	if p.Model == "" {
		return fmt.Errorf("model is empty")
	}
	if p.BytesPerLine <= 0 || p.BytesPerLine > 255 {
		return fmt.Errorf("bytes_per_line %d", p.BytesPerLine)
	}
	if p.Pins != p.BytesPerLine*8 {
		return fmt.Errorf("pins %d != bytes_per_line*8", p.Pins)
	}
	if p.DPI <= 0 {
		return fmt.Errorf("dpi %d", p.DPI)
	}
	if err := checkProvenance("continuous_margin_dots", p.MarginDots.Source); err != nil {
		return err
	}
	if err := checkProvenance("continuous_page_length_dots", p.PageLengthDots.Source); err != nil {
		return err
	}
	seen := make(map[string]struct{}, len(p.Media))
	for i, m := range p.Media {
		if m.ID == "" {
			return fmt.Errorf("media[%d]: empty id", i)
		}
		if _, ok := seen[m.ID]; ok {
			return fmt.Errorf("media %q: duplicate id", m.ID)
		}
		seen[m.ID] = struct{}{}
		if m.Kind != MediaContinuous && m.Kind != MediaDieCut {
			return fmt.Errorf("media %q: kind 0x%02x", m.ID, m.Kind)
		}
		if m.WidthMM <= 0 || m.PrintPins <= 0 {
			return fmt.Errorf("media %q: width or print pins", m.ID)
		}
		if m.LeftPins+m.PrintPins+m.RightPins != p.Pins {
			return fmt.Errorf("media %q: pin columns %d != pins %d", m.ID, m.LeftPins+m.PrintPins+m.RightPins, p.Pins)
		}
	}
	return nil
}

func checkProvenance(field string, s Provenance) error {
	if s != Documented && s != Measured {
		return fmt.Errorf("%s: provenance %q", field, s)
	}
	return nil
}

func detectMedia(p *Profile, s Status) (Media, bool) {
	if p == nil {
		return Media{}, false
	}
	for _, m := range p.Media {
		if m.Kind == s.MediaKind && m.WidthMM == s.MediaWidthMM && m.LengthMM == s.MediaLengthMM {
			return m, true
		}
	}
	return Media{}, false
}

// UnknownMedia is the label for a status the profile does not list.
func UnknownMedia(s Status) string {
	return fmt.Sprintf("unknown media %d×%d type 0x%02X", s.MediaWidthMM, s.MediaLengthMM, byte(s.MediaKind))
}

// Warnings lists documented limits the firmware does not enforce. Encoding is unchanged.
func Warnings(p *Profile, j Job) []string {
	if p == nil {
		return nil
	}
	var out []string
	if j.Media.Kind == MediaContinuous {
		lo, hi := p.MarginDots.V[0], p.MarginDots.V[1]
		if hi != 0 && (j.Options.MarginDots < lo || j.Options.MarginDots > hi) {
			out = append(out, fmt.Sprintf("margin %d dots outside documented range %d-%d", j.Options.MarginDots, lo, hi))
		}
		plo, phi := p.PageLengthDots.V[0], p.PageLengthDots.V[1]
		if phi != 0 {
			for i, page := range j.Pages {
				if page == nil {
					continue
				}
				if page.H < plo || page.H > phi {
					out = append(out, fmt.Sprintf("page %d length %d dots outside documented range %d-%d", i, page.H, plo, phi))
				}
			}
		}
	}
	if j.Media.FixedMargin != 0 && j.Options.MarginDots != j.Media.FixedMargin {
		out = append(out, fmt.Sprintf("margin %d dots differs from fixed margin %d for media %s", j.Options.MarginDots, j.Media.FixedMargin, j.Media.ID))
	}
	return out
}

// LengthMM converts lengthwise dots to millimetres. High-res is 600 dpi along the feed.
func LengthMM(dpi, dots int, highRes bool) int {
	if dpi <= 0 {
		dpi = 300
	}
	if highRes {
		dpi *= 2
	}
	if dots <= 0 {
		return 0
	}
	return (dots*254 + dpi*5) / (dpi * 10)
}
