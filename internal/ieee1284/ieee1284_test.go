package ieee1284

import (
	"slices"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	id := Parse("MFG:Brother;CMD:PT-CBP;MDL:QL-700;CLS:PRINTER;")
	if id.Manufacturer() != "Brother" || id.Model() != "QL-700" || id.Class() != "PRINTER" {
		t.Fatalf("%+v", id)
	}
	if !slices.Equal(id.CommandSet(), []string{"PT-CBP"}) {
		t.Fatalf("cmd %v", id.CommandSet())
	}
	long := Parse("MANUFACTURER:Hewlett-Packard;COMMAND SET:PJL, MLC,PCLXL ,PCL;MODEL:HP LaserJet P2055;garbage;:x;MANUFACTURER:ignored;")
	if long.Manufacturer() != "Hewlett-Packard" || long.Model() != "HP LaserJet P2055" {
		t.Fatalf("%+v", long)
	}
	if !slices.Equal(long.CommandSet(), []string{"PJL", "MLC", "PCLXL", "PCL"}) {
		t.Fatalf("cmd %v", long.CommandSet())
	}
	if !long.HasCommand(func(s string) bool { return strings.HasPrefix(s, "PCL") }) {
		t.Fatal("PCL not found")
	}
	if Parse("").Empty() != true || Parse("nonsense").Empty() != true {
		t.Fatal("empty")
	}
	if got := Parse("cmd:ZPL;mdl:ZD420;mfg:Zebra Technologies;").String(); got != "MFG:Zebra Technologies;MDL:ZD420;CMD:ZPL;" {
		t.Fatalf("String %q", got)
	}
}

func TestMatch(t *testing.T) {
	id := Parse("MFG:Brother;MDL:QL-700;")
	for pat, want := range map[string]bool{
		"": true, "brother": true, "ql-7": true, "Brother:QL": true, ":QL-700": true, "brother:": true,
		"dymo": false, "Brother:QL-800": false, "Zebra:QL": false,
	} {
		if got := id.Match(pat); got != want {
			t.Errorf("Match(%q) = %v", pat, got)
		}
	}
}
