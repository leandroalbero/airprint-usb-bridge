package ipp

import (
	"bytes"
	"errors"
	"io"
	"os"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/OpenPrinting/goipp"
)

func (req *request) printerView() *view {
	s := req.s
	s.mu.Lock()
	st := s.statusLocked()
	s.mu.Unlock()
	return &view{host: req.host, now: time.Now(), status: st}
}

func (req *request) getPrinterAttributes() *ippError {
	want := parseRequested(find(req.ops, "requested-attributes"), true)
	v := req.printerView()
	attrs := goipp.Attributes{}
	for i := range req.s.attrs {
		pa := &req.s.attrs[i]
		if !want.wants(pa.name, pa.group) {
			continue
		}
		if pa.dyn != nil {
			attrs.Add(pa.dyn(v))
		} else {
			attrs.Add(pa.attr)
		}
	}
	req.groups = []goipp.Group{{Tag: goipp.TagPrinterGroup, Attrs: attrs}}
	return nil
}

func (req *request) validateJob() *ippError {
	_, err := req.checkJobRequest()
	return err
}

func (req *request) printJob() *ippError {
	jr, err := req.checkJobRequest()
	if err != nil {
		return err
	}
	release, ok := req.s.reserveJob()
	if !ok {
		return errTooManyJobs()
	}
	defer release()
	doc, err := req.readDocument(jr.format)
	if err != nil {
		return err
	}
	if doc.size == 0 {
		req.s.dropDoc(doc)
		return fail(goipp.StatusErrorBadRequest, "No document data.")
	}
	j := req.s.addJob(jr.name, jr.user, jr.attrs, func(j *Job) {
		j.attachLocked(doc)
		j.closed = true
		j.setState(JobPending, "Job pending.", "none")
	})
	req.s.enqueue(j)
	req.s.logf("job %d: %q from %s, %d bytes %s", j.ID, j.Name, j.User, doc.size, doc.format)
	req.jobCreatedResponse(j)
	return nil
}

func (req *request) createJob() *ippError {
	jr, err := req.checkJobRequest()
	if err != nil {
		return err
	}
	release, ok := req.s.reserveJob()
	if !ok {
		return errTooManyJobs()
	}
	defer release()
	j := req.s.addJob(jr.name, jr.user, jr.attrs, func(j *Job) {
		j.format = jr.format
		j.setState(JobPendingHeld, "Job created.", "job-data-insufficient")
		req.s.armTimeoutLocked(j)
	})
	req.s.logf("job %d: created %q for %s", j.ID, j.Name, j.User)
	req.jobCreatedResponse(j)
	return nil
}

func (req *request) sendDocument(j *Job) *ippError {
	a := find(req.ops, "last-document")
	if a == nil {
		return fail(goipp.StatusErrorBadRequest, "Missing last-document.")
	}
	last, ok := singleBool(a)
	if !ok {
		return fail(goipp.StatusErrorBadRequest, "Bad last-document.")
	}
	var format string
	if err := req.checkDocumentOps(&format); err != nil {
		return err
	}
	j.mu.Lock()
	open := j.state == JobPendingHeld && !j.closed
	receiving := j.receiving
	if format == "" {
		format = j.format
	}
	// The Create-Job timeout must not fire while the body is arriving: iOS
	// renders pages as it sends them, so a long job can take minutes.
	if open && !receiving {
		j.receiving = true
		if j.timeout != nil {
			j.timeout.Stop()
			j.timeout = nil
		}
	}
	j.mu.Unlock()
	if !open {
		return fail(goipp.StatusErrorNotPossible, "Job #%d is not accepting documents.", j.ID)
	}
	if receiving {
		return fail(goipp.StatusErrorBusy, "Job #%d is already receiving a document.", j.ID)
	}
	doc, derr := req.readDocument(format)

	j.mu.Lock()
	j.receiving = false
	open = j.state == JobPendingHeld && !j.closed
	var err *ippError
	switch {
	case derr != nil:
		err = derr
	case !open:
		err = fail(goipp.StatusErrorNotPossible, "Job #%d is not accepting documents.", j.ID)
	case doc.size > 0 && j.hasDoc:
		err = fail(goipp.StatusErrorMultipleJobsNotSupported, "Only one document per job is supported.")
	}
	if err != nil {
		if open {
			req.s.armTimeoutLocked(j)
		}
		j.mu.Unlock()
		if derr == nil {
			req.s.dropDoc(doc)
		} else if derr.status == goipp.StatusErrorDocumentFormatNotSupported {
			req.s.cancelWith(j, JobAborted, derr.msg, "unsupported-document-format")
		}
		return err
	}
	if doc.size > 0 {
		j.attachLocked(doc)
	} else {
		req.s.dropDoc(doc)
	}
	queue := false
	if last {
		queue = req.s.closeLocked(j)
	} else {
		req.s.armTimeoutLocked(j)
	}
	req.groups = []goipp.Group{{Tag: goipp.TagJobGroup, Attrs: req.s.jobCreatedAttrsLocked(j, req)}}
	j.mu.Unlock()
	if queue {
		req.s.enqueue(j)
	}
	return nil
}

func (req *request) closeJob(j *Job) *ippError {
	j.mu.Lock()
	if j.state != JobPendingHeld || j.closed {
		j.mu.Unlock()
		return fail(goipp.StatusErrorNotPossible, "Job #%d is already closed.", j.ID)
	}
	queue := req.s.closeLocked(j)
	j.mu.Unlock()
	if queue {
		req.s.enqueue(j)
	}
	return nil
}

// closeLocked ends document submission. It reports whether the job must be
// queued; it is aborted when it never got a document. The caller enqueues after
// releasing j.mu because the server lock is always taken before job locks.
func (s *Server) closeLocked(j *Job) bool {
	if j.timeout != nil {
		j.timeout.Stop()
		j.timeout = nil
	}
	j.closed = true
	if !j.hasDoc {
		j.abortLocked(s, JobAborted, "Job closed without a document.", "aborted-by-system")
		return false
	}
	j.setState(JobPending, "Job pending.", "none")
	s.logf("job %d: %q from %s, %d bytes %s", j.ID, j.Name, j.User, j.docSize, j.docFormat)
	return true
}

func (s *Server) cancelWith(j *Job, st JobState, msg, reason string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if !j.state.Terminal() && j.state != JobProcessing {
		j.abortLocked(s, st, msg, reason)
	}
}

func (req *request) cancelJob(j *Job) *ippError {
	if !req.s.cancelJob(j) {
		return fail(goipp.StatusErrorNotPossible, "Job #%d is already %s and cannot be canceled.", j.ID, j.State())
	}
	req.s.logf("job %d: cancel requested by %s", j.ID, req.userName())
	return nil
}

func (req *request) cancelMyJobs() *ippError {
	user := req.userName()
	var targets []*Job
	if a := find(req.ops, "job-ids"); a != nil {
		for _, v := range a.Values {
			n, ok := v.V.(goipp.Integer)
			if !ok || v.T != goipp.TagInteger {
				return fail(goipp.StatusErrorBadRequest, "Bad job-ids.")
			}
			j := req.s.findJob(int(n))
			if j == nil || j.User != user {
				req.unsupported.Add(*a)
				return fail(goipp.StatusErrorNotFound, "Job #%d does not exist.", n)
			}
			if j.State().Terminal() {
				req.unsupported.Add(*a)
				return fail(goipp.StatusErrorNotPossible, "Job #%d cannot be canceled.", n)
			}
			targets = append(targets, j)
		}
	} else {
		req.s.mu.Lock()
		for _, j := range req.s.jobs {
			if j.User == user {
				targets = append(targets, j)
			}
		}
		req.s.mu.Unlock()
	}
	for _, j := range targets {
		if req.s.cancelJob(j) {
			req.s.logf("job %d: cancel requested by %s", j.ID, user)
		}
	}
	return nil
}

func (req *request) identifyPrinter() *ippError {
	actions := []string{"sound"}
	if a := find(req.ops, "identify-actions"); a != nil {
		actions = actions[:0]
		for _, v := range a.Values {
			s, ok := v.V.(goipp.String)
			if !ok || v.T != goipp.TagKeyword || (s != "display" && s != "sound") {
				req.unsupported.Add(*a)
				return fail(goipp.StatusErrorAttributesOrValues, "Unsupported identify-actions.")
			}
			actions = append(actions, string(s))
		}
	}
	msg, _ := textValue(find(req.ops, "message"))
	req.s.logf("identify-printer %v %q", actions, msg)
	if f := req.s.cfg.Identify; f != nil {
		go f(actions, msg)
	}
	return nil
}

func (req *request) getJobAttributes(j *Job) *ippError {
	want := parseRequested(find(req.ops, "requested-attributes"), true)
	req.groups = []goipp.Group{{Tag: goipp.TagJobGroup, Attrs: req.s.jobAttrs(j, req, want)}}
	return nil
}

func (req *request) getJobs() *ippError {
	which := "not-completed"
	if a := find(req.ops, "which-jobs"); a != nil {
		w, ok := single(a, goipp.TagKeyword)
		if !ok || !contains(whichJobsSupported, w) {
			req.unsupported.Add(*a)
			return fail(goipp.StatusErrorAttributesOrValues, "Unsupported which-jobs value.")
		}
		which = w
	}
	limit := 0
	if a := find(req.ops, "limit"); a != nil {
		n, ok := singleInt(a, goipp.TagInteger)
		if !ok || n < 1 {
			return fail(goipp.StatusErrorBadRequest, "Bad limit.")
		}
		limit = n
	}
	myJobs := false
	if a := find(req.ops, "my-jobs"); a != nil {
		b, ok := singleBool(a)
		if !ok {
			return fail(goipp.StatusErrorBadRequest, "Bad my-jobs.")
		}
		myJobs = b
	}
	user := req.userName()
	want := parseRequested(find(req.ops, "requested-attributes"), false)
	if find(req.ops, "requested-attributes") == nil {
		want.names["job-id"], want.names["job-uri"] = true, true
	}

	req.s.mu.Lock()
	jobs := append([]*Job(nil), req.s.jobs...)
	req.s.mu.Unlock()
	ascending := which == "not-completed" || which == "pending" || which == "pending-held" ||
		which == "processing" || which == "processing-stopped"
	if !ascending {
		for i, k := 0, len(jobs)-1; i < k; i, k = i+1, k-1 {
			jobs[i], jobs[k] = jobs[k], jobs[i]
		}
	}
	for _, j := range jobs {
		if limit > 0 && len(req.groups) >= limit {
			break
		}
		if myJobs && j.User != user {
			continue
		}
		if !whichMatches(which, j.State()) {
			continue
		}
		req.groups = append(req.groups, goipp.Group{Tag: goipp.TagJobGroup, Attrs: req.s.jobAttrs(j, req, want)})
	}
	return nil
}

func whichMatches(which string, st JobState) bool {
	switch which {
	case "all":
		return true
	case "completed":
		return st.Terminal()
	case "not-completed":
		return !st.Terminal()
	}
	return which == st.String()
}

// document is a spooled document body.
type document struct {
	path   string
	size   int64
	format string
}

// dropDoc deletes a spooled document that no job owns and returns its bytes to
// the spool budget.
func (s *Server) dropDoc(d document) {
	if d.path != "" {
		os.Remove(d.path)
		s.spoolUsed.Add(-d.size)
	}
}

func errTooManyJobs() *ippError {
	return fail(goipp.StatusErrorBusy, "Too many jobs queued; try again later.")
}

var errSpoolFull = errors.New("spool full")

// spoolWriter charges every byte written to the server-wide spool budget.
type spoolWriter struct {
	w    io.Writer
	used *atomic.Int64
	max  int64
}

func (sw *spoolWriter) Write(p []byte) (int, error) {
	n := int64(len(p))
	if sw.used.Add(n) > sw.max {
		sw.used.Add(-n)
		return 0, errSpoolFull
	}
	m, err := sw.w.Write(p)
	if int64(m) < n {
		sw.used.Add(int64(m) - n)
	}
	return m, err
}

func (j *Job) attachLocked(d document) {
	j.docPath, j.docSize, j.docFormat, j.hasDoc = d.path, d.size, d.format, true
}

// readDocument spools the request body that follows the IPP message and
// resolves the document format, sniffing application/octet-stream.
func (req *request) readDocument(declared string) (document, *ippError) {
	f, err := os.CreateTemp(req.s.spoolDir, "job-*.doc")
	if err != nil {
		return document{}, fail(goipp.StatusErrorInternal, "Unable to spool document: %v", err)
	}
	doc := document{path: f.Name()}
	max := req.s.cfg.MaxDocumentBytes
	var head bytes.Buffer
	sw := &spoolWriter{w: f, used: &req.s.spoolUsed, max: req.s.cfg.MaxSpoolBytes}
	n, err := io.Copy(sw, io.TeeReader(io.LimitReader(req.body, max+1), &headWriter{buf: &head, max: 16}))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	doc.size = n
	switch {
	case errors.Is(err, errSpoolFull):
		req.s.dropDoc(doc)
		return document{}, fail(goipp.StatusErrorBusy, "Spool full; try again later.")
	case err != nil:
		req.s.dropDoc(doc)
		return document{}, fail(goipp.StatusErrorBadRequest, "Unable to read document: %v", err)
	case n > max:
		req.s.dropDoc(doc)
		return document{}, fail(goipp.StatusErrorRequestEntity, "Document larger than %d bytes.", max)
	case n == 0:
		doc.format = declared
		return doc, nil
	}
	doc.format = sniffFormat(head.Bytes())
	if doc.format != "" && doc.format != formatURF && doc.format != formatPWG && !contains(req.s.cfg.Caps.Formats, doc.format) {
		doc.format = ""
	}
	if doc.format == "" && (declared == formatURF || declared == formatPWG || contains(req.s.cfg.Caps.Formats, declared)) {
		doc.format = declared
	}
	if doc.format == "" {
		req.s.dropDoc(doc)
		if declared == "" {
			declared = formatOctetStream
		}
		req.unsupported.Add(goipp.MakeAttribute("document-format", goipp.TagMimeType, goipp.String(declared)))
		return document{}, fail(goipp.StatusErrorDocumentFormatNotSupported,
			"Unrecognized document data; send image/urf or image/pwg-raster.")
	}
	return doc, nil
}

type headWriter struct {
	buf *bytes.Buffer
	max int
}

func (w *headWriter) Write(p []byte) (int, error) {
	if room := w.max - w.buf.Len(); room > 0 {
		if len(p) < room {
			room = len(p)
		}
		w.buf.Write(p[:room])
	}
	return len(p), nil
}

// sniffFormat recognizes Apple raster and PWG raster by their sync words, and PDF and PostScript
// for drivers that take them as they are.
func sniffFormat(head []byte) string {
	switch {
	case bytes.HasPrefix(head, []byte("UNIRAST\x00")):
		return formatURF
	case bytes.HasPrefix(head, []byte("RaS2")):
		return formatPWG
	case bytes.HasPrefix(head, []byte("%PDF-")):
		return "application/pdf"
	case bytes.HasPrefix(head, []byte("%!")):
		return "application/postscript"
	}
	return ""
}

func (req *request) jobCreatedResponse(j *Job) {
	j.mu.Lock()
	defer j.mu.Unlock()
	req.groups = []goipp.Group{{Tag: goipp.TagJobGroup, Attrs: req.s.jobCreatedAttrsLocked(j, req)}}
}

func (s *Server) jobCreatedAttrsLocked(j *Job, req *request) goipp.Attributes {
	return goipp.Attributes{
		mkInts("job-id", goipp.TagInteger, j.ID),
		mk("job-uri", goipp.TagURI, goipp.String(req.jobURI(j.ID))),
		mkInts("job-state", goipp.TagEnum, int(j.state)),
		mk("job-state-message", goipp.TagText, goipp.String(j.message)),
		mk("job-state-reasons", goipp.TagKeyword, strs(reasonsOrNone(j.reasons))...),
	}
}

func reasonsOrNone(r []string) []string {
	if len(r) == 0 {
		return []string{"none"}
	}
	return r
}

func (req *request) printerURI() string { return req.scheme + "://" + req.host + printerPath }

func (req *request) jobURI(id int) string { return req.printerURI() + "/" + strconv.Itoa(id) }

// jobAttrs returns the requested job attributes in a fixed order.
func (s *Server) jobAttrs(j *Job, req *request, want requested) goipp.Attributes {
	s.mu.Lock()
	st := s.statusLocked()
	s.mu.Unlock()

	j.mu.Lock()
	defer j.mu.Unlock()
	const d, t = groupDescription, groupTemplate
	out := goipp.Attributes{}
	add := func(g attrGroup, a goipp.Attribute) {
		if want.wants(a.Name, g) {
			out.Add(a)
		}
	}
	timeAttr := func(name string, up int) goipp.Attribute {
		if up == 0 {
			return mk(name, goipp.TagNoValue, goipp.Void{})
		}
		return mkInts(name, goipp.TagInteger, up)
	}
	dateAttr := func(name string, tm time.Time) goipp.Attribute {
		if tm.IsZero() {
			return mk(name, goipp.TagNoValue, goipp.Void{})
		}
		return mk(name, goipp.TagDateTime, goipp.Time{Time: tm.UTC()})
	}

	add(d, mkInts("job-id", goipp.TagInteger, j.ID))
	add(d, mk("job-uri", goipp.TagURI, goipp.String(req.jobURI(j.ID))))
	add(d, mk("job-printer-uri", goipp.TagURI, goipp.String(req.printerURI())))
	add(d, mk("job-uuid", goipp.TagURI, goipp.String(j.uuid)))
	add(d, mk("job-name", goipp.TagName, goipp.String(j.Name)))
	add(d, mk("job-originating-user-name", goipp.TagName, goipp.String(j.User)))
	add(d, mkInts("job-state", goipp.TagEnum, int(j.state)))
	add(d, mk("job-state-reasons", goipp.TagKeyword, strs(reasonsOrNone(j.reasons))...))
	add(d, mk("job-state-message", goipp.TagText, goipp.String(j.message)))
	add(d, mkInts("job-impressions-completed", goipp.TagInteger, j.impressions))
	add(d, mkInts("job-media-sheets-completed", goipp.TagInteger, j.impressions))
	add(d, mkInts("job-k-octets", goipp.TagInteger, int((j.docSize+1023)/1024)))
	docs := 0
	if j.hasDoc {
		docs = 1
	}
	add(d, mkInts("number-of-documents", goipp.TagInteger, docs))
	if j.docFormat != "" {
		add(d, mk("document-format-supplied", goipp.TagMimeType, goipp.String(j.docFormat)))
	}
	add(d, mkInts("job-printer-up-time", goipp.TagInteger, st.upTime))
	add(d, mk("job-printer-state-reasons", goipp.TagKeyword, strs(st.reasons)...))
	add(d, mk("job-printer-state-message", goipp.TagText, goipp.String(st.message)))
	add(d, timeAttr("time-at-creation", j.upAt.created))
	add(d, timeAttr("time-at-processing", j.upAt.processing))
	add(d, timeAttr("time-at-completed", j.upAt.completed))
	add(d, dateAttr("date-time-at-creation", j.created))
	add(d, dateAttr("date-time-at-processing", j.processing))
	add(d, dateAttr("date-time-at-completed", j.completed))

	ja := &j.Attrs
	add(t, mkInts("copies", goipp.TagInteger, ja.Copies))
	add(t, mkInts("print-quality", goipp.TagEnum, ja.PrintQuality))
	if ja.Media != "" {
		add(t, mk("media", goipp.TagKeyword, goipp.String(ja.Media)))
	}
	if ja.MediaCol != nil {
		add(t, mk("media-col", goipp.TagBeginCollection, ja.MediaCol.collection()))
	}
	if ja.Orientation != 0 {
		add(t, mkInts("orientation-requested", goipp.TagEnum, ja.Orientation))
	}
	if ja.ColorMode != "" {
		add(t, mk("print-color-mode", goipp.TagKeyword, goipp.String(ja.ColorMode)))
	}
	if ja.Scaling != "" {
		add(t, mk("print-scaling", goipp.TagKeyword, goipp.String(ja.Scaling)))
	}
	if ja.XDPI != 0 {
		add(t, mk("printer-resolution", goipp.TagResolution, dpi(ja.XDPI, ja.YDPI)))
	}
	return out
}

func (mc *MediaCol) collection() goipp.Collection {
	var col goipp.Collection
	if mc.XDimension != 0 {
		size := goipp.Collection{memberInt("x-dimension", mc.XDimension)}
		if mc.YDimension != 0 {
			size = append(size, memberInt("y-dimension", mc.YDimension))
		}
		col = append(col, goipp.MakeAttribute("media-size", goipp.TagBeginCollection, size))
	}
	if mc.SizeName != "" {
		col = append(col, goipp.MakeAttribute("media-size-name", goipp.TagKeyword, goipp.String(mc.SizeName)))
	}
	col = append(col,
		memberInt("media-bottom-margin", mc.Bottom), memberInt("media-left-margin", mc.Left),
		memberInt("media-right-margin", mc.Right), memberInt("media-top-margin", mc.Top))
	if mc.Source != "" {
		col = append(col, goipp.MakeAttribute("media-source", goipp.TagKeyword, goipp.String(mc.Source)))
	}
	if mc.Type != "" {
		col = append(col, goipp.MakeAttribute("media-type", goipp.TagKeyword, goipp.String(mc.Type)))
	}
	return col
}
