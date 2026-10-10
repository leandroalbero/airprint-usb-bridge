package ql

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"sort"
	"strings"
)

//go:embed profiles/*.json
var profileFS embed.FS

// BrotherVendorID is Brother's USB vendor ID.
const BrotherVendorID = 0x04F9

// EmbeddedProfile is internal/ql/profiles/ql700.json.
func EmbeddedProfile() (*Profile, error) { return embeddedProfile("ql700") }

func embeddedProfile(stem string) (*Profile, error) {
	b, err := profileFS.ReadFile("profiles/" + stem + ".json")
	if err != nil {
		return nil, fmt.Errorf("embedded profile %s: %w", stem, err)
	}
	var p Profile
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, fmt.Errorf("embedded profile %s: %w", stem, err)
	}
	if err := p.validate(); err != nil {
		return nil, fmt.Errorf("embedded profile %s: %w", stem, err)
	}
	return &p, nil
}

// EmbeddedProfileNames lists the built-in profiles by file stem ("ql700", "ql820nwb", ...).
func EmbeddedProfileNames() []string {
	entries, _ := fs.ReadDir(profileFS, "profiles")
	var out []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") {
			out = append(out, strings.TrimSuffix(e.Name(), ".json"))
		}
	}
	sort.Slice(out, func(i, j int) bool { return modelLess(out[i], out[j]) })
	return out
}

// modelLess orders ql500 < ql700 < ql1050 rather than by string.
func modelLess(a, b string) bool {
	na, nb := leadingNumber(a), leadingNumber(b)
	if na != nb {
		return na < nb
	}
	return a < b
}

func leadingNumber(s string) int {
	n := 0
	for _, r := range strings.TrimPrefix(s, "ql") {
		if r < '0' || r > '9' {
			break
		}
		n = n*10 + int(r-'0')
	}
	return n
}

// ModelKey normalises a model name for lookup: "Brother QL-820NWB", "QL-820NWB" and "ql820nwb"
// all give "ql820nwb".
func ModelKey(model string) string {
	s := strings.ToLower(strings.TrimSpace(model))
	s = strings.TrimPrefix(s, "brother")
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// EmbeddedProfileByName returns a built-in profile by name or model ("ql820nwb", "QL-820NWB").
func EmbeddedProfileByName(name string) (*Profile, bool, error) {
	key := ModelKey(name)
	if key == "" {
		return nil, false, nil
	}
	for _, stem := range EmbeddedProfileNames() {
		if stem == key {
			p, err := embeddedProfile(stem)
			return p, true, err
		}
	}
	return nil, false, nil
}

// ProfileForModel picks the built-in profile for an IEEE 1284 MDL value. Brother sells some
// models with a trailing "c" (QL-820NWBc), which drives like the plain model.
func ProfileForModel(model string) (*Profile, bool, error) {
	p, ok, err := EmbeddedProfileByName(model)
	if ok || err != nil {
		return p, ok, err
	}
	if key := ModelKey(model); strings.HasSuffix(key, "c") {
		return EmbeddedProfileByName(strings.TrimSuffix(key, "c"))
	}
	return nil, false, nil
}

// ProfileForUSB picks the built-in profile whose USB vendor and product ID match.
func ProfileForUSB(vendor, product uint16) (*Profile, bool, error) {
	if vendor != BrotherVendorID || product == 0 {
		return nil, false, nil
	}
	for _, stem := range EmbeddedProfileNames() {
		p, err := embeddedProfile(stem)
		if err != nil {
			return nil, false, err
		}
		if p.USBVendor == vendor && (p.USBProduct == product || p.USBProductEditor == product) {
			return p, true, nil
		}
	}
	return nil, false, nil
}

// EmbeddedModels lists the models the built-in profiles cover, e.g. "QL-700".
func EmbeddedModels() []string {
	var out []string
	for _, stem := range EmbeddedProfileNames() {
		if p, err := embeddedProfile(stem); err == nil {
			out = append(out, p.Model)
		}
	}
	return out
}
