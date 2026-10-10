package ipp

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	printerPath        = "/ipp/print"
	maxRetainedJobs    = 50
	statusPollInterval = 2 * time.Second
	statusPollTimeout  = 5 * time.Second
	httpDrainTimeout   = 10 * time.Second
)

// multipleOperationTimeout is how long a Create-Job job waits for its document;
// a variable so tests can shorten it.
var multipleOperationTimeout = 60 * time.Second

type Server struct {
	cfg      Config
	proc     Processor
	attrs    []printerAttr
	tlsCfg   *tls.Config
	httpSrv  *http.Server
	start    time.Time
	spoolDir string
	port     atomic.Int32

	ctx     context.Context
	stopAll context.CancelFunc
	wg      sync.WaitGroup

	// pollMu keeps a device status query and the start of a job from overlapping.
	pollMu sync.Mutex

	mu           sync.Mutex
	jobs         []*Job // ascending ID
	queue        []*Job
	nextID       int
	current      *Job
	dev          DeviceStatus
	lastState    PrinterState
	stateChanged time.Time
	stateUp      int
	wake         chan struct{}
	pollNow      chan struct{}
	reserved     int // job slots held by requests still reading their document

	spoolUsed atomic.Int64
}

// New builds a server and starts its job worker. Call Shutdown to stop it.
func New(cfg Config, proc Processor) (*Server, error) {
	if proc == nil {
		return nil, errors.New("ipp: nil Processor")
	}
	if err := cfg.normalize(); err != nil {
		return nil, err
	}
	cert, err := LoadOrCreateCert(cfg.StateDir)
	if err != nil {
		return nil, fmt.Errorf("ipp: tls certificate: %w", err)
	}
	spool, err := prepareSpool(cfg.StateDir)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	s := &Server{
		cfg:  cfg,
		proc: proc,
		tlsCfg: &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
			NextProtos:   []string{"http/1.1"},
		},
		start:        now,
		spoolDir:     spool,
		nextID:       1,
		lastState:    StateIdle,
		stateChanged: now,
		wake:         make(chan struct{}, 1),
		pollNow:      make(chan struct{}, 1),
	}
	s.port.Store(int32(cfg.Port))
	s.attrs = buildPrinterAttrs(&s.cfg, int(cfg.MaxDocumentBytes/1024))
	s.ctx, s.stopAll = context.WithCancel(context.Background())
	s.httpSrv = &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       5 * time.Minute,
		TLSConfig:         s.tlsCfg,
		ErrorLog:          nil,
	}
	s.wg.Add(1)
	go s.worker()
	if cfg.Status != nil {
		s.wg.Add(1)
		go s.pollStatus()
	}
	return s, nil
}

func prepareSpool(stateDir string) (string, error) {
	if stateDir == "" {
		return os.MkdirTemp("", "ql-airprint-spool-")
	}
	dir := filepath.Join(stateDir, "spool")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("ipp: spool dir: %w", err)
	}
	// Jobs are in-memory only, so anything left over is from a previous run.
	if ents, err := os.ReadDir(dir); err == nil {
		for _, e := range ents {
			if strings.HasPrefix(e.Name(), "job-") {
				os.Remove(filepath.Join(dir, e.Name()))
			}
		}
	}
	return dir, nil
}

// TLSConfig returns the configuration used for IPPS connections.
func (s *Server) TLSConfig() *tls.Config { return s.tlsCfg }

// Serve accepts plain HTTP and TLS connections on l until Shutdown.
func (s *Server) Serve(l net.Listener) error {
	if s.port.Load() == 0 {
		if ta, ok := l.Addr().(*net.TCPAddr); ok {
			s.port.CompareAndSwap(0, int32(ta.Port))
		}
	}
	return s.httpSrv.Serve(newMuxListener(l, s.tlsCfg))
}

// ListenAndServe listens on addr (e.g. ":8631") and calls Serve.
func (s *Server) ListenAndServe(addr string) error {
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	return s.Serve(l)
}

// Shutdown cancels the running job, stops accepting requests and waits until ctx
// is done for the worker. A cancelled job still finishes the label it is
// printing, so ctx should allow for one whole label. Requests in progress get
// at most httpDrainTimeout before their connections are closed.
func (s *Server) Shutdown(ctx context.Context) error {
	s.stopAll()
	hctx, cancel := context.WithTimeout(ctx, httpDrainTimeout)
	err := s.httpSrv.Shutdown(hctx)
	cancel()
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		err = s.httpSrv.Close()
	}
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		return ctx.Err()
	}
	s.mu.Lock()
	for _, j := range s.jobs {
		j.mu.Lock()
		if j.timeout != nil {
			j.timeout.Stop()
		}
		s.dropJobDocLocked(j)
		j.mu.Unlock()
	}
	s.mu.Unlock()
	if s.cfg.StateDir == "" {
		os.RemoveAll(s.spoolDir)
	}
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	return err
}

// Jobs returns a snapshot of the retained jobs, newest first.
func (s *Server) Jobs() []JobInfo {
	s.mu.Lock()
	jobs := append([]*Job(nil), s.jobs...)
	s.mu.Unlock()
	out := make([]JobInfo, 0, len(jobs))
	for i := len(jobs) - 1; i >= 0; i-- {
		out = append(out, jobs[i].info())
	}
	return out
}

func (s *Server) logf(format string, args ...any) {
	if s.cfg.Logf != nil {
		s.cfg.Logf(format, args...)
	}
}

func (s *Server) upTime(t time.Time) int { return int(t.Sub(s.start)/time.Second) + 1 }

func (s *Server) pollStatus() {
	defer s.wg.Done()
	t := time.NewTicker(statusPollInterval)
	defer t.Stop()
	for {
		s.pollMu.Lock()
		s.mu.Lock()
		busy := s.current != nil
		s.mu.Unlock()
		// A status query may open the device, which would contend with the
		// job's own per-label open; the cached status is kept meanwhile.
		if !busy {
			ctx, cancel := context.WithTimeout(s.ctx, statusPollTimeout)
			st := s.cfg.Status(ctx)
			cancel()
			s.mu.Lock()
			s.dev = st
			s.updateStateLocked()
			s.mu.Unlock()
		}
		s.pollMu.Unlock()
		select {
		case <-s.ctx.Done():
			return
		case <-t.C:
		case <-s.pollNow:
		}
	}
}

func (s *Server) kickStatus() {
	select {
	case s.pollNow <- struct{}{}:
	default:
	}
}

// statusLocked derives printer-state from the device status and the queue.
func (s *Server) statusLocked() printerStatus {
	s.updateStateLocked()
	st := printerStatus{
		state:      s.lastState,
		reasons:    s.dev.Reasons,
		message:    s.dev.Message,
		changed:    s.stateChanged,
		changedUp:  s.stateUp,
		upTime:     s.upTime(time.Now()),
		configTime: s.start,
	}
	if len(st.reasons) == 0 {
		st.reasons = []string{"none"}
	}
	if st.message == "" {
		switch st.state {
		case StateIdle:
			st.message = "Idle."
		case StateProcessing:
			st.message = "Printing."
		default:
			st.message = "Stopped."
		}
	}
	for _, j := range s.jobs {
		if !j.State().Terminal() {
			st.queued++
		}
	}
	return st
}

func (s *Server) updateStateLocked() {
	state := StateIdle
	switch {
	case s.dev.State == StateStopped:
		state = StateStopped
	case s.current != nil || s.dev.State == StateProcessing:
		state = StateProcessing
	}
	if state != s.lastState {
		s.lastState = state
		s.stateChanged = time.Now()
		s.stateUp = s.upTime(s.stateChanged)
	}
}

func (s *Server) findJob(id int) *Job {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, j := range s.jobs {
		if j.ID == id {
			return j
		}
	}
	return nil
}

// addJob registers a job; init sets its initial state before it becomes visible.
func (s *Server) addJob(name, user string, attrs JobAttrs, init func(j *Job)) *Job {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	j := &Job{ID: s.nextID, Name: name, User: user, Attrs: attrs, created: now}
	j.uuid = jobUUID(s.cfg.UUID, j.ID, now)
	j.upAt.created = s.upTime(now)
	init(j)
	s.nextID++
	s.jobs = append(s.jobs, j)
	s.pruneLocked()
	return j
}

func (s *Server) pruneLocked() {
	for len(s.jobs) > maxRetainedJobs {
		idx := -1
		for i, j := range s.jobs {
			if j.State().Terminal() {
				idx = i
				break
			}
		}
		if idx < 0 {
			return
		}
		s.jobs = append(s.jobs[:idx], s.jobs[idx+1:]...)
	}
}

func (s *Server) enqueue(j *Job) {
	s.mu.Lock()
	s.queue = append(s.queue, j)
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *Server) worker() {
	defer s.wg.Done()
	for {
		s.mu.Lock()
		var next *Job
		for len(s.queue) > 0 && next == nil {
			j := s.queue[0]
			s.queue = s.queue[1:]
			if j.State() == JobPending {
				next = j
			}
		}
		s.mu.Unlock()
		if next != nil {
			s.run(next)
			continue
		}
		select {
		case <-s.ctx.Done():
			return
		case <-s.wake:
		}
	}
}

func (s *Server) run(j *Job) {
	ctx, cancel := context.WithCancel(s.ctx)
	defer cancel()
	now := time.Now()
	j.mu.Lock()
	if j.state != JobPending {
		j.mu.Unlock()
		return
	}
	j.cancel = cancel
	j.setState(JobProcessing, "Job printing.", "job-printing")
	j.processing = now
	j.upAt.processing = s.upTime(now)
	path, format := j.docPath, j.docFormat
	j.mu.Unlock()

	s.pollMu.Lock()
	s.mu.Lock()
	s.current = j
	s.updateStateLocked()
	s.mu.Unlock()
	s.pollMu.Unlock()
	s.logf("job %d: processing %s (%s)", j.ID, j.Name, format)

	err := s.process(ctx, j, path, format)

	now = time.Now()
	j.mu.Lock()
	switch {
	case j.cancelReq:
		j.setState(JobCanceled, "Job canceled.", "job-canceled-by-user")
	case err == nil:
		j.setState(JobCompleted, "Job completed.", "job-completed-successfully")
	case errors.Is(err, ErrDocumentFormat):
		j.setState(JobAborted, err.Error(), "document-format-error")
	case s.ctx.Err() != nil:
		j.setState(JobAborted, "Printer shut down.", "aborted-by-system")
	default:
		j.setState(JobAborted, err.Error(), "aborted-by-system")
	}
	j.completed = now
	j.upAt.completed = s.upTime(now)
	j.cancel = nil
	s.dropJobDocLocked(j)
	state, msg := j.state, j.message
	j.mu.Unlock()

	s.mu.Lock()
	s.current = nil
	s.updateStateLocked()
	s.pruneLocked()
	s.mu.Unlock()
	s.kickStatus()
	s.logf("job %d: %s: %s", j.ID, state, msg)
}

func (s *Server) process(ctx context.Context, j *Job, path, format string) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("processor panic: %v", r)
		}
	}()
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return s.proc(ctx, j, f, format)
}

// cancelJob cancels a job that has not finished. It reports false when the job
// is already terminal.
func (s *Server) cancelJob(j *Job) bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	switch {
	case j.state.Terminal():
		return false
	case j.state == JobProcessing:
		if !j.cancelReq {
			j.cancelReq = true
			j.reasons = []string{"processing-to-stop-point"}
			j.message = "Canceling job."
			if j.cancel != nil {
				j.cancel()
			}
		}
	default:
		j.abortLocked(s, JobCanceled, "Job canceled.", "job-canceled-by-user")
	}
	return true
}

// abortLocked finishes a job that never reached the worker.
func (j *Job) abortLocked(s *Server, st JobState, msg, reason string) {
	now := time.Now()
	j.setState(st, msg, reason)
	j.completed = now
	j.upAt.completed = s.upTime(now)
	j.closed = true
	if j.timeout != nil {
		j.timeout.Stop()
		j.timeout = nil
	}
	s.dropJobDocLocked(j)
}

// dropJobDocLocked deletes the job's spooled document and returns its bytes to
// the spool budget. j.docSize is kept for job-k-octets.
func (s *Server) dropJobDocLocked(j *Job) {
	if j.docPath != "" {
		os.Remove(j.docPath)
		s.spoolUsed.Add(-j.docSize)
		j.docPath = ""
	}
}

// reserveJob holds a job slot for a request that has not registered its job
// yet, so concurrent uploads cannot overrun MaxPendingJobs. release is
// idempotent.
func (s *Server) reserveJob() (release func(), ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	active := s.reserved
	for _, j := range s.jobs {
		if !j.State().Terminal() {
			active++
		}
	}
	if active >= s.cfg.MaxPendingJobs {
		return nil, false
	}
	s.reserved++
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			s.reserved--
			s.mu.Unlock()
		})
	}, true
}

// armTimeout aborts a Create-Job job that does not get its document in time.
func (s *Server) armTimeoutLocked(j *Job) {
	if j.timeout != nil {
		j.timeout.Stop()
	}
	j.timeout = time.AfterFunc(multipleOperationTimeout, func() {
		j.mu.Lock()
		defer j.mu.Unlock()
		if j.state == JobPendingHeld && !j.closed && !j.receiving {
			j.abortLocked(s, JobAborted, "Timed out waiting for the document.", "aborted-by-system")
			s.logf("job %d: aborted, no document within %s", j.ID, multipleOperationTimeout)
		}
	})
}

func jobUUID(printerUUID string, id int, t time.Time) string {
	u := strings.TrimPrefix(printerUUID, "urn:uuid:")
	if len(u) != 36 {
		return printerUUID
	}
	// Reuse the printer UUID's node part so job UUIDs are unique per printer.
	return fmt.Sprintf("urn:uuid:%08x-%04x-%s", uint32(t.Unix()), uint16(id), u[14:])
}

// uriHost is host:port as the client addressed us, for URIs in responses.
func (s *Server) uriHost(r *http.Request) string {
	h := r.Host
	port := strconv.Itoa(int(s.port.Load()))
	if h == "" {
		h = s.cfg.Host
		if h == "" {
			h = "localhost"
		}
	}
	host, p, err := net.SplitHostPort(h)
	if err != nil {
		host = h
	} else {
		port = p
	}
	host = strings.TrimSuffix(strings.Trim(host, "[]"), ".")
	return net.JoinHostPort(host, port)
}
