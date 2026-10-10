package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/OpenPrinting/goipp"

	"github.com/leandroalbero/airprint-usb-bridge/internal/ql"
	"github.com/leandroalbero/airprint-usb-bridge/internal/raster/testutil"
)

// rect is a black rectangle on a synthetic page, in page pixels (x1, y1 exclusive).
type rect struct{ x0, y0, x1, y1 int }

const pageW = 732 // a 62 mm roll rasterised at 300 dpi

func grayPage(w, h int, rects ...rect) []byte {
	g := bytes.Repeat([]byte{0xff}, w*h)
	for _, r := range rects {
		for y := r.y0; y < r.y1; y++ {
			for x := r.x0; x < r.x1; x++ {
				g[y*w+x] = 0
			}
		}
	}
	return g
}

// expectedLabel is worked out independently of internal/raster: centre-crop the page to
// 696 pins ((732-696)/2 = 18 columns off each side) and drop every blank row at both ends.
func expectedLabel(t *testing.T, w, h int, rects ...rect) *ql.Bitmap {
	t.Helper()
	const pins, off = 696, (pageW - 696) / 2
	g := grayPage(w, h, rects...)
	top, bottom := -1, -1
	for y := 0; y < h; y++ {
		for x := off; x < off+pins; x++ {
			if g[y*w+x] == 0 {
				if top < 0 {
					top = y
				}
				bottom = y
				break
			}
		}
	}
	if top < 0 {
		t.Fatal("expected label is blank")
	}
	b := ql.NewBitmap(pins, bottom-top+1)
	for y := top; y <= bottom; y++ {
		for x := 0; x < pins; x++ {
			b.Bits[(y-top)*pins+x] = g[y*w+off+x] == 0
		}
	}
	return b
}

// labelBytes encodes one 62 mm label independently of the code under test: left-aligned,
// padded to the print pins, auto-cut at the end, margin 0.
func labelBytes(t *testing.T, b *ql.Bitmap) []byte {
	t.Helper()
	prof, err := ql.EmbeddedProfile()
	if err != nil {
		t.Fatal(err)
	}
	media, ok := prof.MediaByID("62")
	if !ok {
		t.Fatal("no 62 mm media")
	}
	o := ql.DefaultJobOptions()
	o.MarginDots = 0
	o.AutoCut = true
	o.CutEvery = 1
	o.CutAtEnd = true
	o.HighRes = false
	o.ValidateMedia = true
	out, err := ql.NewEncoder().Encode(prof, ql.Job{Media: media, Options: o, Pages: []*ql.Bitmap{b}})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func defaultOptions(t *testing.T) options {
	t.Helper()
	var o options
	if err := newFlagSet(&o, io.Discard).Parse(nil); err != nil {
		t.Fatal(err)
	}
	return o
}

type testService struct {
	*service
	addr   string
	dryDir string
	client *http.Client
}

func startService(t *testing.T, mods ...func(*options)) *testService {
	t.Helper()
	o := defaultOptions(t)
	o.name = "Test Label Printer"
	o.dryRun = t.TempDir()
	o.stateDir = t.TempDir()
	o.mdns = "off"
	o.verbose = true
	for _, m := range mods {
		m(&o)
	}
	svc, err := newService(&o, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan struct{})
	go func() {
		defer close(served)
		_ = svc.srv.Serve(l)
	}()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := svc.srv.Shutdown(ctx); err != nil {
			t.Errorf("shutdown: %v", err)
		}
		<-served
		svc.p.send.Close()
	})
	return &testService{
		service: svc,
		addr:    l.Addr().String(),
		dryDir:  o.dryRun,
		client: &http.Client{
			Timeout:   30 * time.Second,
			Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
		},
	}
}

func (ts *testService) request(op goipp.Op, scheme string, extra ...goipp.Attribute) *goipp.Message {
	m := goipp.NewRequest(goipp.MakeVersion(2, 0), op, uint32(time.Now().UnixNano()&0x7fffffff))
	m.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	m.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en")))
	m.Operation.Add(goipp.MakeAttribute("printer-uri", goipp.TagURI, goipp.String(scheme+"://"+ts.addr+"/ipp/print")))
	m.Operation.Add(goipp.MakeAttribute("requesting-user-name", goipp.TagName, goipp.String("tester")))
	for _, a := range extra {
		m.Operation.Add(a)
	}
	return m
}

func (ts *testService) do(t *testing.T, scheme string, m *goipp.Message, doc []byte) *goipp.Message {
	t.Helper()
	b, err := m.EncodeBytes()
	if err != nil {
		t.Fatal(err)
	}
	httpScheme := map[string]string{"ipp": "http", "ipps": "https"}[scheme]
	resp, err := ts.client.Post(httpScheme+"://"+ts.addr+"/ipp/print", "application/ipp", io.MultiReader(bytes.NewReader(b), bytes.NewReader(doc)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("HTTP %d", resp.StatusCode)
	}
	if scheme == "ipps" && resp.TLS == nil {
		t.Fatal("ipps request was not served over TLS")
	}
	var out goipp.Message
	if err := out.Decode(resp.Body); err != nil {
		t.Fatal(err)
	}
	if code := goipp.Status(out.Code); code != goipp.StatusOk {
		t.Fatalf("%s: %s %v", goipp.Op(m.Code), code, out.Operation)
	}
	return &out
}

func attrInt(t *testing.T, attrs goipp.Attributes, name string) int {
	t.Helper()
	for _, a := range attrs {
		if a.Name == name {
			return int(a.Values[0].V.(goipp.Integer))
		}
	}
	t.Fatalf("no %s in %v", name, attrs)
	return 0
}

func attrStrings(attrs goipp.Attributes, name string) []string {
	for _, a := range attrs {
		if a.Name == name {
			var out []string
			for _, v := range a.Values {
				out = append(out, v.V.String())
			}
			return out
		}
	}
	return nil
}

// waitJob polls Get-Job-Attributes until the job is finished and returns its attributes.
func (ts *testService) waitJob(t *testing.T, scheme string, id int) goipp.Attributes {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		resp := ts.do(t, scheme, ts.request(goipp.OpGetJobAttributes, scheme,
			goipp.MakeAttribute("job-id", goipp.TagInteger, goipp.Integer(id))), nil)
		if st := attrInt(t, resp.Job, "job-state"); st >= 7 {
			return resp.Job
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("job %d did not finish", id)
	return nil
}

func formatAttr(f string) goipp.Attribute {
	return goipp.MakeAttribute("document-format", goipp.TagMimeType, goipp.String(f))
}

func readBin(t *testing.T, dir string, n int) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, fmt.Sprintf("job-%d.bin", n)))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func binFiles(t *testing.T, dir string) []string {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(dir, "*.bin"))
	if err != nil {
		t.Fatal(err)
	}
	for i := range m {
		m[i] = filepath.Base(m[i])
	}
	slices.Sort(m)
	return m
}

func TestEndToEndPrintJob(t *testing.T) {
	ts := startService(t)
	const h = 200
	rects := []rect{
		{100, 30, 400, 60},
		{500, 80, 651, 121},
		{2, 50, 10, 55}, // outside the printable columns: dropped
	}
	doc := testutil.EncodeURF(testutil.GrayPage(pageW, h, 300, grayPage(pageW, h, rects...)))

	resp := ts.do(t, "ipp", ts.request(goipp.OpPrintJob, "ipp",
		goipp.MakeAttribute("job-name", goipp.TagName, goipp.String("e2e")),
		formatAttr("image/urf")), doc)
	id := attrInt(t, resp.Job, "job-id")
	job := ts.waitJob(t, "ipp", id)
	if st := attrInt(t, job, "job-state"); st != 9 {
		t.Fatalf("job-state %d, reasons %v", st, attrStrings(job, "job-state-reasons"))
	}
	if n := attrInt(t, job, "job-impressions-completed"); n != 1 {
		t.Fatalf("job-impressions-completed %d, want 1", n)
	}

	want := labelBytes(t, expectedLabel(t, pageW, h, rects...))
	if got := readBin(t, ts.dryDir, 1); !bytes.Equal(got, want) {
		t.Fatalf("job-1.bin (%d bytes) differs from the reference encoding (%d bytes)", len(got), len(want))
	}
	txt, err := os.ReadFile(filepath.Join(ts.dryDir, "job-1.txt"))
	if err != nil {
		t.Fatal(err)
	}
	const want1 = "invalidate 200 | initialize | print-info flags=0x8e media=0x0a width=62 length=0 lines=91 page=0 | " +
		"various-mode auto-cut=1 | cut-every 1 | expanded-mode flags=0x08 cut-at-end=1 high-res=0 | margin 0 | raster-lines 91 | print-and-feed"
	if got := strings.Join(strings.Fields(strings.ReplaceAll(strings.TrimSpace(string(txt)), "\n", " | ")), " "); got != want1 {
		t.Fatalf("job-1.txt = %q", txt)
	}
}

func TestEndToEndCreateJobTLS(t *testing.T) {
	ts := startService(t)
	const h = 300
	first := []rect{{40, 10, 700, 12}, {300, 40, 420, 160}}
	second := []rect{{18, 100, 714, 101}}
	doc := testutil.EncodeURF(
		testutil.GrayPage(pageW, h, 300, grayPage(pageW, h, first...)),
		testutil.GrayPage(pageW, h, 300, grayPage(pageW, h)),
		testutil.GrayPage(pageW, h, 300, grayPage(pageW, h, second...)),
	)

	resp := ts.do(t, "ipps", ts.request(goipp.OpCreateJob, "ipps",
		goipp.MakeAttribute("job-name", goipp.TagName, goipp.String("e2e tls"))), nil)
	id := attrInt(t, resp.Job, "job-id")
	ts.do(t, "ipps", ts.request(goipp.OpSendDocument, "ipps",
		goipp.MakeAttribute("job-id", goipp.TagInteger, goipp.Integer(id)),
		formatAttr("image/urf"),
		goipp.MakeAttribute("last-document", goipp.TagBoolean, goipp.Boolean(true))), doc)
	job := ts.waitJob(t, "ipps", id)
	if st := attrInt(t, job, "job-state"); st != 9 {
		t.Fatalf("job-state %d, reasons %v", st, attrStrings(job, "job-state-reasons"))
	}
	if n := attrInt(t, job, "job-impressions-completed"); n != 2 {
		t.Fatalf("job-impressions-completed %d, want 2 (blank page skipped)", n)
	}
	if got := binFiles(t, ts.dryDir); !slices.Equal(got, []string{"job-1.bin", "job-2.bin"}) {
		t.Fatalf("dry-run files %v", got)
	}
	for i, rs := range [][]rect{first, second} {
		want := labelBytes(t, expectedLabel(t, pageW, h, rs...))
		if got := readBin(t, ts.dryDir, i+1); !bytes.Equal(got, want) {
			t.Fatalf("job-%d.bin differs from the reference encoding", i+1)
		}
	}
}

func TestEndToEndRejectsBeforePrinting(t *testing.T) {
	ts := startService(t)
	good := testutil.GrayPage(pageW, 50, 300, grayPage(pageW, 50, rect{100, 10, 200, 20}))
	truncated := testutil.EncodeURF(good, good)
	truncated = truncated[:len(truncated)-3]
	wide := testutil.GrayPage(800, 50, 300, grayPage(800, 50, rect{100, 10, 200, 20}))
	cases := []struct {
		name   string
		doc    []byte
		format string
	}{
		{"wrong width on page 2", testutil.EncodeURF(good, wide), "image/urf"},
		{"truncated page 2", truncated, "image/urf"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp := ts.do(t, "ipp", ts.request(goipp.OpPrintJob, "ipp", formatAttr(c.format)), c.doc)
			id := attrInt(t, resp.Job, "job-id")
			job := ts.waitJob(t, "ipp", id)
			if st := attrInt(t, job, "job-state"); st != 8 {
				t.Fatalf("job-state %d, want aborted", st)
			}
			if r := attrStrings(job, "job-state-reasons"); !slices.Contains(r, "document-format-error") {
				t.Fatalf("job-state-reasons %v", r)
			}
			if got := binFiles(t, ts.dryDir); len(got) != 0 {
				t.Fatalf("printed %v before refusing the job", got)
			}
		})
	}
}

func TestEndToEndPWG(t *testing.T) {
	ts := startService(t)
	rects := []rect{{60, 5, 600, 40}}
	doc := testutil.EncodePWG(testutil.GrayPage(pageW, 64, 300, grayPage(pageW, 64, rects...)))
	resp := ts.do(t, "ipp", ts.request(goipp.OpPrintJob, "ipp", formatAttr("image/pwg-raster")), doc)
	job := ts.waitJob(t, "ipp", attrInt(t, resp.Job, "job-id"))
	if st := attrInt(t, job, "job-state"); st != 9 {
		t.Fatalf("job-state %d, reasons %v", st, attrStrings(job, "job-state-reasons"))
	}
	if got, want := readBin(t, ts.dryDir, 1), labelBytes(t, expectedLabel(t, pageW, 64, rects...)); !bytes.Equal(got, want) {
		t.Fatal("job-1.bin differs from the reference encoding")
	}
}

func TestEndToEndHighRes(t *testing.T) {
	ts := startService(t, func(o *options) { o.highRes = true })
	const w, h = 2 * pageW, 120
	doc := testutil.EncodeURF(testutil.GrayPage(w, h, 600, grayPage(w, h, rect{200, 20, 1200, 100})))
	quality := goipp.MakeAttribute("print-quality", goipp.TagEnum, goipp.Integer(5))
	m := ts.request(goipp.OpPrintJob, "ipp", formatAttr("image/urf"))
	m.Job.Add(quality)
	resp := ts.do(t, "ipp", m, doc)
	job := ts.waitJob(t, "ipp", attrInt(t, resp.Job, "job-id"))
	if st := attrInt(t, job, "job-state"); st != 9 {
		t.Fatalf("job-state %d, reasons %v", st, attrStrings(job, "job-state-reasons"))
	}
	txt, err := os.ReadFile(filepath.Join(ts.dryDir, "job-1.txt"))
	if err != nil {
		t.Fatal(err)
	}
	// 80 rows at 600 dpi stay 80 rows when high-res keeps the feed resolution.
	if !strings.Contains(string(txt), "flags=0x48 cut-at-end=1 high-res=1") || !strings.Contains(string(txt), "raster-lines 80") {
		t.Fatalf("job-1.txt = %q", txt)
	}
}
