package ipp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OpenPrinting/goipp"

	"github.com/leandroalbero/airprint-usb-bridge/internal/driver/brotherql"
)

func names(attrs goipp.Attributes) []string {
	out := make([]string, len(attrs))
	for i, a := range attrs {
		out[i] = a.Name
	}
	return out
}

func requestedAttrs(vals ...string) goipp.Attribute {
	a := goipp.Attribute{Name: "requested-attributes"}
	for _, v := range vals {
		a.Values.Add(goipp.TagKeyword, goipp.String(v))
	}
	return a
}

func TestGetPrinterAttributesGroups(t *testing.T) {
	ts := startServer(t, testConfig(), (&recorder{}).process)
	get := func(ra ...string) goipp.Attributes {
		t.Helper()
		var extra []goipp.Attribute
		if ra != nil {
			extra = append(extra, requestedAttrs(ra...))
		}
		resp := ts.do(t, "ipp", opRequest(goipp.OpGetPrinterAttributes, ts.uri("ipp"), extra...), nil)
		if status(resp) != goipp.StatusOk {
			t.Fatalf("%v: status %v", ra, status(resp))
		}
		if got := names(resp.Operation); strings.Join(got, ",") != "attributes-charset,attributes-natural-language" {
			t.Fatalf("operation group %v", got)
		}
		return resp.Printer
	}
	has := func(attrs goipp.Attributes, name string) bool { _, ok := attrOf(attrs, name); return ok }

	all := get("all")
	if has(all, "media-col-database") {
		t.Error("all must not include media-col-database")
	}
	if len(all) != len(ts.attrs)-1 {
		t.Errorf("all: %d attributes, want %d", len(all), len(ts.attrs)-1)
	}
	if def := get(); len(def) != len(all) {
		t.Errorf("no requested-attributes: %d attributes, want %d", len(def), len(all))
	}
	both := get("all", "media-col-database")
	if !has(both, "media-col-database") || len(both) != len(ts.attrs) {
		t.Errorf("all,media-col-database: %d attributes", len(both))
	}
	db := get("media-col-database")
	if len(db) != 1 || len(db[0].Values) != 3 {
		t.Errorf("media-col-database: %v", db)
	}
	desc := get("printer-description")
	if has(desc, "copies-default") || !has(desc, "printer-uuid") || has(desc, "media-col-database") {
		t.Errorf("printer-description: %v", names(desc))
	}
	tmpl := get("job-template")
	if !has(tmpl, "copies-default") || !has(tmpl, "media-col-ready") || has(tmpl, "printer-uuid") || has(tmpl, "media-col-database") {
		t.Errorf("job-template: %v", names(tmpl))
	}
	// The iOS polling set, including names we do not support.
	poll := get("printer-state-reasons", "media-source-supported", "printer-input-tray", "printer-mandatory-job-attributes")
	if got := strings.Join(names(poll), ","); got != "media-source-supported,printer-input-tray,printer-state-reasons" {
		t.Errorf("poll set in printer order: %s", got)
	}
	if got := get("printer-mandatory-job-attributes"); len(got) != 0 {
		t.Errorf("unknown only: %v", got)
	}
	if v := strOf(t, get("printer-uri-supported"), "printer-uri-supported"); v != ts.uri("ipp") {
		t.Errorf("printer-uri-supported %s", v)
	}
}

func TestGetPrinterAttributesTLSHost(t *testing.T) {
	ts := startServer(t, testConfig(), (&recorder{}).process)
	resp := ts.do(t, "ipps", opRequest(goipp.OpGetPrinterAttributes, ts.uri("ipps"), requestedAttrs("printer-more-info", "printer-uri-supported")), nil)
	a, _ := attrOf(resp.Printer, "printer-uri-supported")
	if len(a.Values) != 2 || a.Values[1].V.String() != ts.uri("ipps") {
		t.Errorf("printer-uri-supported %v", a.Values)
	}
	if v := strOf(t, resp.Printer, "printer-more-info"); v != "https://"+ts.addr+"/" {
		t.Errorf("printer-more-info %s", v)
	}
}

func TestURIHostFromHeader(t *testing.T) {
	ts := startServer(t, testConfig(), (&recorder{}).process)
	cases := map[string]string{
		"printer.local.:8631": "printer.local:8631",
		"printer.local":       "printer.local:" + strings.Split(ts.addr, ":")[1],
		"[fe80::1]:8631":      "[fe80::1]:8631",
		"":                    "localhost:" + strings.Split(ts.addr, ":")[1],
	}
	for in, want := range cases {
		r := &http.Request{Host: in}
		if got := ts.uriHost(r); got != want {
			t.Errorf("uriHost(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRequestValidation(t *testing.T) {
	ts := startServer(t, testConfig(), (&recorder{}).process)
	uri := ts.uri("ipp")

	m := opRequest(goipp.OpGetPrinterAttributes, uri)
	m.Operation[0], m.Operation[1] = m.Operation[1], m.Operation[0]
	if st := status(ts.do(t, "ipp", m, nil)); st != goipp.StatusErrorBadRequest {
		t.Errorf("swapped charset/language: %v", st)
	}

	m = opRequest(goipp.OpGetPrinterAttributes, uri)
	m.Operation[0] = goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("iso-8859-1"))
	if st := status(ts.do(t, "ipp", m, nil)); st != goipp.StatusErrorCharset {
		t.Errorf("latin-1: %v", st)
	}

	m = opRequest(goipp.OpGetPrinterAttributes, uri)
	m.Version = goipp.MakeVersion(3, 0)
	if st := status(ts.do(t, "ipp", m, nil)); st != goipp.StatusErrorVersionNotSupported {
		t.Errorf("version 3.0: %v", st)
	}

	m = opRequest(goipp.OpGetPrinterAttributes, uri)
	m.Version = goipp.MakeVersion(1, 1)
	resp := ts.do(t, "ipp", m, nil)
	if status(resp) != goipp.StatusOk || resp.Version != m.Version {
		t.Errorf("version 1.1: %v %v", status(resp), resp.Version)
	}

	m = opRequest(goipp.OpGetPrinterAttributes, "ipp://"+ts.addr+"/ipp/other")
	if st := status(ts.do(t, "ipp", m, nil)); st != goipp.StatusErrorNotFound {
		t.Errorf("wrong path: %v", st)
	}

	m = goipp.NewRequest(goipp.MakeVersion(2, 0), goipp.OpGetPrinterAttributes, 1)
	m.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	m.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en")))
	if st := status(ts.do(t, "ipp", m, nil)); st != goipp.StatusErrorBadRequest {
		t.Errorf("no printer-uri: %v", st)
	}

	for _, op := range []goipp.Op{goipp.OpPrintURI, goipp.OpSendURI, goipp.OpHoldJob, goipp.OpPausePrinter} {
		if st := status(ts.do(t, "ipp", opRequest(op, uri), nil)); st != goipp.StatusErrorOperationNotSupported {
			t.Errorf("%v: %v", op, st)
		}
	}

	// A host with a trailing dot, as iOS sends it, is accepted.
	m = opRequest(goipp.OpGetPrinterAttributes, "ipps://printer.local.:8631/ipp/print")
	if st := status(ts.do(t, "ipp", m, nil)); st != goipp.StatusOk {
		t.Errorf("trailing-dot host: %v", st)
	}

	resp2, err := http.Post("http://"+ts.addr+printerPath, "text/plain", strings.NewReader("x"))
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusBadRequest {
		t.Errorf("text/plain POST: HTTP %d", resp2.StatusCode)
	}
	resp2, err = http.Post("http://"+ts.addr+printerPath, "application/ipp", strings.NewReader("\x02\x00"))
	if err != nil {
		t.Fatal(err)
	}
	var bad goipp.Message
	if err := bad.Decode(resp2.Body); err != nil || goipp.Status(bad.Code) != goipp.StatusErrorBadRequest {
		t.Errorf("truncated message: %v %v", err, goipp.Status(bad.Code))
	}
	resp2.Body.Close()
}

func validateJob(t *testing.T, ts *testServer, fidelity *bool, jobAttrs ...goipp.Attribute) *goipp.Message {
	t.Helper()
	m := opRequest(goipp.OpValidateJob, ts.uri("ipps"),
		goipp.MakeAttribute("document-format", goipp.TagMimeType, goipp.String("image/urf")))
	if fidelity != nil {
		m.Operation.Add(goipp.MakeAttribute("ipp-attribute-fidelity", goipp.TagBoolean, goipp.Boolean(*fidelity)))
	}
	m.Job = jobAttrs
	return ts.do(t, "ipps", m, nil)
}

func TestValidateJobMedia(t *testing.T) {
	ts := startServer(t, testConfig(), (&recorder{}).process)
	yes, no := true, false
	media := func(v string) goipp.Attribute {
		return goipp.MakeAttribute("media", goipp.TagKeyword, goipp.String(v))
	}
	iosAttrs := []goipp.Attribute{
		rollMediaCol(6200, 1670),
		goipp.MakeAttribute("orientation-requested", goipp.TagEnum, goipp.Integer(3)),
		goipp.MakeAttribute("print-color-mode", goipp.TagKeyword, goipp.String("monochrome")),
		goipp.MakeAttribute("print-quality", goipp.TagEnum, goipp.Integer(4)),
	}
	cases := []struct {
		name     string
		fidelity *bool
		attrs    []goipp.Attribute
		want     goipp.Status
	}{
		{"ios roll", nil, iosAttrs, goipp.StatusOk},
		{"ios roll fidelity", &yes, iosAttrs, goipp.StatusOk},
		{"ios roll no fidelity", &no, iosAttrs, goipp.StatusOk},
		{"width -1mm", &yes, []goipp.Attribute{rollMediaCol(6100, 5000)}, goipp.StatusOk},
		{"width +1mm", &yes, []goipp.Attribute{rollMediaCol(6300, 5000)}, goipp.StatusOk},
		{"min length", &yes, []goipp.Attribute{rollMediaCol(6200, 800)}, goipp.StatusOk},
		{"max length", &yes, []goipp.Attribute{rollMediaCol(6200, 100000)}, goipp.StatusOk},
		{"preset", &yes, []goipp.Attribute{rollMediaCol(6200, 2900)}, goipp.StatusOk},
		{"too short", &yes, []goipp.Attribute{rollMediaCol(6200, 700)}, goipp.StatusErrorAttributesOrValues},
		{"too long", &no, []goipp.Attribute{rollMediaCol(6200, 100001)}, goipp.StatusOkIgnoredOrSubstituted},
		{"a4 fidelity", &yes, []goipp.Attribute{rollMediaCol(21000, 29700)}, goipp.StatusErrorAttributesOrValues},
		{"a4 no fidelity", &no, []goipp.Attribute{rollMediaCol(21000, 29700)}, goipp.StatusOkIgnoredOrSubstituted},
		{"a4 default fidelity", nil, []goipp.Attribute{rollMediaCol(21000, 29700)}, goipp.StatusOkIgnoredOrSubstituted},
		{"29mm wide", &yes, []goipp.Attribute{rollMediaCol(2900, 5000)}, goipp.StatusErrorAttributesOrValues},
		{"media preset", &yes, []goipp.Attribute{media("om_62x29mm_62x29mm")}, goipp.StatusOk},
		{"media roll", &yes, []goipp.Attribute{media("roll_current_62x0mm")}, goipp.StatusOk},
		{"media custom", &yes, []goipp.Attribute{media("custom_62x40mm_62x40mm")}, goipp.StatusOk},
		{"media a4", &yes, []goipp.Attribute{media("iso_a4_210x297mm")}, goipp.StatusErrorAttributesOrValues},
		{"media a4 no fidelity", &no, []goipp.Attribute{media("iso_a4_210x297mm")}, goipp.StatusOkIgnoredOrSubstituted},
		{"copies 2", &no, []goipp.Attribute{goipp.MakeAttribute("copies", goipp.TagInteger, goipp.Integer(2))}, goipp.StatusOkIgnoredOrSubstituted},
		{"high quality", &yes, []goipp.Attribute{goipp.MakeAttribute("print-quality", goipp.TagEnum, goipp.Integer(5))}, goipp.StatusErrorAttributesOrValues},
		{"color", &no, []goipp.Attribute{goipp.MakeAttribute("print-color-mode", goipp.TagKeyword, goipp.String("color"))}, goipp.StatusOkIgnoredOrSubstituted},
		{"unknown attribute", &yes, []goipp.Attribute{goipp.MakeAttribute("print-content-optimize", goipp.TagKeyword, goipp.String("text"))}, goipp.StatusOk},
	}
	for _, c := range cases {
		resp := validateJob(t, ts, c.fidelity, c.attrs...)
		if st := status(resp); st != c.want {
			t.Errorf("%s: status %v, want %v", c.name, st, c.want)
			continue
		}
		if c.want != goipp.StatusOk {
			if len(resp.Unsupported) == 0 {
				t.Errorf("%s: no unsupported group", c.name)
			}
			if g := resp.Groups; len(g) < 2 || g[1].Tag != goipp.TagUnsupportedGroup {
				t.Errorf("%s: unsupported group must follow the operation group", c.name)
			}
		} else if len(resp.Unsupported) != 0 {
			t.Errorf("%s: unexpected unsupported %v", c.name, resp.Unsupported)
		}
	}

	m := opRequest(goipp.OpValidateJob, ts.uri("ipp"),
		goipp.MakeAttribute("document-format", goipp.TagMimeType, goipp.String("application/pdf")))
	resp := ts.do(t, "ipp", m, nil)
	if st := status(resp); st != goipp.StatusErrorDocumentFormatNotSupported {
		t.Errorf("pdf: %v", st)
	}
	m = opRequest(goipp.OpValidateJob, ts.uri("ipp"),
		goipp.MakeAttribute("compression", goipp.TagKeyword, goipp.String("gzip")))
	if st := status(ts.do(t, "ipp", m, nil)); st != goipp.StatusErrorCompressionNotSupported {
		t.Errorf("gzip: %v", st)
	}
}

func TestValidateJobHighRes(t *testing.T) {
	cfg := testConfig()
	cfg.Caps = qlCaps(func(o *brotherql.Options) { o.HighRes = true })
	ts := startServer(t, cfg, (&recorder{}).process)
	yes := true
	ok := []goipp.Attribute{
		goipp.MakeAttribute("print-quality", goipp.TagEnum, goipp.Integer(5)),
		goipp.MakeAttribute("printer-resolution", goipp.TagResolution, goipp.Resolution{Xres: 600, Yres: 600, Units: goipp.UnitsDpi}),
	}
	if st := status(validateJob(t, ts, &yes, ok...)); st != goipp.StatusOk {
		t.Errorf("high quality with high-res: %v", st)
	}
	bad := goipp.MakeAttribute("printer-resolution", goipp.TagResolution, goipp.Resolution{Xres: 1200, Yres: 1200, Units: goipp.UnitsDpi})
	if st := status(validateJob(t, ts, &yes, bad)); st != goipp.StatusErrorAttributesOrValues {
		t.Errorf("1200 dpi: %v", st)
	}
}

func getJob(t *testing.T, ts *testServer, id int, ra ...string) goipp.Attributes {
	t.Helper()
	extra := []goipp.Attribute{jobIDAttr(id)}
	if ra != nil {
		extra = append(extra, requestedAttrs(ra...))
	}
	resp := ts.do(t, "ipp", opRequest(goipp.OpGetJobAttributes, ts.uri("ipp"), extra...), nil)
	if status(resp) != goipp.StatusOk {
		t.Fatalf("Get-Job-Attributes %d: %v", id, status(resp))
	}
	return resp.Job
}

func printJob(t *testing.T, ts *testServer, format string, doc []byte, jobAttrs ...goipp.Attribute) *goipp.Message {
	t.Helper()
	m := opRequest(goipp.OpPrintJob, ts.uri("ipp"),
		goipp.MakeAttribute("job-name", goipp.TagName, goipp.String("label")))
	if format != "" {
		m.Operation.Add(goipp.MakeAttribute("document-format", goipp.TagMimeType, goipp.String(format)))
	}
	m.Job = jobAttrs
	return ts.do(t, "ipp", m, doc)
}

func TestPrintJob(t *testing.T) {
	rec := &recorder{}
	ts := startServer(t, testConfig(), rec.process)
	urf := tinyURF(732, 30)
	resp := printJob(t, ts, formatURF, urf, rollMediaCol(6200, 1670),
		goipp.MakeAttribute("print-quality", goipp.TagEnum, goipp.Integer(4)))
	if status(resp) != goipp.StatusOk {
		t.Fatalf("Print-Job: %v %v", status(resp), resp.Operation)
	}
	if got := strings.Join(names(resp.Job), ","); got != "job-id,job-uri,job-state,job-state-message,job-state-reasons" {
		t.Errorf("Print-Job job attributes %s", got)
	}
	id := intOf(t, resp.Job, "job-id")
	if u := strOf(t, resp.Job, "job-uri"); u != ts.uri("ipp")+"/"+itoa(id) {
		t.Errorf("job-uri %s", u)
	}
	waitJobState(t, ts, id, JobCompleted)
	attrs := getJob(t, ts, id)
	if intOf(t, attrs, "job-impressions-completed") != 1 || intOf(t, attrs, "job-state") != int(JobCompleted) {
		t.Errorf("job attrs %v", attrs)
	}
	if r := strOf(t, attrs, "job-state-reasons"); r != "job-completed-successfully" {
		t.Errorf("reasons %s", r)
	}
	for _, n := range []string{"time-at-completed", "date-time-at-completed", "job-originating-user-name", "media-col", "job-uuid"} {
		if _, ok := attrOf(attrs, n); !ok {
			t.Errorf("missing %s", n)
		}
	}
	if !bytes.Equal(rec.docs[0], urf) || rec.formats[0] != formatURF {
		t.Errorf("processor got %d bytes %s", len(rec.docs[0]), rec.formats[0])
	}
	j := rec.jobs[0]
	if j.Name != "label" || j.User != "tester" || j.Attrs.MediaCol == nil ||
		j.Attrs.MediaCol.XDimension != 6200 || j.Attrs.MediaCol.YDimension != 1670 || j.Attrs.PrintQuality != 4 {
		t.Errorf("job %+v %+v", j, j.Attrs.MediaCol)
	}
	small := getJob(t, ts, id, "job-state", "job-state-reasons")
	if got := strings.Join(names(small), ","); got != "job-state,job-state-reasons" {
		t.Errorf("requested subset %s", got)
	}
}

func TestPrintJobSniffing(t *testing.T) {
	rec := &recorder{}
	ts := startServer(t, testConfig(), rec.process)
	pwg := append([]byte("RaS2"), make([]byte, 1796)...)
	for _, c := range []struct {
		format string
		doc    []byte
		want   string
	}{
		{formatOctetStream, tinyURF(732, 4), formatURF},
		{"", tinyURF(732, 4), formatURF},
		{formatOctetStream, pwg, formatPWG},
		{formatURF, pwg, formatPWG},
	} {
		resp := printJob(t, ts, c.format, c.doc)
		if status(resp) != goipp.StatusOk {
			t.Fatalf("%s: %v", c.format, status(resp))
		}
		waitJobState(t, ts, intOf(t, resp.Job, "job-id"), JobCompleted)
		if got := rec.formats[len(rec.formats)-1]; got != c.want {
			t.Errorf("declared %q: format %s, want %s", c.format, got, c.want)
		}
		if f := strOf(t, getJob(t, ts, intOf(t, resp.Job, "job-id")), "document-format-supplied"); f != c.want {
			t.Errorf("document-format-supplied %s", f)
		}
	}
	resp := printJob(t, ts, formatOctetStream, []byte("%PDF-1.7 hello"))
	if status(resp) != goipp.StatusErrorDocumentFormatNotSupported || len(resp.Unsupported) == 0 {
		t.Errorf("pdf as octet-stream: %v", status(resp))
	}
	resp = printJob(t, ts, formatURF, nil)
	if status(resp) != goipp.StatusErrorBadRequest {
		t.Errorf("empty document: %v", status(resp))
	}
}

func TestPrintJobTooLarge(t *testing.T) {
	cfg := testConfig()
	cfg.MaxDocumentBytes = 1000
	ts := startServer(t, cfg, (&recorder{}).process)
	if st := status(printJob(t, ts, formatURF, tinyURF(732, 200))); st != goipp.StatusErrorRequestEntity {
		t.Errorf("status %v", st)
	}
}

func createJob(t *testing.T, ts *testServer, scheme string, jobAttrs ...goipp.Attribute) int {
	t.Helper()
	m := opRequest(goipp.OpCreateJob, ts.uri(scheme),
		goipp.MakeAttribute("job-name", goipp.TagName, goipp.String("Labels #1")))
	m.Job = jobAttrs
	resp := ts.do(t, scheme, m, nil)
	if status(resp) != goipp.StatusOk {
		t.Fatalf("Create-Job: %v %v", status(resp), resp.Operation)
	}
	if st := intOf(t, resp.Job, "job-state"); st != int(JobPendingHeld) {
		t.Errorf("Create-Job state %d", st)
	}
	if r := strOf(t, resp.Job, "job-state-reasons"); r != "job-data-insufficient" {
		t.Errorf("Create-Job reasons %s", r)
	}
	return intOf(t, resp.Job, "job-id")
}

func sendDocument(t *testing.T, ts *testServer, id int, last bool, format string, doc []byte) *goipp.Message {
	t.Helper()
	m := opRequest(goipp.OpSendDocument, ts.uri("ipps"), jobIDAttr(id),
		goipp.MakeAttribute("last-document", goipp.TagBoolean, goipp.Boolean(last)))
	if format != "" {
		m.Operation.Add(goipp.MakeAttribute("document-format", goipp.TagMimeType, goipp.String(format)))
	}
	return ts.do(t, "ipps", m, doc)
}

func TestCreateJobSendDocument(t *testing.T) {
	rec := &recorder{}
	ts := startServer(t, testConfig(), rec.process)
	id := createJob(t, ts, "ipps", rollMediaCol(6200, 1670))
	resp := sendDocument(t, ts, id, true, formatURF, tinyURF(732, 20))
	if status(resp) != goipp.StatusOk {
		t.Fatalf("Send-Document: %v %v", status(resp), resp.Operation)
	}
	if u := strOf(t, resp.Job, "job-uri"); !strings.HasPrefix(u, "ipps://") {
		t.Errorf("job-uri %s", u)
	}
	waitJobState(t, ts, id, JobCompleted)
	if len(rec.docs) != 1 || rec.formats[0] != formatURF {
		t.Fatalf("processor calls %d", len(rec.docs))
	}
	if st := status(sendDocument(t, ts, id, true, formatURF, tinyURF(732, 2))); st != goipp.StatusErrorNotPossible {
		t.Errorf("Send-Document to completed job: %v", st)
	}

	// last-document=false, then an empty last Send-Document.
	id = createJob(t, ts, "ipp")
	if st := status(sendDocument(t, ts, id, false, "", tinyURF(732, 2))); st != goipp.StatusOk {
		t.Fatalf("first Send-Document: %v", st)
	}
	if st := status(sendDocument(t, ts, id, false, "", tinyURF(732, 2))); st != goipp.StatusErrorMultipleJobsNotSupported {
		t.Errorf("second document: %v", st)
	}
	if st := status(sendDocument(t, ts, id, true, "", nil)); st != goipp.StatusOk {
		t.Fatalf("closing Send-Document: %v", st)
	}
	waitJobState(t, ts, id, JobCompleted)

	// last-document=false, then Close-Job.
	id = createJob(t, ts, "ipp")
	sendDocument(t, ts, id, false, formatOctetStream, tinyURF(732, 2))
	close := func(id int) goipp.Status {
		return status(ts.do(t, "ipp", opRequest(goipp.OpCloseJob, ts.uri("ipp"), jobIDAttr(id)), nil))
	}
	if st := close(id); st != goipp.StatusOk {
		t.Fatalf("Close-Job: %v", st)
	}
	waitJobState(t, ts, id, JobCompleted)
	if st := close(id); st != goipp.StatusErrorNotPossible {
		t.Errorf("Close-Job twice: %v", st)
	}

	// Close-Job without a document aborts.
	id = createJob(t, ts, "ipp")
	if st := close(id); st != goipp.StatusOk {
		t.Fatalf("Close-Job empty: %v", st)
	}
	waitJobState(t, ts, id, JobAborted)

	if st := status(sendDocument(t, ts, 999, true, formatURF, tinyURF(732, 2))); st != goipp.StatusErrorNotFound {
		t.Errorf("unknown job: %v", st)
	}

	// Unrecognised data aborts the created job.
	id = createJob(t, ts, "ipp")
	if st := status(sendDocument(t, ts, id, true, formatOctetStream, []byte("garbage!"))); st != goipp.StatusErrorDocumentFormatNotSupported {
		t.Errorf("garbage: %v", st)
	}
	waitJobState(t, ts, id, JobAborted)
	if len(rec.docs) != 3 {
		t.Errorf("processor calls %d, want 3", len(rec.docs))
	}
}

func TestCreateJobTimeout(t *testing.T) {
	old := multipleOperationTimeout
	multipleOperationTimeout = 100 * time.Millisecond
	defer func() { multipleOperationTimeout = old }()
	ts := startServer(t, testConfig(), (&recorder{}).process)
	id := createJob(t, ts, "ipp")
	waitJobState(t, ts, id, JobAborted)
	if st := status(sendDocument(t, ts, id, true, formatURF, tinyURF(732, 2))); st != goipp.StatusErrorNotPossible {
		t.Errorf("Send-Document after timeout: %v", st)
	}
}

func TestJobFIFOAndCancel(t *testing.T) {
	rec := &recorder{hold: make(chan struct{}), started: make(chan int, 10)}
	ts := startServer(t, testConfig(), rec.process)
	var ids []int
	for i := 0; i < 3; i++ {
		resp := printJob(t, ts, formatURF, tinyURF(732, 2))
		ids = append(ids, intOf(t, resp.Job, "job-id"))
	}
	if got := <-rec.started; got != ids[0] {
		t.Fatalf("first started job %d, want %d", got, ids[0])
	}
	waitJobState(t, ts, ids[0], JobProcessing)
	if st := ts.findJob(ids[1]).State(); st != JobPending {
		t.Errorf("second job %s while first prints", st)
	}

	pa := ts.do(t, "ipp", opRequest(goipp.OpGetPrinterAttributes, ts.uri("ipp"),
		requestedAttrs("printer-state", "queued-job-count")), nil)
	if intOf(t, pa.Printer, "printer-state") != int(StateProcessing) || intOf(t, pa.Printer, "queued-job-count") != 3 {
		t.Errorf("printer while printing: %v", pa.Printer)
	}

	cancel := func(id int) goipp.Status {
		return status(ts.do(t, "ipp", opRequest(goipp.OpCancelJob, ts.uri("ipp"), jobIDAttr(id)), nil))
	}
	if st := cancel(ids[1]); st != goipp.StatusOk {
		t.Fatalf("cancel pending: %v", st)
	}
	waitJobState(t, ts, ids[1], JobCanceled)
	if st := cancel(ids[0]); st != goipp.StatusOk {
		t.Fatalf("cancel processing: %v", st)
	}
	waitJobState(t, ts, ids[0], JobCanceled)
	if got := <-rec.started; got != ids[2] {
		t.Fatalf("next started job %d, want %d", got, ids[2])
	}
	rec.hold <- struct{}{}
	waitJobState(t, ts, ids[2], JobCompleted)
	if st := cancel(ids[2]); st != goipp.StatusErrorNotPossible {
		t.Errorf("cancel completed: %v", st)
	}
	if st := cancel(12345); st != goipp.StatusErrorNotFound {
		t.Errorf("cancel unknown: %v", st)
	}
	resp := ts.do(t, "ipp", opRequest(goipp.OpCancelJob, ts.uri("ipp"),
		goipp.MakeAttribute("job-uri", goipp.TagURI, goipp.String(ts.uri("ipp")+"/"+itoa(ids[2])))), nil)
	if status(resp) != goipp.StatusErrorNotPossible {
		t.Errorf("cancel by job-uri: %v", status(resp))
	}
	if len(rec.docs) != 2 {
		t.Errorf("processor ran %d jobs, want 2", len(rec.docs))
	}
}

func TestGetJobs(t *testing.T) {
	rec := &recorder{hold: make(chan struct{}), started: make(chan int, 10)}
	ts := startServer(t, testConfig(), rec.process)
	done := printJob(t, ts, formatURF, tinyURF(732, 2))
	<-rec.started
	rec.hold <- struct{}{}
	doneID := intOf(t, done.Job, "job-id")
	waitJobState(t, ts, doneID, JobCompleted)
	active := intOf(t, printJob(t, ts, formatURF, tinyURF(732, 2)).Job, "job-id")
	<-rec.started
	other := opRequest(goipp.OpCreateJob, ts.uri("ipp"))
	other.Operation[3] = goipp.MakeAttribute("requesting-user-name", goipp.TagName, goipp.String("someone"))
	otherID := intOf(t, ts.do(t, "ipp", other, nil).Job, "job-id")

	getJobs := func(extra ...goipp.Attribute) []goipp.Group {
		t.Helper()
		resp := ts.do(t, "ipp", opRequest(goipp.OpGetJobs, ts.uri("ipp"), extra...), nil)
		if status(resp) != goipp.StatusOk {
			t.Fatalf("Get-Jobs: %v", status(resp))
		}
		var out []goipp.Group
		for _, g := range resp.Groups {
			if g.Tag == goipp.TagJobGroup {
				out = append(out, g)
			}
		}
		return out
	}
	ids := func(gs []goipp.Group) string {
		var s []string
		for _, g := range gs {
			s = append(s, itoa(intOf(t, g.Attrs, "job-id")))
		}
		return strings.Join(s, ",")
	}
	which := func(w string) goipp.Attribute {
		return goipp.MakeAttribute("which-jobs", goipp.TagKeyword, goipp.String(w))
	}

	def := getJobs()
	if got := ids(def); got != fmt.Sprintf("%d,%d", active, otherID) {
		t.Errorf("not-completed: %s", got)
	}
	if got := strings.Join(names(def[0].Attrs), ","); got != "job-id,job-uri" {
		t.Errorf("default attributes %s", got)
	}
	if got := ids(getJobs(which("completed"))); got != itoa(doneID) {
		t.Errorf("completed: %s", got)
	}
	if got := ids(getJobs(which("all"))); got != fmt.Sprintf("%d,%d,%d", otherID, active, doneID) {
		t.Errorf("all: %s", got)
	}
	if got := ids(getJobs(which("all"), goipp.MakeAttribute("limit", goipp.TagInteger, goipp.Integer(1)))); got != itoa(otherID) {
		t.Errorf("limit: %s", got)
	}
	if got := ids(getJobs(which("all"), goipp.MakeAttribute("my-jobs", goipp.TagBoolean, goipp.Boolean(true)))); got != fmt.Sprintf("%d,%d", active, doneID) {
		t.Errorf("my-jobs: %s", got)
	}
	if got := ids(getJobs(which("pending-held"))); got != itoa(otherID) {
		t.Errorf("pending-held: %s", got)
	}
	full := getJobs(which("completed"), requestedAttrs("all"))
	if _, ok := attrOf(full[0].Attrs, "job-impressions-completed"); !ok {
		t.Errorf("requested all: %v", names(full[0].Attrs))
	}
	resp := ts.do(t, "ipp", opRequest(goipp.OpGetJobs, ts.uri("ipp"), which("fetchable")), nil)
	if status(resp) != goipp.StatusErrorAttributesOrValues {
		t.Errorf("which-jobs fetchable: %v", status(resp))
	}

	// Cancel-My-Jobs cancels only the requester's jobs.
	resp = ts.do(t, "ipp", opRequest(goipp.OpCancelMyJobs, ts.uri("ipp")), nil)
	if status(resp) != goipp.StatusOk {
		t.Fatalf("Cancel-My-Jobs: %v", status(resp))
	}
	waitJobState(t, ts, active, JobCanceled)
	if st := ts.findJob(otherID).State(); st != JobPendingHeld {
		t.Errorf("other user's job %s", st)
	}
	m := opRequest(goipp.OpCancelMyJobs, ts.uri("ipp"),
		goipp.MakeAttribute("job-ids", goipp.TagInteger, goipp.Integer(otherID)))
	if st := status(ts.do(t, "ipp", m, nil)); st != goipp.StatusErrorNotFound {
		t.Errorf("Cancel-My-Jobs on another user's job: %v", st)
	}
}

func TestProcessorErrors(t *testing.T) {
	rec := &recorder{err: fmt.Errorf("label: %w", ErrDocumentFormat)}
	ts := startServer(t, testConfig(), rec.process)
	id := intOf(t, printJob(t, ts, formatURF, tinyURF(732, 2)).Job, "job-id")
	waitJobState(t, ts, id, JobAborted)
	if r := strOf(t, getJob(t, ts, id), "job-state-reasons"); r != "document-format-error" {
		t.Errorf("reasons %s", r)
	}

	rec.mu.Lock()
	rec.err = errors.New("printer unplugged")
	rec.mu.Unlock()
	id = intOf(t, printJob(t, ts, formatURF, tinyURF(732, 2)).Job, "job-id")
	waitJobState(t, ts, id, JobAborted)
	attrs := getJob(t, ts, id)
	if strOf(t, attrs, "job-state-reasons") != "aborted-by-system" || strOf(t, attrs, "job-state-message") != "printer unplugged" {
		t.Errorf("aborted job %v", attrs)
	}

	panicky := startServer(t, testConfig(), func(context.Context, *Job, io.Reader, string) error { panic("boom") })
	id = intOf(t, printJob(t, panicky, formatURF, tinyURF(732, 2)).Job, "job-id")
	waitJobState(t, panicky, id, JobAborted)
}

func TestJobRetention(t *testing.T) {
	ts := startServer(t, testConfig(), (&recorder{}).process)
	var last int
	for i := 0; i < maxRetainedJobs+5; i++ {
		last = intOf(t, printJob(t, ts, formatURF, tinyURF(732, 1)).Job, "job-id")
		waitJobState(t, ts, last, JobCompleted)
	}
	jobs := ts.Jobs()
	if len(jobs) != maxRetainedJobs || jobs[0].ID != last || jobs[len(jobs)-1].ID != last-maxRetainedJobs+1 {
		t.Errorf("retained %d jobs, newest %d oldest %d", len(jobs), jobs[0].ID, jobs[len(jobs)-1].ID)
	}
	if st := status(ts.do(t, "ipp", opRequest(goipp.OpGetJobAttributes, ts.uri("ipp"), jobIDAttr(1)), nil)); st != goipp.StatusErrorNotFound {
		t.Errorf("pruned job: %v", st)
	}
}

func TestIdentifyPrinter(t *testing.T) {
	cfg := testConfig()
	got := make(chan []string, 1)
	cfg.Identify = func(actions []string, msg string) { got <- actions }
	ts := startServer(t, cfg, (&recorder{}).process)
	acts := goipp.Attribute{Name: "identify-actions"}
	acts.Values.Add(goipp.TagKeyword, goipp.String("display"))
	acts.Values.Add(goipp.TagKeyword, goipp.String("sound"))
	if st := status(ts.do(t, "ipp", opRequest(goipp.OpIdentifyPrinter, ts.uri("ipp"), acts), nil)); st != goipp.StatusOk {
		t.Fatalf("Identify-Printer: %v", st)
	}
	select {
	case a := <-got:
		if strings.Join(a, ",") != "display,sound" {
			t.Errorf("actions %v", a)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Identify callback not called")
	}
	bad := goipp.MakeAttribute("identify-actions", goipp.TagKeyword, goipp.String("flash"))
	if st := status(ts.do(t, "ipp", opRequest(goipp.OpIdentifyPrinter, ts.uri("ipp"), bad), nil)); st != goipp.StatusErrorAttributesOrValues {
		t.Errorf("flash: %v", st)
	}
}

func TestDeviceStatus(t *testing.T) {
	cfg := testConfig()
	var mu sync.Mutex
	dev := DeviceStatus{}
	cfg.Status = func(context.Context) DeviceStatus { mu.Lock(); defer mu.Unlock(); return dev }
	ts := startServer(t, cfg, (&recorder{}).process)
	mu.Lock()
	dev = DeviceStatus{State: StateStopped, Reasons: []string{"media-empty-error"}, Message: "No labels."}
	mu.Unlock()
	ts.kickStatus()
	deadline := time.Now().Add(5 * time.Second)
	for {
		pa := ts.do(t, "ipp", opRequest(goipp.OpGetPrinterAttributes, ts.uri("ipp"), requestedAttrs("printer-state", "printer-state-reasons", "printer-state-message")), nil)
		if intOf(t, pa.Printer, "printer-state") == int(StateStopped) {
			if r := strOf(t, pa.Printer, "printer-state-reasons"); r != "media-empty-error" {
				t.Errorf("reasons %s", r)
			}
			if m := strOf(t, pa.Printer, "printer-state-message"); m != "No labels." {
				t.Errorf("message %s", m)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("printer never stopped")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestHTTP(t *testing.T) {
	ts := startServer(t, testConfig(), (&recorder{}).process)
	printJob(t, ts, formatURF, tinyURF(732, 2))
	for path, want := range map[string]string{
		"/":            "Test Label Printer",
		"/supplies":    "Test Label Printer",
		"/en.strings":  "media-type.roll",
		"/icon.png":    "\x89PNG",
		"/icon-sm.png": "\x89PNG",
		"/icon-lg.png": "\x89PNG",
	} {
		for _, scheme := range []string{"http", "https"} {
			resp, err := ts.client.Get(scheme + "://" + ts.addr + path)
			if err != nil {
				t.Fatal(err)
			}
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != 200 || !strings.Contains(string(b), want) {
				t.Errorf("%s %s: %d %.60q", scheme, path, resp.StatusCode, b)
			}
		}
	}
	resp, err := ts.client.Get("http://" + ts.addr + "/nope")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Errorf("/nope: %d", resp.StatusCode)
	}
}

// Chunked bodies and Expect: 100-continue are what CUPS and iOS send for documents.
func TestChunkedExpectContinue(t *testing.T) {
	rec := &recorder{}
	ts := startServer(t, testConfig(), rec.process)
	m := opRequest(goipp.OpPrintJob, ts.uri("ipp"),
		goipp.MakeAttribute("document-format", goipp.TagMimeType, goipp.String(formatURF)))
	hdr, _ := m.EncodeBytes()
	urf := tinyURF(732, 50)
	pr, pw := io.Pipe()
	go func() {
		pw.Write(hdr)
		pw.Write(urf[:100])
		pw.Write(urf[100:])
		pw.Close()
	}()
	req, _ := http.NewRequest("POST", "https://"+ts.addr+printerPath, pr)
	req.Header.Set("Content-Type", "application/ipp")
	req.Header.Set("Expect", "100-continue")
	tr := ts.client.Transport.(*http.Transport)
	tr.ExpectContinueTimeout = 5 * time.Second
	resp, err := ts.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out goipp.Message
	if err := out.Decode(resp.Body); err != nil || goipp.Status(out.Code) != goipp.StatusOk {
		t.Fatalf("chunked Print-Job: %v %v", err, goipp.Status(out.Code))
	}
	waitJobState(t, ts, intOf(t, out.Job, "job-id"), JobCompleted)
	if !bytes.Equal(rec.docs[0], urf) {
		t.Errorf("document %d bytes, want %d", len(rec.docs[0]), len(urf))
	}
}

func TestCertPersisted(t *testing.T) {
	dir := t.TempDir()
	a, err := LoadOrCreateCert(dir)
	if err != nil {
		t.Fatal(err)
	}
	b, err := LoadOrCreateCert(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a.Certificate[0], b.Certificate[0]) {
		t.Error("certificate regenerated")
	}
	leaf := b.Leaf
	if leaf == nil || len(leaf.DNSNames) == 0 || leaf.NotAfter.Before(time.Now().AddDate(5, 0, 0)) {
		t.Errorf("leaf %+v", leaf)
	}
	if err := leaf.VerifyHostname("localhost"); err != nil {
		t.Error(err)
	}
}

func TestTXT(t *testing.T) {
	cfg := testConfig()
	txt := cfg.TXT("printer.local", 8631)
	want := map[string]string{
		"rp": "ipp/print", "ty": cfg.Caps.MakeModel, "adminurl": "https://printer.local:8631/", "note": cfg.Location,
		"pdl": "image/pwg-raster,image/urf", "Color": "F", "Duplex": "F", "UUID": "00000000-1111-2222-3333-444455556666",
		"TLS": "1.2", "URF": "V1.4,W8,CP1,RS300", "txtvers": "1", "qtotal": "1",
	}
	for k, v := range want {
		if txt[k] != v {
			t.Errorf("TXT %s = %q, want %q", k, txt[k], v)
		}
	}
	if len(txt) != len(want) {
		t.Errorf("TXT has %d keys, want %d", len(txt), len(want))
	}
}

func TestConfigValidation(t *testing.T) {
	for name, mut := range map[string]func(*Config){
		"no name":   func(c *Config) { c.Name = "" },
		"no uuid":   func(c *Config) { c.UUID = "" },
		"no width":  func(c *Config) { c.Caps.Roll.Width = 0 },
		"no media":  func(c *Config) { c.Caps.Roll, c.Caps.Media = nil, nil },
		"bad range": func(c *Config) { c.Caps.Roll.MinLength, c.Caps.Roll.MaxLength = 500, 400 },
	} {
		c := testConfig()
		mut(&c)
		if _, err := New(c, (&recorder{}).process); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	if _, err := New(testConfig(), nil); err == nil {
		t.Error("nil processor accepted")
	}
}

func TestMediaPolicyPresets(t *testing.T) {
	cfg := testConfig()
	cfg.Caps = qlCaps(func(o *brotherql.Options) { o.MediaPolicy = brotherql.PolicyRollPresets })
	ts := startServer(t, cfg, (&recorder{}).process)
	pa := ts.do(t, "ipp", opRequest(goipp.OpGetPrinterAttributes, ts.uri("ipp"), requestedAttrs("media-ready", "media-col-ready")), nil)
	ready, _ := attrOf(pa.Printer, "media-ready")
	if got := ready.Values.String(); got != "[roll_current_62x0mm,om_62x29mm_62x29mm,om_62x50mm_62x50mm]" {
		t.Errorf("media-ready %s", got)
	}
	colReady, _ := attrOf(pa.Printer, "media-col-ready")
	if len(colReady.Values) != 3 {
		t.Errorf("media-col-ready has %d values", len(colReady.Values))
	}
}

func TestStatusNotPolledDuringJob(t *testing.T) {
	cfg := testConfig()
	var mu sync.Mutex
	printing, overlap := false, false
	cfg.Status = func(context.Context) DeviceStatus {
		mu.Lock()
		defer mu.Unlock()
		if printing {
			overlap = true
		}
		return DeviceStatus{}
	}
	proc := func(ctx context.Context, j *Job, doc io.Reader, format string) error {
		mu.Lock()
		printing = true
		mu.Unlock()
		time.Sleep(3 * statusPollInterval / 2)
		mu.Lock()
		printing = false
		mu.Unlock()
		return nil
	}
	ts := startServer(t, cfg, proc)
	id := intOf(t, printJob(t, ts, formatURF, tinyURF(732, 2)).Job, "job-id")
	ts.kickStatus()
	waitJobState(t, ts, id, JobCompleted)
	mu.Lock()
	defer mu.Unlock()
	if overlap {
		t.Error("device status polled while a job was printing")
	}
}
