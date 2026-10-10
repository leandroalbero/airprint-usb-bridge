package cups

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultFilterDirs are where Linux distributions and macOS install CUPS filters.
var DefaultFilterDirs = []string{"/usr/lib/cups/filter", "/usr/libexec/cups/filter"}

// printerName is argv[0] and $PRINTER for the filter: the queue name under cupsd.
const printerName = "airprint-usb-bridge"

// findFilter resolves a *cupsFilter program: an absolute path as is, a bare name in dirs.
func findFilter(program string, dirs []string) (string, error) {
	if filepath.IsAbs(program) {
		return program, checkExecutable(program)
	}
	if program == "" || strings.ContainsRune(program, '/') || program == "." || program == ".." {
		return "", fmt.Errorf("filter %q: want a bare program name or an absolute path", program)
	}
	var tried []string
	for _, d := range dirs {
		p := filepath.Join(d, program)
		if err := checkExecutable(p); err == nil {
			return p, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		tried = append(tried, d)
	}
	return "", fmt.Errorf("filter %s not found in %s (install the driver package, or set --driver-opt filter-dir= or filter=)",
		program, strings.Join(tried, ", "))
}

func checkExecutable(p string) error {
	fi, err := os.Stat(p)
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() || fi.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("filter %s is not an executable file", p)
	}
	return nil
}

// filterEnv is the environment cupsd gives a filter, without inheriting the bridge's.
func filterEnv(ppd, finalType, deviceURI, filterPath string) []string {
	env := []string{
		"PATH=/usr/bin:/bin:/usr/sbin:/sbin",
		"PPD=" + ppd,
		"CHARSET=utf-8",
		"LANG=C",
		"CONTENT_TYPE=application/vnd.cups-raster",
		"FINAL_CONTENT_TYPE=" + finalType,
		"DEVICE_URI=" + deviceURI,
		"PRINTER=" + printerName,
		"TMPDIR=" + os.TempDir(),
	}
	for _, kv := range [][2]string{{"CUPS_DATADIR", "/usr/share/cups"}, {"CUPS_SERVERROOT", "/etc/cups"}} {
		if fi, err := os.Stat(kv[1]); err == nil && fi.IsDir() {
			env = append(env, kv[0]+"="+kv[1])
		}
	}
	if dir := filepath.Dir(filterPath); filepath.Base(dir) == "filter" {
		env = append(env, "CUPS_SERVERBIN="+filepath.Dir(dir))
	}
	return env
}

// argText makes a job attribute safe as a filter argument: exec refuses NUL, and control
// characters would corrupt the filter's logs.
func argText(s, def string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
	if len(s) > 255 {
		s = s[:255]
	}
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}

// optionString encodes options the way cupsParseOptions reads argv[5].
func optionString(keys []string, opts map[string]string) string {
	var b strings.Builder
	for _, k := range keys {
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(k)
		b.WriteByte('=')
		for _, r := range opts[k] {
			if r == ' ' || r == '\t' || r == '\\' || r == '"' || r == '\'' {
				b.WriteByte('\\')
			}
			b.WriteRune(r)
		}
	}
	return b.String()
}

type filterRun struct {
	path      string
	args      []string // argv[1:]
	env       []string
	timeout   time.Duration
	maxOutput int64
	// grace is how long the filter may still run after the job is cancelled, once part of its
	// output reached the printer.
	grace   time.Duration
	logf    func(format string, args ...any)
	message func(string) // job-state-message; nil = none
}

var errOutputCap = errors.New("output cap reached")

// cancelGrace bounds how long a cancelled job's filter may take to finish the pages it has: a
// gs-based filter on a small board can need tens of seconds for a 600 dpi page.
const cancelGrace = time.Minute

// run executes the filter directly (no shell) with stdin as its input and copies its output to
// stdout as it comes. The filter is killed when it makes no progress (reads no input, writes no
// output) for the timeout, when the output passes maxOutput bytes, or when stdout fails. When
// parent is cancelled, stdin is expected to end; a filter that has already sent output then gets
// the grace period to end its device job, and one that has not is killed at once. It returns the
// number of bytes written to stdout.
func (f filterRun) run(parent context.Context, stdin io.Reader, stdout io.Writer) (int64, error) {
	name := filepath.Base(f.path)
	// Killing the filter as soon as the job is cancelled would cut the printer off mid-page,
	// with no job ending, and the next job's first bytes would be read as this one's data.
	ctx, cancel := context.WithCancel(context.WithoutCancel(parent))
	defer cancel()
	act := &activity{}
	act.touch()
	in := &activeReader{r: stdin, act: act}
	out := &outWriter{w: stdout, max: f.maxOutput, cancel: cancel, act: act}
	cmd := exec.CommandContext(ctx, f.path)
	cmd.Args = append([]string{printerName}, f.args...)
	cmd.Env = f.env
	cmd.Stdin = in
	cmd.Stdout = out
	log := &stderrLog{logf: f.logf, message: f.message, name: name}
	cmd.Stderr = log
	cmd.WaitDelay = 5 * time.Second
	ownProcessGroup(cmd)
	start := time.Now()
	var timedOut, gaveUp atomic.Bool
	done := make(chan struct{})
	go func() {
		select {
		case <-done:
			return
		case <-parent.Done():
		}
		if out.stopIfEmpty(parent.Err()) {
			cancel()
			return
		}
		t := time.NewTimer(f.grace)
		defer t.Stop()
		select {
		case <-done:
		case <-t.C:
			gaveUp.Store(true)
			cancel()
		}
	}()
	go func() {
		t := time.NewTicker(min(f.timeout/4, time.Second))
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				if act.idle() > f.timeout {
					timedOut.Store(true)
					cancel()
					return
				}
			}
		}
	}()
	err := cmd.Run()
	close(done)
	killGroup(cmd)
	log.flush()
	n, werr, over := out.result()
	switch {
	case over:
		return n, fmt.Errorf("filter %s: output exceeds %d bytes (raise --driver-opt max-output)", name, f.maxOutput)
	case werr != nil:
		return n, werr
	case gaveUp.Load():
		err := fmt.Errorf("filter %s: killed %v after the job was cancelled; the printer may not have got the end of the job: %w", name, f.grace, parent.Err())
		if f.logf != nil {
			f.logf("%v", err)
		}
		return n, err
	case parent.Err() != nil:
		return n, parent.Err()
	case timedOut.Load():
		return n, fmt.Errorf("filter %s: timed out after %v without progress%s", name, f.timeout, log.summary())
	case err != nil:
		return n, fmt.Errorf("filter %s failed: %w%s", name, err, log.summary())
	case n == 0:
		return 0, fmt.Errorf("filter %s produced no output%s", name, log.summary())
	}
	if f.logf != nil {
		f.logf("filter %s: %d bytes in %v", name, n, time.Since(start).Round(time.Millisecond))
	}
	return n, nil
}

// activity is when the filter last read input or wrote output.
type activity struct{ last atomic.Int64 }

func (a *activity) touch()              { a.last.Store(time.Now().UnixNano()) }
func (a *activity) idle() time.Duration { return time.Duration(time.Now().UnixNano() - a.last.Load()) }

type activeReader struct {
	r   io.Reader
	act *activity
}

func (r *activeReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	if n > 0 {
		r.act.touch()
	}
	return n, err
}

// outWriter passes the filter's output on, and stops the filter once it passes max bytes or the
// destination fails: a filter whose output is no longer read would block until the timeout.
type outWriter struct {
	mu     sync.Mutex
	w      io.Writer
	n      int64
	max    int64
	isOver bool
	err    error
	cancel context.CancelFunc
	act    *activity
}

func (o *outWriter) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.err != nil {
		return 0, o.err
	}
	if o.n+int64(len(p)) > o.max {
		o.isOver = true
		o.err = errOutputCap
		o.cancel()
		return 0, o.err
	}
	n, err := o.w.Write(p)
	o.n += int64(n)
	o.act.touch()
	if err != nil {
		o.err = err
		o.cancel()
	}
	return n, err
}

// stopIfEmpty makes every later write fail with err if nothing was passed on yet, and reports
// whether it did.
func (o *outWriter) stopIfEmpty(err error) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.n > 0 {
		return false
	}
	if o.err == nil {
		o.err = err
	}
	return true
}

// result is the bytes passed on, the destination's error and whether the cap was passed.
func (o *outWriter) result() (int64, error, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.isOver {
		return o.n, nil, true
	}
	return o.n, o.err, false
}

const (
	maxStderrLine   = 4096
	maxStderrLogged = 100
	maxKeptMessages = 8
)

// stderrLog reads the filter's message lines (CUPS "LEVEL: text" convention): ERROR and STATE
// lines go to the job log and the error returned on failure, WARNING and NOTICE to the log.
// INFO lines are dropped with DEBUG: under cupsd they only update printer-state-message, and
// filters send one per few percent of every page.
type stderrLog struct {
	mu      sync.Mutex
	logf    func(format string, args ...any)
	message func(string)
	name    string
	partial []byte
	errs    []string
	states  []string
	logged  int
	dropped int
}

func (s *stderrLog) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.partial = append(s.partial, p...)
	for {
		i := bytes.IndexByte(s.partial, '\n')
		if i < 0 {
			break
		}
		s.line(string(s.partial[:i]))
		s.partial = s.partial[i+1:]
	}
	if len(s.partial) > maxStderrLine {
		s.line(string(s.partial[:maxStderrLine]))
		s.partial = s.partial[:0]
	}
	return len(p), nil
}

func (s *stderrLog) flush() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.partial) > 0 {
		s.line(string(s.partial))
		s.partial = nil
	}
}

func (s *stderrLog) line(l string) {
	l = strings.TrimRight(l, "\r")
	if len(l) > maxStderrLine {
		l = l[:maxStderrLine]
	}
	if strings.TrimSpace(l) == "" {
		return
	}
	level, msg, ok := strings.Cut(l, ":")
	msg = strings.TrimSpace(msg)
	if !ok {
		level, msg = "", l
	}
	switch level {
	case "DEBUG", "DEBUG2", "INFO", "ATTR", "PPD", "PAGE", "CONTROL":
		return
	case "ERROR", "EMERG", "ALERT", "CRIT":
		if len(s.errs) < maxKeptMessages {
			s.errs = append(s.errs, msg)
		}
		if s.message != nil && msg != "" {
			s.message(msg)
		}
		s.log("%s: %s", strings.ToLower(level), msg)
	case "STATE":
		if len(s.states) < maxKeptMessages {
			s.states = append(s.states, msg)
		}
		s.log("state %s", msg)
	case "WARNING", "NOTICE":
		s.log("%s: %s", strings.ToLower(level), msg)
	default:
		s.log("%s", l)
	}
}

func (s *stderrLog) log(format string, args ...any) {
	if s.logf == nil {
		return
	}
	if s.logged >= maxStderrLogged {
		s.dropped++
		return
	}
	s.logged++
	s.logf("filter %s: "+format, append([]any{s.name}, args...)...)
}

// summary is appended to a failure: the filter's ERROR and STATE lines.
func (s *stderrLog) summary() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var b strings.Builder
	if len(s.errs) > 0 {
		b.WriteString(": ")
		b.WriteString(strings.Join(s.errs, "; "))
	}
	if len(s.states) > 0 {
		b.WriteString(" (state ")
		b.WriteString(strings.Join(s.states, " "))
		b.WriteString(")")
	}
	return b.String()
}
