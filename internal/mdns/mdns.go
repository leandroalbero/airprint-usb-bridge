// Package mdns advertises the printer over DNS-SD as _ipp._tcp and _ipps._tcp with the
// _universal and _print subtypes, either through avahi-daemon (D-Bus) or a built-in
// multicast DNS responder.
package mdns

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// Service describes the printer to advertise. Host is the bare host name (".local" is
// optional); empty means the system host name. TXT is published on both service types.
type Service struct {
	Instance string
	Port     int
	Host     string
	TXT      map[string]string
}

const (
	ModeAuto    = "auto"
	ModeAvahi   = "avahi"
	ModeBuiltin = "builtin"
	ModeOff     = "off"
)

// ServiceTypes are the DNS-SD types every instance is published under.
var ServiceTypes = []string{"_ipp._tcp", "_ipps._tcp"}

// Subtypes are added to each of ServiceTypes.
var Subtypes = []string{"_universal", "_print"}

// SubtypeName returns the full subtype service name, e.g. "_universal._sub._ipp._tcp".
func SubtypeName(subtype, serviceType string) string {
	return subtype + "._sub." + serviceType
}

var (
	avahiPresent   = avahiOnSystemBus
	publishAvahi   = publishAvahiDBus
	publishBuiltin = publishBuiltinDNSSD
)

// Publish advertises _ipp._tcp and _ipps._tcp with the _universal and _print subtypes, and
// returns a stop func. ctx bounds both the setup and the lifetime of the advertisement.
// The stop func sends goodbyes, waits for them to go out and is safe to call more than once.
func Publish(ctx context.Context, mode string, s Service) (stop func(), err error) {
	if mode == ModeOff {
		return func() {}, nil
	}
	if err := s.validate(); err != nil {
		return nil, err
	}
	switch mode {
	case ModeAuto, "":
		// avahi-daemon owns 5353; a second responder beside it gives conflicting answers, so
		// a failed avahi publish must not fall back to builtin.
		if avahiPresent() {
			return publishAvahi(ctx, s)
		}
		return publishBuiltin(ctx, s)
	case ModeAvahi:
		return publishAvahi(ctx, s)
	case ModeBuiltin:
		return publishBuiltin(ctx, s)
	default:
		return nil, fmt.Errorf("mdns: unknown mode %q (want auto, avahi, builtin or off)", mode)
	}
}

func (s Service) validate() error {
	if s.Instance == "" {
		return errors.New("mdns: empty instance name")
	}
	// Leave room for a " (NN)" rename suffix inside the 63-byte label limit.
	if len(s.Instance) > 58 {
		return fmt.Errorf("mdns: instance name %q longer than 58 bytes", s.Instance)
	}
	if s.Port <= 0 || s.Port > 65535 {
		return fmt.Errorf("mdns: invalid port %d", s.Port)
	}
	_, err := txtStrings(s.TXT)
	return err
}

// txtStrings encodes TXT as key=value strings with txtvers first (RFC 6763 6.7) and the
// rest sorted so the record is stable across restarts.
func txtStrings(txt map[string]string) ([]string, error) {
	keys := make([]string, 0, len(txt))
	for k := range txt {
		if k == "" || strings.ContainsRune(k, '=') {
			return nil, fmt.Errorf("mdns: invalid TXT key %q", k)
		}
		if k != "txtvers" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	if _, ok := txt["txtvers"]; ok {
		keys = append([]string{"txtvers"}, keys...)
	}
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		e := k + "=" + txt[k]
		if len(e) > 255 {
			return nil, fmt.Errorf("mdns: TXT entry %q longer than 255 bytes", k)
		}
		out = append(out, e)
	}
	return out, nil
}

func txtBytes(txt map[string]string) ([][]byte, error) {
	ss, err := txtStrings(txt)
	if err != nil {
		return nil, err
	}
	out := make([][]byte, len(ss))
	for i, s := range ss {
		out[i] = []byte(s)
	}
	return out, nil
}

func renamed(base string, n int) string {
	if n <= 1 {
		return base
	}
	return fmt.Sprintf("%s (%d)", base, n)
}

// bareHost strips the domain so "host.local." and "host" both become "host".
func bareHost(h string) string {
	h = strings.TrimSuffix(h, ".")
	h = strings.TrimSuffix(h, ".local")
	return h
}

func onceFunc(f func()) func() {
	var once sync.Once
	return func() { once.Do(f) }
}
