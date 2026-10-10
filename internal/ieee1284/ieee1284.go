// Package ieee1284 parses IEEE 1284 device ID strings such as
// "MFG:Brother;CMD:PT-CBP;MDL:QL-700;CLS:PRINTER;".
package ieee1284

import (
	"sort"
	"strings"
)

// ID is a parsed device ID. Keys are stored upper-case; values keep their case.
type ID struct {
	Raw    string
	Fields map[string]string
}

// Parse splits s into key:value pairs. It never fails: pairs without a colon are dropped, and the
// first occurrence of a key wins.
func Parse(s string) ID {
	id := ID{Raw: s, Fields: map[string]string{}}
	for _, part := range strings.Split(s, ";") {
		k, v, ok := strings.Cut(part, ":")
		if !ok {
			continue
		}
		k = strings.ToUpper(strings.TrimSpace(k))
		if k == "" {
			continue
		}
		if _, dup := id.Fields[k]; !dup {
			id.Fields[k] = strings.TrimSpace(v)
		}
	}
	return id
}

// Get returns the first non-empty value among keys (upper-case).
func (id ID) Get(keys ...string) string {
	for _, k := range keys {
		if v := id.Fields[k]; v != "" {
			return v
		}
	}
	return ""
}

// Empty reports whether no field was parsed.
func (id ID) Empty() bool { return len(id.Fields) == 0 }

func (id ID) Manufacturer() string { return id.Get("MFG", "MANUFACTURER") }
func (id ID) Model() string        { return id.Get("MDL", "MODEL") }
func (id ID) Class() string        { return id.Get("CLS", "CLASS") }
func (id ID) Description() string  { return id.Get("DES", "DESCRIPTION") }

// CommandSet returns the CMD tokens, trimmed, in order.
func (id ID) CommandSet() []string {
	raw := id.Get("CMD", "COMMAND SET")
	if raw == "" {
		return nil
	}
	var out []string
	for _, t := range strings.Split(raw, ",") {
		if t = strings.TrimSpace(t); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// HasCommand reports whether any CMD token satisfies match, compared upper-case.
func (id ID) HasCommand(match func(token string) bool) bool {
	for _, t := range id.CommandSet() {
		if match(strings.ToUpper(t)) {
			return true
		}
	}
	return false
}

// String renders the fields in a stable order, MFG and MDL first.
func (id ID) String() string {
	if id.Empty() {
		return id.Raw
	}
	keys := make([]string, 0, len(id.Fields))
	for k := range id.Fields {
		keys = append(keys, k)
	}
	rank := func(k string) int {
		switch k {
		case "MFG", "MANUFACTURER":
			return 0
		case "MDL", "MODEL":
			return 1
		case "CMD", "COMMAND SET":
			return 2
		}
		return 3
	}
	sort.Slice(keys, func(i, j int) bool {
		if ri, rj := rank(keys[i]), rank(keys[j]); ri != rj {
			return ri < rj
		}
		return keys[i] < keys[j]
	})
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k + ":" + id.Fields[k] + ";")
	}
	return b.String()
}

// Match implements --match: "MFG:MDL" matches when the manufacturer contains MFG and the model
// contains MDL, case-insensitively; either side may be empty. A pattern without a colon matches
// the manufacturer or the model. An empty pattern matches everything.
func (id ID) Match(pattern string) bool {
	return MatchNames(id.Manufacturer(), id.Model(), pattern)
}

// MatchNames is Match for a manufacturer and model known from elsewhere (e.g. USB strings).
func MatchNames(mfg, mdl, pattern string) bool {
	if pattern == "" {
		return true
	}
	mfg, mdl = strings.ToLower(mfg), strings.ToLower(mdl)
	pattern = strings.ToLower(pattern)
	pm, pd, ok := strings.Cut(pattern, ":")
	if !ok {
		return strings.Contains(mfg, pattern) || strings.Contains(mdl, pattern)
	}
	return strings.Contains(mfg, pm) && strings.Contains(mdl, pd)
}
