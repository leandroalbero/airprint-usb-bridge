package ql

import (
	"errors"
	"strings"
	"testing"
)

func rawStatus(mut func([]byte)) []byte {
	b := make([]byte, 32)
	b[0], b[1], b[2] = 0x80, 0x20, 0x42
	b[3], b[4] = 0x34, 0x35
	b[10] = 62
	b[11] = byte(MediaContinuous)
	mut(b)
	return b
}

func TestParseStatus_fixtures(t *testing.T) {
	p := testProfile(t)
	tests := []struct {
		name    string
		raw     []byte
		typ     StatusType
		errs    []string
		unexp   bool
		width   int
		kind    MediaKind
		print   bool
		phase   uint16
		notify  byte
		wantErr error
	}{
		{
			name:  "reply idle 62mm continuous",
			raw:   rawStatus(func(b []byte) { b[18] = byte(StatusReply) }),
			typ:   StatusReply,
			width: 62,
			kind:  MediaContinuous,
		},
		{
			name:  "phase change printing",
			raw:   rawStatus(func(b []byte) { b[18] = byte(StatusPhase); b[19] = PhasePrinting; b[20] = 0x34; b[21] = 0x12 }),
			typ:   StatusPhase,
			width: 62,
			kind:  MediaContinuous,
			print: true,
			phase: 0x1234,
		},
		{
			name:  "printing completed",
			raw:   rawStatus(func(b []byte) { b[18] = byte(StatusCompleted) }),
			typ:   StatusCompleted,
			width: 62,
			kind:  MediaContinuous,
		},
		{
			name:  "error no media",
			raw:   rawStatus(func(b []byte) { b[8] = 0x01; b[18] = byte(StatusError) }),
			typ:   StatusError,
			errs:  []string{"no media"},
			width: 62,
			kind:  MediaContinuous,
		},
		{
			name:  "cover open",
			raw:   rawStatus(func(b []byte) { b[9] = 0x10; b[18] = byte(StatusError) }),
			typ:   StatusError,
			errs:  []string{"cover open"},
			width: 62,
			kind:  MediaContinuous,
		},
		{
			name:  "settings report",
			raw:   rawStatus(func(b []byte) { b[18] = byte(StatusSettings); b[30] = 0 }),
			typ:   StatusSettings,
			width: 62,
			kind:  MediaContinuous,
		},
		{
			name:  "unknown bit",
			raw:   rawStatus(func(b []byte) { b[8] = 0x08; b[18] = byte(StatusReply) }),
			typ:   StatusReply,
			unexp: true,
			width: 62,
			kind:  MediaContinuous,
		},
		{
			name:  "model-invalid bit kept",
			raw:   rawStatus(func(b []byte) { b[8] = 0x10; b[18] = byte(StatusError) }),
			typ:   StatusError,
			unexp: true,
			width: 62,
			kind:  MediaContinuous,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st, err := ParseStatus(p, tt.raw)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatal(err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if st.Type != tt.typ || st.MediaWidthMM != tt.width || st.MediaKind != tt.kind || st.Printing != tt.print {
				t.Fatalf("%+v", st)
			}
			if st.PhaseNumber != tt.phase || st.Notification != tt.notify {
				t.Fatalf("phase %d notify %02x", st.PhaseNumber, st.Notification)
			}
			if strings.Join(st.Errors, ",") != strings.Join(tt.errs, ",") {
				t.Fatalf("errors %v", st.Errors)
			}
			if tt.unexp && len(st.Unexpected) == 0 {
				t.Fatal("missing unexpected")
			}
			if tt.unexp && !strings.Contains(st.Unexpected[0], "unexpected bit") {
				t.Fatal(st.Unexpected)
			}
			if tt.name == "model-invalid bit kept" && len(st.Errors) != 0 {
				t.Fatalf("named %v unexpected %v", st.Errors, st.Unexpected)
			}
			if tt.name == "settings report" && (st.Type.String() != "settings report" || st.Raw[30] != 0) {
				t.Fatalf("%s byte30=%d", st.Type, st.Raw[30])
			}
			if tt.name == "unknown bit" && !strings.Contains(st.Unexpected[0], "err1:0x08") {
				t.Fatal(st.Unexpected)
			}
			if tt.name == "model-invalid bit kept" && !strings.Contains(st.Unexpected[0], "main unit in use") {
				t.Fatal(st.Unexpected)
			}
		})
	}
}

func TestParseStatus_framingAndLength(t *testing.T) {
	p := testProfile(t)
	_, err := ParseStatus(p, []byte{0x00, 0x20, 0x42})
	if !errors.Is(err, ErrStatusLength) {
		t.Fatal(err)
	}
	raw := rawStatus(func(b []byte) { b[0] = 0x00 })
	_, err = ParseStatus(p, raw)
	if !errors.Is(err, ErrFraming) {
		t.Fatal(err)
	}
}

func TestParseStatus_mediaMismatchNamed(t *testing.T) {
	p := testProfile(t)
	st, err := ParseStatus(p, rawStatus(func(b []byte) { b[9] = 0x01; b[18] = byte(StatusError) }))
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Errors) != 1 || st.Errors[0] != "media mismatch" {
		t.Fatal(st.Errors)
	}
	if err := statusError(st); !errors.Is(err, ErrMediaMismatch) {
		t.Fatal(err)
	}
}
