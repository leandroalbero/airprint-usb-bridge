package ipp

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"
)

// JobState is the IPP job-state enum.
type JobState int

const (
	JobPending           JobState = 3
	JobPendingHeld       JobState = 4
	JobProcessing        JobState = 5
	JobProcessingStopped JobState = 6
	JobCanceled          JobState = 7
	JobAborted           JobState = 8
	JobCompleted         JobState = 9
)

func (s JobState) String() string {
	switch s {
	case JobPending:
		return "pending"
	case JobPendingHeld:
		return "pending-held"
	case JobProcessing:
		return "processing"
	case JobProcessingStopped:
		return "processing-stopped"
	case JobCanceled:
		return "canceled"
	case JobAborted:
		return "aborted"
	case JobCompleted:
		return "completed"
	}
	return "unknown"
}

// Terminal reports whether the job has finished one way or another.
func (s JobState) Terminal() bool { return s >= JobCanceled }

// Processor prints one job's document. It runs on the single queue worker; ctx
// is cancelled by Cancel-Job and on shutdown. A nil error completes the job,
// an error aborts it (wrap ErrDocumentFormat for undecodable input).
type Processor func(ctx context.Context, job *Job, doc io.Reader, format string) error

// ErrDocumentFormat marks a document the processor could not decode; the job
// gets job-state-reasons document-format-error.
var ErrDocumentFormat = errors.New("document format error")

// MediaCol is an accepted media-col. Dimensions are hundredths of a millimetre.
type MediaCol struct {
	XDimension int
	YDimension int // 0 when the client sent a range or no media-size
	SizeName   string
	Source     string
	Type       string
	Top        int
	Bottom     int
	Left       int
	Right      int
}

// JobAttrs are the job template values that were accepted. Unsupported values
// that were ignored are left at their defaults.
type JobAttrs struct {
	Copies       int
	PrintQuality int    // 3 draft, 4 normal, 5 high
	XDPI, YDPI   int    // printer-resolution, 0 when absent
	Media        string // media keyword, "" when absent
	MediaCol     *MediaCol
	Orientation  int // 3 portrait, 4 landscape, 0 when absent
	ColorMode    string
	Scaling      string
}

type Job struct {
	ID    int
	Name  string
	User  string
	Attrs JobAttrs

	uuid    string
	created time.Time
	upAt    struct{ created, processing, completed int }

	mu          sync.Mutex
	state       JobState
	reasons     []string
	message     string
	impressions int
	processing  time.Time
	completed   time.Time
	format      string // document-format as supplied
	docFormat   string // format handed to the processor
	docPath     string
	docSize     int64
	hasDoc      bool
	closed      bool // no more documents will come
	receiving   bool // a Send-Document body is being read
	cancelReq   bool
	cancel      context.CancelFunc
	timeout     *time.Timer
}

// SetImpressions sets job-impressions-completed.
func (j *Job) SetImpressions(n int) {
	j.mu.Lock()
	j.impressions = n
	j.mu.Unlock()
}

// SetStateMessage sets job-state-message.
func (j *Job) SetStateMessage(s string) {
	j.mu.Lock()
	j.message = s
	j.mu.Unlock()
}

// State returns the current job-state.
func (j *Job) State() JobState {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.state
}

// JobInfo is a snapshot for status pages and logs.
type JobInfo struct {
	ID          int
	Name        string
	User        string
	State       JobState
	Reasons     []string
	Message     string
	Impressions int
	Created     time.Time
	Completed   time.Time
	Size        int64
}

func (j *Job) info() JobInfo {
	j.mu.Lock()
	defer j.mu.Unlock()
	return JobInfo{
		ID: j.ID, Name: j.Name, User: j.User, State: j.state,
		Reasons: append([]string(nil), j.reasons...), Message: j.message,
		Impressions: j.impressions, Created: j.created, Completed: j.completed, Size: j.docSize,
	}
}

func (j *Job) setState(st JobState, msg string, reasons ...string) {
	j.state = st
	j.message = msg
	j.reasons = reasons
}
