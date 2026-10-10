package ipp

import (
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/OpenPrinting/goipp"
)

const testUUID = "urn:uuid:00000000-1111-2222-3333-444455556666"

func testConfig() Config {
	c := DefaultConfig()
	c.Name = "Test Label Printer"
	c.Location = "Test Bench"
	c.UUID = testUUID
	return c
}

type testServer struct {
	*Server
	addr   string
	client *http.Client
}

func startServer(t *testing.T, cfg Config, proc Processor) *testServer {
	t.Helper()
	if cfg.StateDir == "" {
		cfg.StateDir = t.TempDir()
	}
	if cfg.Logf == nil {
		cfg.Logf = t.Logf
	}
	s, err := New(cfg, proc)
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
		_ = s.Serve(l)
	}()
	for s.port.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := s.Shutdown(ctx); err != nil {
			t.Errorf("shutdown: %v", err)
		}
		<-served
	})
	return &testServer{
		Server: s,
		addr:   l.Addr().String(),
		client: &http.Client{Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		}, Timeout: 30 * time.Second},
	}
}

func (ts *testServer) uri(scheme string) string { return scheme + "://" + ts.addr + printerPath }

// recorder is a Processor that records documents and can be held mid-job.
type recorder struct {
	mu      sync.Mutex
	docs    [][]byte
	formats []string
	jobs    []*Job
	hold    chan struct{} // when non-nil, each job waits for a receive or ctx
	started chan int
	err     error
}

func (r *recorder) process(ctx context.Context, job *Job, doc io.Reader, format string) error {
	b, err := io.ReadAll(doc)
	if err != nil {
		return err
	}
	r.mu.Lock()
	r.docs = append(r.docs, b)
	r.formats = append(r.formats, format)
	r.jobs = append(r.jobs, job)
	hold, started, perr := r.hold, r.started, r.err
	r.mu.Unlock()
	if started != nil {
		started <- job.ID
	}
	if hold != nil {
		select {
		case <-hold:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	job.SetImpressions(1)
	return perr
}

func opRequest(op goipp.Op, uri string, extra ...goipp.Attribute) *goipp.Message {
	m := goipp.NewRequest(goipp.MakeVersion(2, 0), op, 1)
	m.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	m.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en")))
	m.Operation.Add(goipp.MakeAttribute("printer-uri", goipp.TagURI, goipp.String(uri)))
	m.Operation.Add(goipp.MakeAttribute("requesting-user-name", goipp.TagName, goipp.String("tester")))
	for _, a := range extra {
		m.Operation.Add(a)
	}
	return m
}

func (ts *testServer) do(t *testing.T, scheme string, m *goipp.Message, doc []byte) *goipp.Message {
	t.Helper()
	b, err := m.EncodeBytes()
	if err != nil {
		t.Fatal(err)
	}
	b = append(b, doc...)
	resp, err := ts.client.Post("http"+scheme[3:]+"://"+ts.addr+printerPath, "application/ipp", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("HTTP %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/ipp" {
		t.Fatalf("Content-Type %q", ct)
	}
	var out goipp.Message
	if err := out.Decode(resp.Body); err != nil {
		t.Fatal(err)
	}
	if out.RequestID != m.RequestID {
		t.Fatalf("request-id %d, want %d", out.RequestID, m.RequestID)
	}
	return &out
}

func status(m *goipp.Message) goipp.Status { return goipp.Status(m.Code) }

func attrOf(attrs goipp.Attributes, name string) (goipp.Attribute, bool) {
	for _, a := range attrs {
		if a.Name == name {
			return a, true
		}
	}
	return goipp.Attribute{}, false
}

func intOf(t *testing.T, attrs goipp.Attributes, name string) int {
	t.Helper()
	a, ok := attrOf(attrs, name)
	if !ok {
		t.Fatalf("missing %s in %v", name, attrs)
	}
	n, ok := a.Values[0].V.(goipp.Integer)
	if !ok {
		t.Fatalf("%s is %T", name, a.Values[0].V)
	}
	return int(n)
}

func strOf(t *testing.T, attrs goipp.Attributes, name string) string {
	t.Helper()
	a, ok := attrOf(attrs, name)
	if !ok {
		t.Fatalf("missing %s in %v", name, attrs)
	}
	return a.Values[0].V.String()
}

func jobIDAttr(id int) goipp.Attribute {
	return goipp.MakeAttribute("job-id", goipp.TagInteger, goipp.Integer(id))
}

func rollMediaCol(x, y int) goipp.Attribute {
	size := goipp.Collection{
		goipp.MakeAttribute("x-dimension", goipp.TagInteger, goipp.Integer(x)),
		goipp.MakeAttribute("y-dimension", goipp.TagInteger, goipp.Integer(y)),
	}
	col := goipp.Collection{
		goipp.MakeAttribute("media-size", goipp.TagBeginCollection, size),
		goipp.MakeAttribute("media-right-margin", goipp.TagInteger, goipp.Integer(0)),
		goipp.MakeAttribute("media-left-margin", goipp.TagInteger, goipp.Integer(0)),
		goipp.MakeAttribute("media-top-margin", goipp.TagInteger, goipp.Integer(0)),
		goipp.MakeAttribute("media-bottom-margin", goipp.TagInteger, goipp.Integer(0)),
	}
	return goipp.MakeAttribute("media-col", goipp.TagBeginCollection, col)
}

// tinyURF is a one-page 8-bit grey URF, width x height at 300 dpi, with a black
// bar across the middle rows.
func tinyURF(width, height int) []byte {
	var b bytes.Buffer
	b.WriteString("UNIRAST\x00")
	be32 := func(v uint32) { b.Write([]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)}) }
	be32(1)
	b.Write([]byte{8, 0, 1, 4}) // bpp, sGray, simplex, normal quality
	be32(0)
	be32(0)
	be32(uint32(width))
	be32(uint32(height))
	be32(300)
	be32(0)
	be32(0)
	for y := 0; y < height; y++ {
		b.WriteByte(0) // line repeat: once
		v := byte(0xff)
		if y >= height/3 && y < 2*height/3 {
			v = 0
		}
		for x := 0; x < width; {
			n := width - x
			if n > 128 {
				n = 128
			}
			b.WriteByte(byte(n - 1))
			b.WriteByte(v)
			x += n
		}
	}
	return b.Bytes()
}

func waitJobState(t *testing.T, ts *testServer, id int, want JobState) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if j := ts.findJob(id); j != nil && j.State() == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	j := ts.findJob(id)
	got := "missing"
	if j != nil {
		got = j.State().String()
	}
	t.Fatalf("job %d state %s, want %s", id, got, want)
}

func itoa(n int) string { return strconv.Itoa(n) }
