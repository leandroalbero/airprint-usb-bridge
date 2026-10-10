package ipp

import (
	"bytes"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/OpenPrinting/goipp"
)

func postRaw(t *testing.T, ts *testServer, body io.Reader) *goipp.Message {
	t.Helper()
	req, err := http.NewRequest("POST", "http://"+ts.addr+printerPath, body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/ipp")
	resp, err := ts.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out goipp.Message
	if err := out.Decode(resp.Body); err != nil {
		t.Fatal(err)
	}
	return &out
}

func TestSendDocumentSlowerThanCreateTimeout(t *testing.T) {
	old := multipleOperationTimeout
	multipleOperationTimeout = 100 * time.Millisecond
	defer func() { multipleOperationTimeout = old }()
	rec := &recorder{}
	ts := startServer(t, testConfig(), rec.process)
	id := createJob(t, ts, "ipp")

	m := opRequest(goipp.OpSendDocument, ts.uri("ipp"), jobIDAttr(id),
		goipp.MakeAttribute("last-document", goipp.TagBoolean, goipp.Boolean(true)),
		goipp.MakeAttribute("document-format", goipp.TagMimeType, goipp.String(formatURF)))
	hdr, _ := m.EncodeBytes()
	urf := tinyURF(732, 40)
	pr, pw := io.Pipe()
	go func() {
		pw.Write(hdr)
		for i := 0; i < len(urf); i += len(urf) / 5 {
			time.Sleep(80 * time.Millisecond)
			pw.Write(urf[i:min(i+len(urf)/5, len(urf))])
		}
		pw.Close()
	}()
	if st := status(postRaw(t, ts, pr)); st != goipp.StatusOk {
		t.Fatalf("slow Send-Document: %v", st)
	}
	waitJobState(t, ts, id, JobCompleted)
	if !bytes.Equal(rec.docs[0], urf) {
		t.Errorf("document %d bytes, want %d", len(rec.docs[0]), len(urf))
	}
}

// A failed upload must leave the job's timeout running, or it stays pending-held forever.
func TestSendDocumentErrorRearmsTimeout(t *testing.T) {
	old := multipleOperationTimeout
	multipleOperationTimeout = 100 * time.Millisecond
	defer func() { multipleOperationTimeout = old }()
	cfg := testConfig()
	cfg.MaxDocumentBytes = 1000
	ts := startServer(t, cfg, (&recorder{}).process)
	id := createJob(t, ts, "ipp")
	if st := status(sendDocument(t, ts, id, true, formatURF, tinyURF(732, 200))); st != goipp.StatusErrorRequestEntity {
		t.Fatalf("oversized Send-Document: %v", st)
	}
	waitJobState(t, ts, id, JobAborted)
}

func TestRequestIDZero(t *testing.T) {
	ts := startServer(t, testConfig(), (&recorder{}).process)
	m := opRequest(goipp.OpGetPrinterAttributes, ts.uri("ipp"))
	m.RequestID = 0
	if st := status(ts.do(t, "ipp", m, nil)); st != goipp.StatusErrorBadRequest {
		t.Errorf("request-id 0: %v", st)
	}
}

// goipp decodes nested collections recursively; deep nesting must be refused
// by size before it can exhaust the stack.
func TestNestedCollectionsRefused(t *testing.T) {
	ts := startServer(t, testConfig(), (&recorder{}).process)
	var b bytes.Buffer
	b.Write([]byte{2, 0, 0, byte(goipp.OpGetPrinterAttributes), 0, 0, 0, 1, byte(goipp.TagOperationGroup)})
	b.Write([]byte{byte(goipp.TagBeginCollection), 0, 1, 'c', 0, 0})
	for b.Len() < 4*maxMessageBytes {
		b.Write([]byte{byte(goipp.TagMemberName), 0, 0, 0, 1, 'm'})
		b.Write([]byte{byte(goipp.TagBeginCollection), 0, 0, 0, 0})
	}
	if st := status(postRaw(t, ts, &b)); st != goipp.StatusErrorRequestEntity {
		t.Errorf("deeply nested request: %v", st)
	}
	if st := status(ts.do(t, "ipp", opRequest(goipp.OpGetPrinterAttributes, ts.uri("ipp")), nil)); st != goipp.StatusOk {
		t.Errorf("server after nested request: %v", st)
	}
}

func TestTooManyPendingJobs(t *testing.T) {
	cfg := testConfig()
	cfg.MaxPendingJobs = 2
	rec := &recorder{hold: make(chan struct{}), started: make(chan int, 10)}
	ts := startServer(t, cfg, rec.process)
	first := intOf(t, printJob(t, ts, formatURF, tinyURF(732, 2)).Job, "job-id")
	<-rec.started
	if st := status(printJob(t, ts, formatURF, tinyURF(732, 2))); st != goipp.StatusOk {
		t.Fatalf("second job: %v", st)
	}
	if st := status(printJob(t, ts, formatURF, tinyURF(732, 2))); st != goipp.StatusErrorBusy {
		t.Errorf("third Print-Job: %v", st)
	}
	m := opRequest(goipp.OpCreateJob, ts.uri("ipp"))
	if st := status(ts.do(t, "ipp", m, nil)); st != goipp.StatusErrorBusy {
		t.Errorf("Create-Job over the cap: %v", st)
	}
	rec.hold <- struct{}{}
	waitJobState(t, ts, first, JobCompleted)
	if st := status(printJob(t, ts, formatURF, tinyURF(732, 2))); st != goipp.StatusOk {
		t.Errorf("Print-Job after a slot freed: %v", st)
	}
	close(rec.hold)
}

func TestSpoolBudget(t *testing.T) {
	doc := tinyURF(732, 300)
	cfg := testConfig()
	cfg.MaxDocumentBytes = int64(len(doc)) + 10
	cfg.MaxSpoolBytes = int64(len(doc)) * 3 / 2
	rec := &recorder{hold: make(chan struct{}), started: make(chan int, 10)}
	ts := startServer(t, cfg, rec.process)
	first := intOf(t, printJob(t, ts, formatURF, doc).Job, "job-id")
	<-rec.started
	if st := status(printJob(t, ts, formatURF, doc)); st != goipp.StatusErrorBusy {
		t.Errorf("Print-Job past the spool budget: %v", st)
	}
	if n := ts.spoolUsed.Load(); n != int64(len(doc)) {
		t.Errorf("spool used %d while one job holds %d", n, len(doc))
	}
	close(rec.hold)
	waitJobState(t, ts, first, JobCompleted)
	if n := ts.spoolUsed.Load(); n != 0 {
		t.Errorf("spool used %d after the job finished", n)
	}
	id := intOf(t, printJob(t, ts, formatURF, doc).Job, "job-id")
	waitJobState(t, ts, id, JobCompleted)
}
