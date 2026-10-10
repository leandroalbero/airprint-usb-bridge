package ql

import (
	_ "embed"
	"encoding/json"
	"fmt"
)

//go:embed profiles/ql700.json
var ql700JSON []byte

// EmbeddedProfile is internal/ql/profiles/ql700.json.
func EmbeddedProfile() (*Profile, error) {
	var p Profile
	if err := json.Unmarshal(ql700JSON, &p); err != nil {
		return nil, fmt.Errorf("embedded profile: %w", err)
	}
	if err := p.validate(); err != nil {
		return nil, fmt.Errorf("embedded profile: %w", err)
	}
	return &p, nil
}
