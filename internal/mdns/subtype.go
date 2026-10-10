package mdns

import (
	"context"
	"errors"
	"math/rand/v2"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/brutella/dnssd"
	"github.com/miekg/dns"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
)

var (
	mdnsGroup4 = &net.UDPAddr{IP: net.IPv4(224, 0, 0, 251), Port: 5353}
	mdnsGroup6 = &net.UDPAddr{IP: net.ParseIP("ff02::fb"), Port: 5353}

	announceInterval = time.Second
)

const (
	// Same TTL dnssd gives its own service PTR.
	subtypeTTL = 4500
	// RFC 6762 6.7: legacy unicast answers carry a TTL of at most 10 s.
	legacyTTL = 10
	qClassQU  = 1 << 15
)

// subtypeReply answers the subtype PTR questions in req. names maps lower-cased subtype
// FQDNs to instance FQDNs. It returns nil when nothing in req is ours. unicast reports that
// the reply goes straight back to the sender: a QU question or a legacy (non-5353) resolver.
func subtypeReply(req *dns.Msg, names map[string]string, legacy bool) (resp *dns.Msg, unicast bool) {
	if req == nil || req.Response || req.Opcode != dns.OpcodeQuery {
		return nil, false
	}
	var answers []dns.RR
	var questions []dns.Question
	seen := map[string]bool{}
	qu := false
	for _, q := range req.Question {
		if q.Qtype != dns.TypePTR && q.Qtype != dns.TypeANY {
			continue
		}
		class := q.Qclass &^ qClassQU
		if class != dns.ClassINET && class != dns.ClassANY {
			continue
		}
		key := strings.ToLower(q.Name)
		target, ok := names[key]
		if !ok || seen[key] {
			continue
		}
		seen[key] = true
		ptr := subtypePTR(key, target, subtypeTTL)
		if knownAnswer(req.Answer, ptr) {
			continue
		}
		if legacy {
			ptr.Hdr.Ttl = legacyTTL
		}
		answers = append(answers, ptr)
		questions = append(questions, q)
		if q.Qclass&qClassQU != 0 {
			qu = true
		}
	}
	if len(answers) == 0 {
		return nil, false
	}
	resp = new(dns.Msg)
	resp.Response = true
	resp.Authoritative = true
	resp.Answer = answers
	if legacy {
		resp.Id = req.Id
		resp.Question = questions
	}
	return resp, legacy || qu
}

func subtypePTR(name, target string, ttl uint32) *dns.PTR {
	return &dns.PTR{
		Hdr: dns.RR_Header{Name: name, Rrtype: dns.TypePTR, Class: dns.ClassINET, Ttl: ttl},
		Ptr: target,
	}
}

// knownAnswer implements RFC 6762 7.1: skip a record the querier already holds with more
// than half its TTL left.
func knownAnswer(known []dns.RR, ptr *dns.PTR) bool {
	for _, rr := range known {
		k, ok := rr.(*dns.PTR)
		if !ok {
			continue
		}
		if strings.EqualFold(k.Hdr.Name, ptr.Hdr.Name) && strings.EqualFold(k.Ptr, ptr.Ptr) && k.Hdr.Ttl > ptr.Hdr.Ttl/2 {
			return true
		}
	}
	return false
}

func subtypeAnnouncement(names map[string]string, ttl uint32) *dns.Msg {
	keys := make([]string, 0, len(names))
	for k := range names {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	m := new(dns.Msg)
	m.Response = true
	m.Authoritative = true
	for _, k := range keys {
		m.Answer = append(m.Answer, subtypePTR(k, names[k], ttl))
	}
	return m
}

type subtypeResponder struct {
	names map[string]string
	v4    *ipv4.PacketConn
	v6    *ipv6.PacketConn
	wg    sync.WaitGroup
	ctx   context.Context
}

func newSubtypeResponder(names map[string]string) (*subtypeResponder, error) {
	r := &subtypeResponder{names: names}
	ifaces := dnssd.MulticastInterfaces()
	if c, err := net.ListenUDP("udp4", mdnsGroup4); err == nil {
		p := ipv4.NewPacketConn(c)
		_ = p.SetControlMessage(ipv4.FlagInterface, true)
		_ = p.SetMulticastLoopback(true)
		_ = p.SetTTL(255)
		_ = p.SetMulticastTTL(255)
		for _, ifi := range ifaces {
			_ = p.JoinGroup(ifi, &net.UDPAddr{IP: mdnsGroup4.IP})
		}
		r.v4 = p
	}
	if c, err := net.ListenUDP("udp6", mdnsGroup6); err == nil {
		p := ipv6.NewPacketConn(c)
		_ = p.SetControlMessage(ipv6.FlagInterface, true)
		_ = p.SetMulticastLoopback(true)
		_ = p.SetHopLimit(255)
		_ = p.SetMulticastHopLimit(255)
		for _, ifi := range ifaces {
			_ = p.JoinGroup(ifi, &net.UDPAddr{IP: mdnsGroup6.IP})
		}
		r.v6 = p
	}
	if r.v4 == nil && r.v6 == nil {
		return nil, errors.New("mdns: cannot listen on udp 5353 for subtype answers")
	}
	return r, nil
}

func (r *subtypeResponder) start(ctx context.Context) {
	r.ctx = ctx
	if r.v4 != nil {
		r.wg.Add(1)
		go r.read4()
	}
	if r.v6 != nil {
		r.wg.Add(1)
		go r.read6()
	}
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		msg := subtypeAnnouncement(r.names, subtypeTTL)
		for i := 0; i < 2; i++ {
			r.multicastAll(msg)
			select {
			case <-ctx.Done():
				return
			case <-time.After(announceInterval):
			}
		}
	}()
}

// close sends goodbyes; the start ctx must already be cancelled so readers stop replying.
func (r *subtypeResponder) close() {
	bye := subtypeAnnouncement(r.names, 0)
	r.multicastAll(bye)
	time.Sleep(250 * time.Millisecond)
	r.multicastAll(bye)
	if r.v4 != nil {
		r.v4.Close()
	}
	if r.v6 != nil {
		r.v6.Close()
	}
	r.wg.Wait()
}

func (r *subtypeResponder) multicastAll(m *dns.Msg) {
	b, err := m.Pack()
	if err != nil {
		return
	}
	for _, ifi := range dnssd.MulticastInterfaces() {
		if r.v4 != nil {
			_, _ = r.v4.WriteTo(b, &ipv4.ControlMessage{IfIndex: ifi.Index}, mdnsGroup4)
		}
		if r.v6 != nil {
			_, _ = r.v6.WriteTo(b, &ipv6.ControlMessage{IfIndex: ifi.Index}, mdnsGroup6)
		}
	}
}

func (r *subtypeResponder) read4() {
	defer r.wg.Done()
	buf := make([]byte, 9000)
	for {
		n, cm, from, err := r.v4.ReadFrom(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		ifIndex := 0
		if cm != nil {
			ifIndex = cm.IfIndex
		}
		r.handle(buf[:n], from, ifIndex, func(b []byte, to net.Addr) {
			_, _ = r.v4.WriteTo(b, &ipv4.ControlMessage{IfIndex: ifIndex}, to)
		}, mdnsGroup4)
	}
}

func (r *subtypeResponder) read6() {
	defer r.wg.Done()
	buf := make([]byte, 9000)
	for {
		n, cm, from, err := r.v6.ReadFrom(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		ifIndex := 0
		if cm != nil {
			ifIndex = cm.IfIndex
		}
		r.handle(buf[:n], from, ifIndex, func(b []byte, to net.Addr) {
			_, _ = r.v6.WriteTo(b, &ipv6.ControlMessage{IfIndex: ifIndex}, to)
		}, mdnsGroup6)
	}
}

func (r *subtypeResponder) handle(pkt []byte, from net.Addr, ifIndex int, send func([]byte, net.Addr), group *net.UDPAddr) {
	if r.ctx.Err() != nil {
		return
	}
	req := new(dns.Msg)
	if err := req.Unpack(pkt); err != nil {
		return
	}
	src, _ := from.(*net.UDPAddr)
	legacy := src != nil && src.Port != 5353
	resp, unicast := subtypeReply(req, r.names, legacy)
	if resp == nil {
		return
	}
	b, err := resp.Pack()
	if err != nil {
		return
	}
	if unicast && src != nil {
		send(b, src)
		return
	}
	// RFC 6762 6: delay shared-record answers 20-120 ms so peers' answers can suppress ours.
	delay := time.Duration(20+rand.IntN(100)) * time.Millisecond
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		select {
		case <-r.ctx.Done():
		case <-time.After(delay):
			send(b, group)
		}
	}()
}
