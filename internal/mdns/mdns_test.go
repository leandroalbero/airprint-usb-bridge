package mdns

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/brutella/dnssd"
	"github.com/miekg/dns"
)

func TestTxtStrings(t *testing.T) {
	got, err := txtStrings(map[string]string{
		"rp": "ipp/print", "txtvers": "1", "Color": "F", "URF": "V1.4,W8,CP1,RS300", "note": "",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"txtvers=1", "Color=F", "URF=V1.4,W8,CP1,RS300", "note=", "rp=ipp/print"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q want %q", got, want)
	}

	b, err := txtBytes(map[string]string{"txtvers": "1", "a": "b"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(b, [][]byte{[]byte("txtvers=1"), []byte("a=b")}) {
		t.Fatalf("txtBytes = %q", b)
	}

	for name, txt := range map[string]map[string]string{
		"empty key": {"": "x"},
		"eq in key": {"a=b": "x"},
		"too long":  {"k": strings.Repeat("x", 254)},
	} {
		if _, err := txtStrings(txt); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
	if _, err := txtStrings(map[string]string{"k": strings.Repeat("x", 253)}); err != nil {
		t.Errorf("255-byte entry rejected: %v", err)
	}
}

func TestSubtypeNames(t *testing.T) {
	var got []string
	for _, typ := range ServiceTypes {
		for _, sub := range Subtypes {
			got = append(got, SubtypeName(sub, typ))
		}
	}
	want := []string{
		"_universal._sub._ipp._tcp", "_print._sub._ipp._tcp",
		"_universal._sub._ipps._tcp", "_print._sub._ipps._tcp",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q want %q", got, want)
	}

	table := subtypeTable("Label Printer (2)", "")
	wantTable := map[string]string{
		"_universal._sub._ipp._tcp.local.":  `Label\ Printer\ \(2\)._ipp._tcp.local.`,
		"_print._sub._ipp._tcp.local.":      `Label\ Printer\ \(2\)._ipp._tcp.local.`,
		"_universal._sub._ipps._tcp.local.": `Label\ Printer\ \(2\)._ipps._tcp.local.`,
		"_print._sub._ipps._tcp.local.":     `Label\ Printer\ \(2\)._ipps._tcp.local.`,
	}
	if !reflect.DeepEqual(table, wantTable) {
		t.Fatalf("table = %q", table)
	}
}

func TestEscapeLabelMatchesMiekg(t *testing.T) {
	for _, name := range []string{"Label Printer (2)", "a.b@c;d\"e'f\\g", "Étiquette 62"} {
		ptr := subtypePTR("_x._sub._ipp._tcp.local.", escapeLabel(name)+"._ipp._tcp.local.", 1)
		buf := make([]byte, 512)
		n, err := dns.PackRR(ptr, buf, 0, nil, false)
		if err != nil {
			t.Fatalf("%q: pack: %v", name, err)
		}
		rr, _, err := dns.UnpackRR(buf[:n], 0)
		if err != nil {
			t.Fatalf("%q: unpack: %v", name, err)
		}
		if got := rr.(*dns.PTR).Ptr; got != ptr.Ptr {
			t.Errorf("%q: round trip %q != %q", name, got, ptr.Ptr)
		}
		labels := dns.SplitDomainName(rr.(*dns.PTR).Ptr)
		if len(labels) != 4 {
			t.Errorf("%q: instance split into extra labels: %q", name, labels)
		}
	}
}

func query(name string, qtype uint16, qu bool) *dns.Msg {
	m := new(dns.Msg)
	m.Id = 0x1234
	class := uint16(dns.ClassINET)
	if qu {
		class |= qClassQU
	}
	m.Question = []dns.Question{{Name: name, Qtype: qtype, Qclass: class}}
	return m
}

func TestSubtypeReply(t *testing.T) {
	names := subtypeTable("P", "local")
	target := "P._ipps._tcp.local."

	resp, unicast := subtypeReply(query("_Universal._SUB._ipps._tcp.local.", dns.TypePTR, false), names, false)
	if resp == nil || unicast {
		t.Fatalf("multicast PTR: resp=%v unicast=%v", resp, unicast)
	}
	if len(resp.Answer) != 1 || resp.Answer[0].(*dns.PTR).Ptr != target || resp.Answer[0].Header().Ttl != subtypeTTL {
		t.Fatalf("answer = %v", resp.Answer)
	}
	if resp.Id != 0 || len(resp.Question) != 0 || !resp.Response || !resp.Authoritative {
		t.Fatalf("multicast reply header: %v", resp)
	}

	if resp, unicast = subtypeReply(query("_print._sub._ipp._tcp.local.", dns.TypePTR, true), names, false); resp == nil || !unicast {
		t.Fatalf("QU question: resp=%v unicast=%v", resp, unicast)
	}
	if resp, _ = subtypeReply(query("_print._sub._ipp._tcp.local.", dns.TypeANY, false), names, false); resp == nil {
		t.Fatal("ANY question unanswered")
	}

	for _, q := range []*dns.Msg{
		query("_ipp._tcp.local.", dns.TypePTR, false),
		query("_scanner._sub._ipp._tcp.local.", dns.TypePTR, false),
		query("_print._sub._ipp._tcp.local.", dns.TypeSRV, false),
	} {
		if resp, _ := subtypeReply(q, names, false); resp != nil {
			t.Errorf("%v answered: %v", q.Question[0], resp)
		}
	}

	r := query("_print._sub._ipp._tcp.local.", dns.TypePTR, false)
	r.Response = true
	if resp, _ := subtypeReply(r, names, false); resp != nil {
		t.Error("answered a response")
	}

	known := query("_universal._sub._ipp._tcp.local.", dns.TypePTR, false)
	known.Answer = []dns.RR{subtypePTR("_universal._sub._ipp._tcp.local.", "P._ipp._tcp.local.", subtypeTTL)}
	if resp, _ := subtypeReply(known, names, false); resp != nil {
		t.Errorf("known answer not suppressed: %v", resp)
	}
	known.Answer[0].Header().Ttl = subtypeTTL / 4
	if resp, _ := subtypeReply(known, names, false); resp == nil {
		t.Error("stale known answer suppressed the reply")
	}

	legacy := query("_universal._sub._ipps._tcp.local.", dns.TypePTR, false)
	resp, unicast = subtypeReply(legacy, names, true)
	if resp == nil || !unicast || resp.Id != 0x1234 || len(resp.Question) != 1 || resp.Answer[0].Header().Ttl != legacyTTL {
		t.Fatalf("legacy reply: unicast=%v %v", unicast, resp)
	}
}

func TestSubtypeAnnouncement(t *testing.T) {
	names := subtypeTable("P", "local")
	m := subtypeAnnouncement(names, 0)
	if len(m.Answer) != 4 || !m.Response {
		t.Fatalf("announcement = %v", m)
	}
	for _, rr := range m.Answer {
		if rr.Header().Ttl != 0 || rr.Header().Class != dns.ClassINET {
			t.Errorf("goodbye record %v", rr)
		}
	}
}

func swapPublishers(t *testing.T, present bool) *[]string {
	t.Helper()
	oldP, oldA, oldB := avahiPresent, publishAvahi, publishBuiltin
	t.Cleanup(func() { avahiPresent, publishAvahi, publishBuiltin = oldP, oldA, oldB })
	var calls []string
	avahiPresent = func() bool { calls = append(calls, "probe"); return present }
	publishAvahi = func(context.Context, Service) (func(), error) {
		calls = append(calls, "avahi")
		return func() {}, nil
	}
	publishBuiltin = func(context.Context, Service) (func(), error) {
		calls = append(calls, "builtin")
		return func() {}, nil
	}
	return &calls
}

func TestPublishModeSelection(t *testing.T) {
	svc := Service{Instance: "P", Port: 8631, TXT: map[string]string{"txtvers": "1"}}
	cases := []struct {
		mode    string
		present bool
		want    []string
		wantErr bool
	}{
		{ModeAuto, true, []string{"probe", "avahi"}, false},
		{ModeAuto, false, []string{"probe", "builtin"}, false},
		{"", false, []string{"probe", "builtin"}, false},
		{ModeAvahi, false, []string{"avahi"}, false},
		{ModeBuiltin, true, []string{"builtin"}, false},
		{ModeOff, true, nil, false},
		{"bonjour", true, nil, true},
	}
	for _, c := range cases {
		calls := swapPublishers(t, c.present)
		stop, err := Publish(context.Background(), c.mode, svc)
		if (err != nil) != c.wantErr {
			t.Errorf("%q: err = %v", c.mode, err)
		}
		if err == nil {
			stop()
		}
		if !reflect.DeepEqual(*calls, c.want) {
			t.Errorf("%q present=%v: calls %v want %v", c.mode, c.present, *calls, c.want)
		}
	}
}

func TestPublishAutoAvahiErrorDoesNotFallBack(t *testing.T) {
	calls := swapPublishers(t, true)
	publishAvahi = func(context.Context, Service) (func(), error) {
		*calls = append(*calls, "avahi")
		return nil, errors.New("boom")
	}
	if _, err := Publish(context.Background(), ModeAuto, Service{Instance: "P", Port: 1}); err == nil {
		t.Fatal("want error")
	}
	if !reflect.DeepEqual(*calls, []string{"probe", "avahi"}) {
		t.Fatalf("calls %v", *calls)
	}
}

func TestPublishValidates(t *testing.T) {
	swapPublishers(t, false)
	for _, s := range []Service{
		{Instance: "", Port: 1},
		{Instance: "P", Port: 0},
		{Instance: "P", Port: 70000},
		{Instance: strings.Repeat("x", 59), Port: 1},
		{Instance: "P", Port: 1, TXT: map[string]string{"a=b": ""}},
	} {
		if _, err := Publish(context.Background(), ModeBuiltin, s); err == nil {
			t.Errorf("%+v: want error", s)
		}
	}
}

type fakeGroup struct {
	mu        sync.Mutex
	calls     []string
	commits   int
	onCommit  []int32
	states    chan int32
	freed     int
	resets    int
	hosts     []string
	txt       [][]byte
	commitErr error
}

func newFakeGroup(onCommit ...int32) *fakeGroup {
	return &fakeGroup{onCommit: onCommit, states: make(chan int32, 8)}
}

func (g *fakeGroup) AddService(name, typ, host string, port uint16, txt [][]byte) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls = append(g.calls, "svc "+name+" "+typ)
	g.hosts = append(g.hosts, host)
	g.txt = txt
	return nil
}

func (g *fakeGroup) AddServiceSubtype(name, typ, subtype string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls = append(g.calls, "sub "+name+" "+typ+" "+subtype)
	return nil
}

func (g *fakeGroup) Commit() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.commitErr != nil {
		return g.commitErr
	}
	g.calls = append(g.calls, "commit")
	if g.commits < len(g.onCommit) {
		g.states <- 1
		g.states <- g.onCommit[g.commits]
	}
	g.commits++
	return nil
}

func (g *fakeGroup) Reset() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.resets++
	g.calls = append(g.calls, "reset")
	return nil
}

func (g *fakeGroup) States() <-chan int32 { return g.states }

func (g *fakeGroup) Free() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.freed++
	return nil
}

func (g *fakeGroup) snapshot() (calls []string, freed, resets int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.calls...), g.freed, g.resets
}

func registration(name string) []string {
	return []string{
		"svc " + name + " _ipp._tcp",
		"svc " + name + " _ipps._tcp",
		"sub " + name + " _ipp._tcp _universal._sub._ipp._tcp",
		"sub " + name + " _ipp._tcp _print._sub._ipp._tcp",
		"sub " + name + " _ipps._tcp _universal._sub._ipps._tcp",
		"sub " + name + " _ipps._tcp _print._sub._ipps._tcp",
		"commit",
	}
}

func TestAvahiCollisionRenames(t *testing.T) {
	g := newFakeGroup(groupCollision, groupCollision, groupEstablished)
	stop, err := publishWithGroup(context.Background(), g, Service{
		Instance: "Label Printer", Port: 8631, Host: "printhost",
		TXT: map[string]string{"rp": "ipp/print", "txtvers": "1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	calls, freed, resets := g.snapshot()
	var want []string
	want = append(want, registration("Label Printer")...)
	want = append(want, "reset")
	want = append(want, registration("Label Printer (2)")...)
	want = append(want, "reset")
	want = append(want, registration("Label Printer (3)")...)
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls\n%q\nwant\n%q", calls, want)
	}
	if resets != 2 || freed != 0 {
		t.Fatalf("resets=%d freed=%d", resets, freed)
	}
	if g.hosts[0] != "printhost.local" {
		t.Fatalf("host %q", g.hosts[0])
	}
	if !reflect.DeepEqual(g.txt, [][]byte{[]byte("txtvers=1"), []byte("rp=ipp/print")}) {
		t.Fatalf("txt %q", g.txt)
	}
	stop()
	stop()
	if _, freed, _ := g.snapshot(); freed != 1 {
		t.Fatalf("freed %d times", freed)
	}
}

func TestAvahiDefaultHostIsEmpty(t *testing.T) {
	g := newFakeGroup(groupEstablished)
	stop, err := publishWithGroup(context.Background(), g, Service{Instance: "P", Port: 1})
	if err != nil {
		t.Fatal(err)
	}
	stop()
	if g.hosts[0] != "" {
		t.Fatalf("host %q, want empty for avahi default", g.hosts[0])
	}

	g = newFakeGroup(groupEstablished)
	stop, err = publishWithGroup(context.Background(), g, Service{Instance: "P", Port: 1, Host: systemHost() + ".local"})
	if err != nil {
		t.Fatal(err)
	}
	stop()
	if g.hosts[0] != "" {
		t.Fatalf("own host name passed explicitly: %q", g.hosts[0])
	}
}

func TestAvahiProbeAbsentIsFast(t *testing.T) {
	t0 := time.Now()
	present := avahiOnSystemBus()
	t.Logf("avahi on system bus: %v (%v)", present, time.Since(t0))
	if !present && time.Since(t0) > 2*time.Second {
		t.Fatalf("absent probe took %v", time.Since(t0))
	}
}

func TestAvahiFailure(t *testing.T) {
	g := newFakeGroup(groupFailure)
	if _, err := publishWithGroup(context.Background(), g, Service{Instance: "P", Port: 1}); err == nil {
		t.Fatal("want error")
	}
	if _, freed, _ := g.snapshot(); freed != 1 {
		t.Fatalf("freed %d", freed)
	}

	g = newFakeGroup()
	g.commitErr = errors.New("denied")
	if _, err := publishWithGroup(context.Background(), g, Service{Instance: "P", Port: 1}); err == nil {
		t.Fatal("want commit error")
	}
	if _, freed, _ := g.snapshot(); freed != 1 {
		t.Fatalf("freed %d after commit error", freed)
	}
}

func TestAvahiContextCancelFrees(t *testing.T) {
	g := newFakeGroup(groupEstablished)
	ctx, cancel := context.WithCancel(context.Background())
	if _, err := publishWithGroup(ctx, g, Service{Instance: "P", Port: 1}); err != nil {
		t.Fatal(err)
	}
	cancel()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, freed, _ := g.snapshot(); freed == 1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("group not freed after ctx cancel")
}

func TestAvahiLateCollisionStillRenames(t *testing.T) {
	g := newFakeGroup(groupEstablished, groupEstablished)
	stop, err := publishWithGroup(context.Background(), g, Service{Instance: "P", Port: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	g.states <- groupCollision
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		calls, _, _ := g.snapshot()
		if len(calls) > 0 && calls[len(calls)-1] == "commit" && strings.Contains(strings.Join(calls, "|"), "P (2)") {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	calls, _, _ := g.snapshot()
	t.Fatalf("no rename after late collision: %q", calls)
}

func TestSettleNamesKeepsTypesOnOneName(t *testing.T) {
	old := probeService
	t.Cleanup(func() { probeService = old })
	var probed []string
	probeService = func(_ context.Context, s dnssd.Service) (dnssd.Service, error) {
		probed = append(probed, s.Name+" "+s.Type)
		if s.Type == "_ipps._tcp" && s.Name == "Label Printer" {
			s.Name = "Label Printer (2)"
		}
		return s, nil
	}
	cfgs, err := settleNames(context.Background(), Service{Instance: "Label Printer", Port: 8631, Host: "printhost.local."})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"Label Printer _ipp._tcp", "Label Printer _ipps._tcp",
		"Label Printer (2) _ipp._tcp", "Label Printer (2) _ipps._tcp",
	}
	if !reflect.DeepEqual(probed, want) {
		t.Fatalf("probes %q want %q", probed, want)
	}
	var got []string
	for _, c := range cfgs {
		got = append(got, c.Name+" "+c.Type+" "+c.Host)
	}
	sort.Strings(got)
	if !reflect.DeepEqual(got, []string{"Label Printer (2) _ipp._tcp printhost", "Label Printer (2) _ipps._tcp printhost"}) {
		t.Fatalf("services %q", got)
	}
}

func TestSettleNamesKeepsUserSuffix(t *testing.T) {
	old := probeService
	t.Cleanup(func() { probeService = old })
	probeService = func(_ context.Context, s dnssd.Service) (dnssd.Service, error) { return s, nil }
	cfgs, err := settleNames(context.Background(), Service{Instance: "Printer (3)", Port: 1})
	if err != nil {
		t.Fatal(err)
	}
	if cfgs[0].Name != "Printer (3)" {
		t.Fatalf("name %q", cfgs[0].Name)
	}
}
