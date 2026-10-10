package brotherql

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/leandroalbero/airprint-usb-bridge/internal/backend"
	"github.com/leandroalbero/airprint-usb-bridge/internal/ql"
)

func profile(t *testing.T) *ql.Profile {
	t.Helper()
	p, err := ql.EmbeddedProfile()
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func media(t *testing.T, p *ql.Profile, id string) ql.Media {
	t.Helper()
	m, ok := p.MediaByID(id)
	if !ok {
		t.Fatalf("no media %s", id)
	}
	return m
}

func label(w, h int) *ql.Bitmap {
	b := ql.NewBitmap(w, h)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			b.Set(x, y, (x/7+y/5)%3 == 0)
		}
	}
	return b
}

func label62(t *testing.T) (*ql.Profile, ql.Job) {
	t.Helper()
	p := profile(t)
	return p, JobFor(p, media(t, p, "62"), label(696, 151), false)
}

func TestJobForDecode(t *testing.T) {
	p, job := label62(t)
	bin, err := ql.NewEncoder().Encode(p, job)
	if err != nil {
		t.Fatal(err)
	}
	text, err := ql.Decode(bin)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Split(text, "\n")
	want := []string{
		"invalidate 200",
		"initialize",
		"print-info flags=0x8e media=0x0a width=62 length=0 lines=151",
		"various-mode auto-cut=1",
		"cut-every 1",
		"expanded-mode flags=0x08 cut-at-end=1 high-res=0",
		"margin 0",
		"raster-lines 151",
		"print-and-feed",
	}
	if len(got) != len(want) {
		t.Fatalf("decode:\n%s", text)
	}
	for i := range want {
		if got[i] != want[i] && !(strings.HasPrefix(want[i], "print-info") && strings.HasPrefix(got[i], want[i]+" ")) {
			t.Fatalf("line %d = %q, want %q\n%s", i, got[i], want[i], text)
		}
	}
}

func TestJobForOptions(t *testing.T) {
	p := profile(t)
	m := media(t, p, "62")
	for _, hr := range []bool{false, true} {
		job := JobFor(p, m, label(696, 10), hr)
		want := ql.DefaultJobOptions()
		want.MarginDots, want.AutoCut, want.CutEvery, want.CutAtEnd, want.HighRes, want.ValidateMedia = 0, true, 1, true, hr, true
		want.LastTerminator = ql.TermPrintAndFeed
		if job.Options != want {
			t.Fatalf("highRes=%v options %+v, want %+v", hr, job.Options, want)
		}
		if job.Media != m {
			t.Fatalf("media %+v", job.Media)
		}
	}
	bin, err := ql.NewEncoder().Encode(p, JobFor(p, m, label(696, 10), true))
	if err != nil {
		t.Fatal(err)
	}
	text, _ := ql.Decode(bin)
	if !strings.Contains(text, "expanded-mode flags=0x48 cut-at-end=1 high-res=1") {
		t.Fatalf("high-res decode:\n%s", text)
	}
}

func TestJobForPadsRight(t *testing.T) {
	p := profile(t)
	m := media(t, p, "62")
	src := label(600, 20)
	job := JobFor(p, m, src, false)
	if len(job.Pages) != 1 {
		t.Fatalf("pages %d", len(job.Pages))
	}
	b := job.Pages[0]
	if b.W != m.PrintPins || b.H != 20 {
		t.Fatalf("size %dx%d", b.W, b.H)
	}
	for y := 0; y < b.H; y++ {
		for x := 0; x < b.W; x++ {
			if b.At(x, y) != src.At(x, y) {
				t.Fatalf("dot %d,%d differs", x, y)
			}
		}
	}
	exact := label(m.PrintPins, 5)
	if JobFor(p, m, exact, false).Pages[0] != exact {
		t.Fatal("full-width bitmap copied")
	}
}

func TestJobForDieCutPadsHeight(t *testing.T) {
	p := profile(t)
	var m ql.Media
	for _, x := range p.Media {
		if x.PrintLength > 0 {
			m = x
			break
		}
	}
	if m.PrintLength == 0 {
		t.Skip("profile has no die-cut media")
	}
	b := JobFor(p, m, label(m.PrintPins-10, m.PrintLength-7), false).Pages[0]
	if b.W != m.PrintPins || b.H != m.PrintLength {
		t.Fatalf("size %dx%d, want %dx%d", b.W, b.H, m.PrintPins, m.PrintLength)
	}
}

func TestJobForTooWideFailsEncode(t *testing.T) {
	p := profile(t)
	m := media(t, p, "62")
	job := JobFor(p, m, label(m.PrintPins+1, 5), false)
	if _, err := ql.NewEncoder().Encode(p, job); !errors.Is(err, ql.ErrBitmapWidth) {
		t.Fatalf("err %v, want ErrBitmapWidth", err)
	}
	if _, err := ql.NewEncoder().Encode(p, JobFor(p, m, nil, false)); err == nil {
		t.Fatal("nil bitmap encoded")
	}
}

func TestReasonsForStatus(t *testing.T) {
	cases := []struct {
		errs, unexpected []string
		typ              ql.StatusType
		want             []string
	}{
		{[]string{"no media"}, nil, ql.StatusError, []string{"media-empty", "media-needed"}},
		{[]string{"end of media"}, nil, ql.StatusError, []string{"media-empty", "media-needed"}},
		{[]string{"media mismatch"}, nil, ql.StatusError, []string{"media-needed"}},
		{[]string{"cutter jam"}, nil, ql.StatusError, []string{"media-jam"}},
		{[]string{"cannot feed"}, nil, ql.StatusError, []string{"media-jam"}},
		{[]string{"cover open"}, nil, ql.StatusError, []string{"cover-open"}},
		{[]string{"cover open", "no media"}, nil, ql.StatusError, []string{"cover-open", "media-empty", "media-needed"}},
		{[]string{"fan"}, nil, ql.StatusError, []string{"other"}},
		{[]string{"system error", "transmission"}, nil, ql.StatusError, []string{"other"}},
		{nil, []string{"unexpected bit err1:0x08"}, ql.StatusError, []string{"other"}},
		{nil, nil, ql.StatusError, []string{"other"}},
		{nil, nil, ql.StatusReply, nil},
	}
	for _, c := range cases {
		got := ReasonsForStatus(ql.Status{Type: c.typ, Errors: c.errs, Unexpected: c.unexpected})
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%v %v: %v, want %v", c.errs, c.unexpected, got, c.want)
		}
	}
}

func TestReasonsForError(t *testing.T) {
	if got := ReasonsForError(fmt.Errorf("print: %w", errors.Join(ql.ErrCoverOpen, ql.ErrNoMedia))); !reflect.DeepEqual(got, []string{"media-empty", "media-needed", "cover-open"}) {
		t.Fatalf("got %v", got)
	}
	if got := ReasonsForError(retryable(fmt.Errorf("open: %w", ql.ErrNoPrinter))); !reflect.DeepEqual(got, []string{"offline-report"}) {
		t.Fatalf("got %v", got)
	}
	if got := ReasonsForError(errors.New("encode")); got != nil {
		t.Fatalf("got %v", got)
	}
}

func TestStateFromStatus(t *testing.T) {
	if s := StateFromStatus(ql.Status{Type: ql.StatusReply, MediaWidthMM: 62, MediaKind: ql.MediaContinuous}); s.Condition != backend.Idle || len(s.Details) != 1 || s.Details[0].Value != "62 mm (kind 0x0a)" {
		t.Fatalf("idle: %+v", s)
	}
	if s := StateFromStatus(ql.Status{Type: ql.StatusPhase, Printing: true}); s.Condition != backend.Busy {
		t.Fatalf("printing: %+v", s)
	}
	s := StateFromStatus(ql.Status{Type: ql.StatusError, Errors: []string{"cover open"}})
	if s.Condition != backend.Error || !reflect.DeepEqual(s.Reasons, []string{"cover-open"}) || s.Message != "cover open" {
		t.Fatalf("error: %+v", s)
	}
}
