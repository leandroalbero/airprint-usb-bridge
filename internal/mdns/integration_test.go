//go:build mdns_integration

package mdns

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

// Run with: go test -tags mdns_integration -run Integration -v ./internal/mdns
// It needs macOS dns-sd and a network interface that carries multicast.
func TestIntegrationBuiltinBrowse(t *testing.T) {
	if _, err := exec.LookPath("dns-sd"); err != nil {
		t.Skip("dns-sd not on PATH")
	}
	instance := fmt.Sprintf("qlap-it-%d", time.Now().UnixNano()%1_000_000)
	stop, err := Publish(context.Background(), ModeBuiltin, Service{
		Instance: instance,
		Port:     18631,
		TXT:      map[string]string{"txtvers": "1", "rp": "ipp/print", "ty": "Test Label Printer", "pdl": "image/pwg-raster,image/urf"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	cases := [][]string{
		{"-B", "_ipp._tcp,_universal"},
		{"-B", "_ipps._tcp,_universal"},
		{"-B", "_ipp._tcp,_print"},
		{"-B", "_ipps._tcp,_print"},
		{"-B", "_ipps._tcp"},
		{"-L", instance, "_ipps._tcp", "local"},
	}
	outs := make([]string, len(cases))
	var wg sync.WaitGroup
	for i, args := range cases {
		wg.Add(1)
		go func() {
			defer wg.Done()
			outs[i] = dnssdFor(5*time.Second, args...)
		}()
	}
	wg.Wait()

	for i, args := range cases {
		out := outs[i]
		t.Logf("dns-sd %s\n%s", strings.Join(args, " "), out)
		if !strings.Contains(out, instance) {
			t.Errorf("dns-sd %s: %q not found", strings.Join(args, " "), instance)
		}
	}
	if l := outs[len(outs)-1]; !strings.Contains(l, ":18631") || !strings.Contains(l, "txtvers=1") {
		t.Errorf("resolve did not return port and TXT")
	}

	byeOut := make(chan string, 1)
	// dnssd's goodbyes take a few seconds with many interfaces, and mDNSResponder drops a
	// TTL-0 record one second after it arrives.
	go func() { byeOut <- dnssdFor(10*time.Second, "-B", "_ipps._tcp,_universal") }()
	time.Sleep(2 * time.Second)
	stop()
	out := <-byeOut
	t.Logf("dns-sd -B _ipps._tcp,_universal across stop\n%s", out)
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "Rmv") && strings.Contains(line, instance) {
			return
		}
	}
	t.Errorf("no goodbye (Rmv) seen for %q", instance)
}

func dnssdFor(d time.Duration, args ...string) string {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	cmd := exec.CommandContext(ctx, "dns-sd", args...)
	// SIGKILL would lose dns-sd's buffered stdout.
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	_ = cmd.Run()
	return buf.String()
}
