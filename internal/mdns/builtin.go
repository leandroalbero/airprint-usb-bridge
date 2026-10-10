package mdns

import (
	"context"
	"fmt"
	"strings"

	"github.com/brutella/dnssd"
)

// probeService is swapped in tests; dnssd.ProbeService needs a live network.
var probeService = dnssd.ProbeService

// publishBuiltinDNSSD runs a brutella/dnssd responder for the base types. dnssd v1.2.14 has
// no subtype support (its responder only answers the service type, instance, host and
// _services._dns-sd._udp names), so a companion responder answers the subtype PTRs.
func publishBuiltinDNSSD(ctx context.Context, s Service) (func(), error) {
	cfgs, err := settleNames(ctx, s)
	if err != nil {
		return nil, err
	}

	rsp, err := dnssd.NewResponder()
	if err != nil {
		return nil, fmt.Errorf("mdns: dnssd responder: %w", err)
	}
	for _, c := range cfgs {
		if _, err := rsp.Add(c); err != nil {
			return nil, fmt.Errorf("mdns: dnssd add %s: %w", c.Type, err)
		}
	}

	sub, err := newSubtypeResponder(subtypeTable(cfgs[0].Name, cfgs[0].Domain))
	if err != nil {
		return nil, err
	}

	runCtx, cancel := context.WithCancel(context.Background())
	respDone := make(chan struct{})
	go func() {
		defer close(respDone)
		rsp.Respond(runCtx)
	}()
	sub.start(runCtx)

	stop := onceFunc(func() {
		cancel()
		// dnssd sends its goodbyes one interface at a time with 250 ms pauses; overlap ours.
		subDone := make(chan struct{})
		go func() {
			defer close(subDone)
			sub.close()
		}()
		<-respDone
		<-subDone
	})
	go func() {
		select {
		case <-ctx.Done():
			stop()
		case <-runCtx.Done():
		}
	}()
	return stop, nil
}

// settleNames probes every service type under one instance name. dnssd renames each service
// independently, so a conflict on either type restarts probing for both under the new name,
// keeping _ipp and _ipps (and the subtype PTRs) on the same instance.
func settleNames(ctx context.Context, s Service) ([]dnssd.Service, error) {
	name, host := s.Instance, bareHost(s.Host)
	for round := 0; round < 20; round++ {
		var out []dnssd.Service
		changed := false
		for _, t := range ServiceTypes {
			svc, err := newDNSSDService(name, t, host, s)
			if err != nil {
				return nil, err
			}
			p, err := probeService(ctx, svc)
			if err != nil {
				return nil, fmt.Errorf("mdns: probe %s: %w", t, err)
			}
			if p.Name != svc.Name || p.Host != svc.Host {
				name, host = p.Name, p.Host
				changed = true
				break
			}
			out = append(out, p)
		}
		if !changed {
			return out, nil
		}
	}
	return nil, fmt.Errorf("mdns: no stable name for %q after 20 probe rounds", s.Instance)
}

func newDNSSDService(name, typ, host string, s Service) (dnssd.Service, error) {
	svc, err := dnssd.NewService(dnssd.Config{
		Name: name,
		Type: typ,
		Host: host,
		Text: s.TXT,
		Port: s.Port,
	})
	if err != nil {
		return svc, fmt.Errorf("mdns: %w", err)
	}
	// NewService strips a trailing " (N)", which would undo a rename or a user's own name.
	svc.Name = name
	return svc, nil
}

// subtypeTable maps each lower-cased subtype service FQDN to the instance FQDN it points at.
func subtypeTable(instance, domain string) map[string]string {
	if domain == "" {
		domain = "local"
	}
	t := map[string]string{}
	for _, typ := range ServiceTypes {
		target := escapeLabel(instance) + "." + typ + "." + domain + "."
		for _, sub := range Subtypes {
			t[strings.ToLower(SubtypeName(sub, typ)+"."+domain+".")] = target
		}
	}
	return t
}

// escapeLabel matches the presentation form miekg/dns produces when unpacking a name, so
// known-answer comparison works on the raw strings.
func escapeLabel(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case strings.IndexByte(".'@;()\"\\ ", c) >= 0:
			b.WriteByte('\\')
			b.WriteByte(c)
		case c < ' ' || c > '~':
			fmt.Fprintf(&b, "\\%03d", c)
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}
