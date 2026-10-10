package ql

import (
	"context"
	"fmt"
	"sync"
	"time"
)

func LoadProfile(path string) (*Profile, error) { return loadProfile(path) }

func (p *Profile) MediaByID(id string) (Media, bool) {
	if p == nil {
		return Media{}, false
	}
	for _, m := range p.Media {
		if m.ID == id {
			return m, true
		}
	}
	return Media{}, false
}

// DetectMedia maps a status' (kind, width, length) to a profile media entry.
func (p *Profile) DetectMedia(s Status) (Media, bool) { return detectMedia(p, s) }

func NewEncoder() Encoder { return &RasterEncoder{} }

// ParseStatus decodes a 32-byte status using the profile's valid error bits.
func ParseStatus(p *Profile, raw []byte) (Status, error) { return parseStatus(p, raw) }

// Invalidate is the byte run that abandons a half-sent job: the profile's NUL count, then ESC @.
func Invalidate(p *Profile) []byte { return invalidate(p) }

// Decode renders a byte stream as human-readable commands for dry runs.
func Decode(b []byte) (string, error) { return decode(b) }

// OpenDevice opens a usblp node read/write, non-blocking, with an exclusive flock.
// path "auto" tries /dev/usb/lp0..lp3. ENODEV and ENXIO mean that node has no printer.
func OpenDevice(p *Profile, path string) (Device, error) { return openDevice(p, path) }

type Timeouts struct {
	FirstStatusReply time.Duration // first ESC i S after open; the QL-700 often drops it (600 ms)
	StatusReply      time.Duration
	PhasePrinting    time.Duration
	CompletedBase    time.Duration
	CompletedPerMM   time.Duration
	Cooling          time.Duration
}

func DefaultTimeouts() Timeouts {
	return Timeouts{
		FirstStatusReply: 600 * time.Millisecond,
		StatusReply:      2 * time.Second,
		PhasePrinting:    10 * time.Second,
		CompletedBase:    10 * time.Second,
		CompletedPerMM:   20 * time.Millisecond,
		Cooling:          10 * time.Minute,
	}
}

func normalizeTimeouts(t Timeouts) Timeouts {
	d := DefaultTimeouts()
	if t.FirstStatusReply == 0 {
		t.FirstStatusReply = d.FirstStatusReply
	}
	if t.StatusReply == 0 {
		t.StatusReply = d.StatusReply
	}
	if t.PhasePrinting == 0 {
		t.PhasePrinting = d.PhasePrinting
	}
	if t.CompletedBase == 0 {
		t.CompletedBase = d.CompletedBase
	}
	if t.CompletedPerMM == 0 {
		t.CompletedPerMM = d.CompletedPerMM
	}
	if t.Cooling == 0 {
		t.Cooling = d.Cooling
	}
	return t
}

type JobState string

const (
	JobQueued    JobState = "queued"
	JobSending   JobState = "sending"
	JobPrinting  JobState = "printing"
	JobDone      JobState = "done"
	JobFailed    JobState = "failed"
	JobCancelled JobState = "cancelled"
	JobUnknown   JobState = "unknown"
)

type JobResult struct {
	State          JobState
	PagesConfirmed int
	Statuses       []Status
	Err            error
	PageCount      int
	Warnings       []string
	Cooling        bool
	Stopped        bool // ended by Observer.StopBefore
	PagesWritten   int  // pages whose bytes were fully written
	InFlightBytes  bool // some bytes of page PagesConfirmed were written (maybe on paper, §3.5 step 5)
	LastByteAt     time.Time
}

// Printer is the single owner of the device; jobs run one at a time.
type Printer struct {
	profile  *Profile
	open     func() (Device, error)
	timeouts Timeouts

	mu     sync.Mutex
	cond   *sync.Cond
	queue  []queuedJob
	worker bool
	busy   bool
	uncut  bool
	stall  time.Duration
}

func NewPrinter(p *Profile, open func() (Device, error), t Timeouts) *Printer {
	pr := &Printer{
		profile:  p,
		open:     open,
		timeouts: normalizeTimeouts(t),
		stall:    500 * time.Millisecond,
	}
	pr.cond = sync.NewCond(&pr.mu)
	return pr
}

// Status sends ESC i S while idle and returns the parsed reply.
func (pr *Printer) Status(ctx context.Context) (Status, error) {
	if pr == nil {
		return Status{}, fmt.Errorf("status: nil printer")
	}
	return pr.status(ctx)
}

// Print encodes and sends a job page by page, waiting for completion between pages.
func (pr *Printer) Print(ctx context.Context, j Job) JobResult {
	if pr == nil {
		return JobResult{State: JobFailed, Err: fmt.Errorf("print: nil printer")}
	}
	return pr.print(ctx, j)
}

// SendRaw writes arbitrary bytes and captures every status received for the capture window.
func (pr *Printer) SendRaw(ctx context.Context, b []byte, capture time.Duration) ([]Status, error) {
	if pr == nil {
		return nil, fmt.Errorf("send raw: nil printer")
	}
	return pr.sendRaw(ctx, b, capture)
}

// UncutTailPending reports chain-printing state across jobs.
func (pr *Printer) UncutTailPending() bool {
	if pr == nil {
		return false
	}
	pr.mu.Lock()
	defer pr.mu.Unlock()
	return pr.uncut
}
