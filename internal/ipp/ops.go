package ipp

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/OpenPrinting/goipp"
)

// request is one IPP operation being answered.
type request struct {
	s           *Server
	http        *http.Request
	body        io.Reader
	msg         *goipp.Message
	op          goipp.Op
	ops         goipp.Attributes // operation attributes
	host        string
	scheme      string
	status      goipp.Status
	statusMsg   string
	unsupported goipp.Attributes
	groups      []goipp.Group // job/printer groups after the unsupported group
}

type ippError struct {
	status goipp.Status
	msg    string
}

func (e *ippError) Error() string { return e.msg }

func fail(st goipp.Status, format string, args ...any) *ippError {
	return &ippError{status: st, msg: fmt.Sprintf(format, args...)}
}

func (s *Server) serveIPP(w http.ResponseWriter, r *http.Request) {
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(strings.ToLower(ct), "application/ipp") {
		http.Error(w, "Content-Type must be application/ipp", http.StatusBadRequest)
		return
	}
	rc := http.NewResponseController(w)
	body := &idleReader{r: r.Body, rc: rc, idle: bodyIdleTimeout}
	req := &request{s: s, http: r, body: body, msg: &goipp.Message{}, host: s.uriHost(r), scheme: "ipp", status: goipp.StatusOk}
	if r.TLS != nil {
		req.scheme = "ipps"
	}
	var err *ippError
	// goipp has no nesting limit for collections and decodes them recursively, so
	// an unbounded attribute section can overflow the stack. It reads exact
	// lengths, so the document that follows is still unread in r.Body.
	lr := &io.LimitedReader{R: body, N: maxMessageBytes}
	if derr := req.msg.Decode(lr); derr != nil {
		if lr.N <= 0 {
			err = fail(goipp.StatusErrorRequestEntity, "IPP attributes larger than %d bytes.", maxMessageBytes)
		} else {
			err = fail(goipp.StatusErrorBadRequest, "Bad request: %v", derr)
		}
	} else {
		req.op = goipp.Op(req.msg.Code)
		err = req.dispatch()
	}
	// An expired idle deadline would fail net/http's background read and cancel
	// the request context while the response is written.
	_ = rc.SetReadDeadline(time.Time{})
	if err != nil {
		req.status, req.statusMsg = err.status, err.msg
		req.groups = nil
	}
	s.logf("%s %s %s: %s %s", r.RemoteAddr, req.scheme, req.op, req.status, req.statusMsg)
	out, eerr := req.response().EncodeBytes()
	if eerr != nil {
		s.logf("encode %s response: %v", req.op, eerr)
		req.status, req.statusMsg, req.groups, req.unsupported = goipp.StatusErrorInternal, "Internal error.", nil, nil
		out, _ = req.response().EncodeBytes()
	}
	w.Header().Set("Content-Type", "application/ipp")
	w.Header().Set("Content-Length", strconv.Itoa(len(out)))
	_, _ = w.Write(out)
}

const (
	maxMessageBytes = 64 << 10
	// bodyIdleTimeout drops uploads that stall; iOS renders pages while it
	// sends them, so the whole body may take long but keeps moving.
	bodyIdleTimeout = 60 * time.Second
)

// idleReader extends the connection's read deadline before every read, bounding
// how long a stalled client holds a job slot without capping total upload time.
type idleReader struct {
	r    io.Reader
	rc   *http.ResponseController
	idle time.Duration
}

func (b *idleReader) Read(p []byte) (int, error) {
	_ = b.rc.SetReadDeadline(time.Now().Add(b.idle))
	return b.r.Read(p)
}

func (req *request) response() *goipp.Message {
	v := req.msg.Version
	if v.Major() != 1 && v.Major() != 2 {
		v = goipp.MakeVersion(2, 0)
	}
	op := goipp.Attributes{
		goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")),
		goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en")),
	}
	if req.statusMsg != "" {
		op.Add(goipp.MakeAttribute("status-message", goipp.TagText, goipp.String(req.statusMsg)))
	}
	groups := goipp.Groups{{Tag: goipp.TagOperationGroup, Attrs: op}}
	if len(req.unsupported) > 0 {
		groups.Add(goipp.Group{Tag: goipp.TagUnsupportedGroup, Attrs: req.unsupported})
	}
	for _, g := range req.groups {
		groups.Add(g)
	}
	return goipp.NewMessageWithGroups(v, goipp.Code(req.status), req.msg.RequestID, groups)
}

func (req *request) dispatch() *ippError {
	if m := req.msg.Version.Major(); m != 1 && m != 2 {
		return fail(goipp.StatusErrorVersionNotSupported, "Bad request version %s.", req.msg.Version)
	}
	if req.msg.RequestID == 0 {
		return fail(goipp.StatusErrorBadRequest, "Bad request-id 0.")
	}
	if len(req.msg.Groups) == 0 || req.msg.Groups[0].Tag != goipp.TagOperationGroup {
		return fail(goipp.StatusErrorBadRequest, "Missing operation attributes.")
	}
	req.ops = req.msg.Groups[0].Attrs
	if len(req.ops) < 2 || req.ops[0].Name != "attributes-charset" ||
		req.ops[1].Name != "attributes-natural-language" {
		return fail(goipp.StatusErrorBadRequest,
			"attributes-charset and attributes-natural-language must be the first operation attributes.")
	}
	cs, ok := single(&req.ops[0], goipp.TagCharset)
	if !ok {
		return fail(goipp.StatusErrorBadRequest, "Bad attributes-charset.")
	}
	if cs = strings.ToLower(cs); cs != "utf-8" && cs != "us-ascii" {
		req.unsupported.Add(req.ops[0])
		return fail(goipp.StatusErrorCharset, "Unsupported character set %q.", cs)
	}
	if _, ok := single(&req.ops[1], goipp.TagLanguage); !ok {
		return fail(goipp.StatusErrorBadRequest, "Bad attributes-natural-language.")
	}

	switch req.op {
	case goipp.OpGetPrinterAttributes:
		return req.withPrinter(req.getPrinterAttributes)
	case goipp.OpValidateJob:
		return req.withPrinter(req.validateJob)
	case goipp.OpPrintJob:
		return req.withPrinter(req.printJob)
	case goipp.OpCreateJob:
		return req.withPrinter(req.createJob)
	case goipp.OpGetJobs:
		return req.withPrinter(req.getJobs)
	case goipp.OpCancelMyJobs:
		return req.withPrinter(req.cancelMyJobs)
	case goipp.OpIdentifyPrinter:
		return req.withPrinter(req.identifyPrinter)
	case goipp.OpSendDocument:
		return req.withJob(req.sendDocument)
	case goipp.OpCloseJob:
		return req.withJob(req.closeJob)
	case goipp.OpGetJobAttributes:
		return req.withJob(req.getJobAttributes)
	case goipp.OpCancelJob:
		return req.withJob(req.cancelJob)
	}
	return fail(goipp.StatusErrorOperationNotSupported, "Operation %s not supported.", req.op)
}

func (req *request) withPrinter(f func() *ippError) *ippError {
	a := find(req.ops, "printer-uri")
	if a == nil {
		return fail(goipp.StatusErrorBadRequest, "Missing printer-uri.")
	}
	u, ok := single(a, goipp.TagURI)
	if !ok {
		return fail(goipp.StatusErrorBadRequest, "Bad printer-uri.")
	}
	if p := uriPath(u); p != printerPath && p != printerPath+"/" {
		return fail(goipp.StatusErrorNotFound, "%s is not a printer URI.", u)
	}
	return f()
}

func (req *request) withJob(f func(j *Job) *ippError) *ippError {
	var id int
	if a := find(req.ops, "job-uri"); a != nil {
		u, ok := single(a, goipp.TagURI)
		if !ok {
			return fail(goipp.StatusErrorBadRequest, "Bad job-uri.")
		}
		rest, ok := strings.CutPrefix(uriPath(u), printerPath+"/")
		n, err := strconv.Atoi(rest)
		if !ok || err != nil {
			return fail(goipp.StatusErrorNotFound, "%s is not a job URI.", u)
		}
		id = n
	} else {
		perr := req.withPrinter(func() *ippError { return nil })
		if perr != nil {
			return perr
		}
		a := find(req.ops, "job-id")
		if a == nil {
			return fail(goipp.StatusErrorBadRequest, "Missing job-id.")
		}
		n, ok := singleInt(a, goipp.TagInteger)
		if !ok {
			return fail(goipp.StatusErrorBadRequest, "Bad job-id.")
		}
		id = n
	}
	j := req.s.findJob(id)
	if j == nil {
		return fail(goipp.StatusErrorNotFound, "Job #%d does not exist.", id)
	}
	return f(j)
}

func uriPath(u string) string {
	p, err := url.Parse(u)
	if err != nil {
		return ""
	}
	return p.Path
}

func find(attrs goipp.Attributes, name string) *goipp.Attribute {
	for i := range attrs {
		if attrs[i].Name == name {
			return &attrs[i]
		}
	}
	return nil
}

// single returns the attribute's only value when it is a string of the given tag.
func single(a *goipp.Attribute, tags ...goipp.Tag) (string, bool) {
	if a == nil || len(a.Values) != 1 || !hasTag(a.Values[0].T, tags) {
		return "", false
	}
	s, ok := a.Values[0].V.(goipp.String)
	return string(s), ok
}

func singleInt(a *goipp.Attribute, tags ...goipp.Tag) (int, bool) {
	if a == nil || len(a.Values) != 1 || !hasTag(a.Values[0].T, tags) {
		return 0, false
	}
	n, ok := a.Values[0].V.(goipp.Integer)
	return int(n), ok
}

func singleBool(a *goipp.Attribute) (bool, bool) {
	if a == nil || len(a.Values) != 1 || a.Values[0].T != goipp.TagBoolean {
		return false, false
	}
	b, ok := a.Values[0].V.(goipp.Boolean)
	return bool(b), ok
}

func hasTag(t goipp.Tag, tags []goipp.Tag) bool {
	for _, x := range tags {
		if t == x {
			return true
		}
	}
	return false
}
