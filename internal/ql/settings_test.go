package ql

import (
	"bytes"
	"context"
	"testing"
)

func settingsReport(v byte) Status {
	st := Status{Type: StatusSettings}
	st.Raw[30] = v
	return st
}

func TestPrinter_getSetting(t *testing.T) {
	p := testProfile(t)
	f := &fakeDevice{reads: []readStep{{st: settingsReport(0)}}}
	pr := NewPrinter(p, func() (Device, error) { return f, nil }, shortTimeouts())
	got, err := pr.GetSetting(context.Background(), 0x41)
	if err != nil || got != 0 {
		t.Fatalf("got %d err %v", got, err)
	}
	writes, _ := f.snapshot()
	if len(writes) != 1 || !bytes.Equal(writes[0], []byte{0x1b, 0x69, 0x55, 0x41, 0x01}) {
		t.Fatalf("writes %x", writes)
	}
}

func TestPrinter_setSettingReadback(t *testing.T) {
	p := testProfile(t)
	f := &fakeDevice{reads: []readStep{{st: Status{Type: StatusReply}}, {st: settingsReport(1)}}}
	pr := NewPrinter(p, func() (Device, error) { return f, nil }, shortTimeouts())
	if err := pr.SetSetting(context.Background(), 0x70, 1); err != nil {
		t.Fatal(err)
	}
	writes, _ := f.snapshot()
	want := [][]byte{
		{0x1b, 0x69, 0x55, 0x70, 0x00, 0x01},
		{0x1b, 0x69, 0x53},
		{0x1b, 0x69, 0x55, 0x70, 0x01},
	}
	if len(writes) != len(want) {
		t.Fatalf("writes %x", writes)
	}
	for i := range want {
		if !bytes.Equal(writes[i], want[i]) {
			t.Fatalf("write %d %x", i, writes[i])
		}
	}
}

func TestPrinter_setSettingRejectsErrorAndMismatch(t *testing.T) {
	p := testProfile(t)
	bad := &fakeDevice{reads: []readStep{{st: Status{Type: StatusError, Errors: []string{"cover open"}}}}}
	pr := NewPrinter(p, func() (Device, error) { return bad, nil }, shortTimeouts())
	if err := pr.SetSetting(context.Background(), 0x41, 0); err == nil {
		t.Fatal("expected status error")
	}
	writes, _ := bad.snapshot()
	if len(writes) != 2 {
		t.Fatalf("stopped after status, writes %d", len(writes))
	}

	mismatch := &fakeDevice{reads: []readStep{{st: Status{Type: StatusReply}}, {st: settingsReport(6)}}}
	pr = NewPrinter(p, func() (Device, error) { return mismatch, nil }, shortTimeouts())
	if err := pr.SetSetting(context.Background(), 0x41, 0); err == nil {
		t.Fatal("expected readback mismatch")
	}
}

func TestLoadProfile_settingsKeys(t *testing.T) {
	p := testProfile(t)
	off, ok := p.SettingByID("auto-power-off")
	if !ok || off.Key != 0x41 {
		t.Fatalf("%+v", off)
	}
	v, ok := off.ValueOf("never")
	if !ok || v != 0 {
		t.Fatalf("never %d %v", v, ok)
	}
	v, ok = off.ValueOf("60m")
	if !ok || v != 6 {
		t.Fatalf("60m %d", v)
	}
	on, ok := p.SettingByID("auto-power-on")
	if !ok || on.Key != 0x70 {
		t.Fatalf("%+v", on)
	}
	if tok, ok := on.TokenOf(1); !ok || tok != "on" {
		t.Fatalf("token %q", tok)
	}
}
