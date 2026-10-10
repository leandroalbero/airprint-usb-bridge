package backend

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/leandroalbero/ql-airprint/internal/ql"
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

func TestStateIPP(t *testing.T) {
	cases := []struct {
		s       State
		state   int
		reasons []string
	}{
		{State{Condition: Idle}, 3, []string{"none"}},
		{State{Condition: Unknown}, 3, []string{"none"}},
		{State{Condition: Busy}, 4, []string{"none"}},
		{State{Condition: Offline, Reasons: []string{"offline-report"}}, 5, []string{"offline-report"}},
		{State{Condition: Error, Reasons: []string{"media-empty", "media-needed"}}, 5, []string{"media-empty-error", "media-needed-error"}},
		{State{Condition: Busy, Reasons: []string{"other"}}, 4, []string{"other-report"}},
	}
	for _, c := range cases {
		if c.s.IPPState() != c.state || !reflect.DeepEqual(c.s.IPPReasons(), c.reasons) {
			t.Errorf("%v: %d %v, want %d %v", c.s.Condition, c.s.IPPState(), c.s.IPPReasons(), c.state, c.reasons)
		}
	}
}

func TestStateFromStatus(t *testing.T) {
	if s := StateFromStatus(ql.Status{Type: ql.StatusReply, MediaWidthMM: 62}); s.Condition != Idle || s.Status == nil || s.Status.MediaWidthMM != 62 {
		t.Fatalf("idle: %+v", s)
	}
	if s := StateFromStatus(ql.Status{Type: ql.StatusPhase, Printing: true}); s.Condition != Busy {
		t.Fatalf("printing: %+v", s)
	}
	s := StateFromStatus(ql.Status{Type: ql.StatusError, Errors: []string{"cover open"}})
	if s.Condition != Error || !reflect.DeepEqual(s.Reasons, []string{"cover-open"}) || s.Message != "cover open" {
		t.Fatalf("error: %+v", s)
	}
}

type scripted struct {
	mu    sync.Mutex
	errs  []error
	calls int
}

func (s *scripted) Print(context.Context, ql.Job) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if len(s.errs) == 0 {
		return nil
	}
	err := s.errs[0]
	if len(s.errs) > 1 {
		s.errs = s.errs[1:]
	}
	return err
}
func (s *scripted) State(context.Context) (State, error) { return State{}, nil }
func (s *scripted) Close() error                         { return nil }

func busyErr() error { return retryable(fmt.Errorf("open: %w", ql.ErrDeviceBusy)) }

func TestPrintWithRetrySucceedsAfterRetryable(t *testing.T) {
	b := &scripted{errs: []error{busyErr(), busyErr(), nil}}
	var logs []string
	err := printWithRetry(context.Background(), b, ql.Job{}, time.Second, func(s string) { logs = append(logs, s) }, time.Millisecond, 4*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if b.calls != 3 || len(logs) != 2 {
		t.Fatalf("calls %d logs %v", b.calls, logs)
	}
	if !strings.Contains(logs[0], "device busy") || !strings.Contains(logs[0], "retrying in 1ms") || !strings.Contains(logs[1], "retrying in 2ms") {
		t.Fatalf("logs %v", logs)
	}
}

func TestPrintWithRetryNeverRetriesFinal(t *testing.T) {
	final := fmt.Errorf("print: %w", ql.ErrDeviceGone)
	b := &scripted{errs: []error{final}}
	err := printWithRetry(context.Background(), b, ql.Job{}, time.Second, nil, time.Millisecond, time.Millisecond)
	if !errors.Is(err, ql.ErrDeviceGone) || b.calls != 1 {
		t.Fatalf("err %v calls %d", err, b.calls)
	}
	// Busy without ErrRetryable means bytes went out.
	b = &scripted{errs: []error{fmt.Errorf("print: %w", ql.ErrDeviceBusy)}}
	if err := printWithRetry(context.Background(), b, ql.Job{}, time.Second, nil, time.Millisecond, time.Millisecond); err == nil || b.calls != 1 {
		t.Fatalf("err %v calls %d", err, b.calls)
	}
}

func TestPrintWithRetryDeadline(t *testing.T) {
	b := &scripted{errs: []error{busyErr()}}
	start := time.Now()
	err := printWithRetry(context.Background(), b, ql.Job{}, 40*time.Millisecond, nil, 5*time.Millisecond, 10*time.Millisecond)
	el := time.Since(start)
	if !errors.Is(err, ErrRetryable) || !errors.Is(err, ql.ErrDeviceBusy) || !strings.Contains(err.Error(), "gave up") {
		t.Fatalf("err %v", err)
	}
	if el < 40*time.Millisecond || el > time.Second {
		t.Fatalf("elapsed %s", el)
	}
	if b.calls < 3 {
		t.Fatalf("calls %d", b.calls)
	}
}

func TestPrintWithRetryBackoffCaps(t *testing.T) {
	b := &scripted{errs: []error{busyErr(), busyErr(), busyErr(), busyErr(), busyErr(), nil}}
	var logs []string
	if err := printWithRetry(context.Background(), b, ql.Job{}, time.Second, func(s string) { logs = append(logs, s) }, time.Millisecond, 4*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	want := []string{"1ms", "2ms", "4ms", "4ms", "4ms"}
	for i, w := range want {
		if !strings.HasSuffix(logs[i], "retrying in "+w) {
			t.Fatalf("log %d %q, want %s", i, logs[i], w)
		}
	}
}

func TestPrintWithRetryContext(t *testing.T) {
	b := &scripted{errs: []error{busyErr()}}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := printWithRetry(ctx, b, ql.Job{}, time.Minute, nil, time.Hour, time.Hour)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err %v", err)
	}
	if b.calls != 1 {
		t.Fatalf("calls %d", b.calls)
	}
}

func TestPrintWithRetryDefaults(t *testing.T) {
	b := &scripted{}
	if err := PrintWithRetry(context.Background(), b, ql.Job{}, 0, nil); err != nil || b.calls != 1 {
		t.Fatalf("err %v calls %d", err, b.calls)
	}
}
