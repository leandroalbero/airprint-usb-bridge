package mdns

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"
)

const (
	avahiName       = "org.freedesktop.Avahi"
	avahiServer     = "org.freedesktop.Avahi.Server"
	avahiEntryGroup = "org.freedesktop.Avahi.EntryGroup"

	avahiIfUnspec    = int32(-1)
	avahiProtoUnspec = int32(-1)

	groupEstablished = int32(2)
	groupCollision   = int32(3)
	groupFailure     = int32(4)
)

// avahiEstablishTimeout bounds how long Publish waits for avahi to finish probing. Avahi keeps
// registering in the background past it, so a timeout is not an error.
var avahiEstablishTimeout = 10 * time.Second

// entryGroup is the slice of org.freedesktop.Avahi.EntryGroup that publishing needs.
type entryGroup interface {
	AddService(name, typ, host string, port uint16, txt [][]byte) error
	AddServiceSubtype(name, typ, subtype string) error
	Commit() error
	Reset() error
	// States delivers StateChanged values; it is closed when the group goes away.
	States() <-chan int32
	Free() error
}

func avahiOnSystemBus() bool {
	conn, err := dbus.ConnectSystemBus()
	if err != nil {
		return false
	}
	defer conn.Close()
	var has bool
	if err := conn.BusObject().Call("org.freedesktop.DBus.NameHasOwner", 0, avahiName).Store(&has); err != nil {
		return false
	}
	return has
}

func publishAvahiDBus(ctx context.Context, s Service) (func(), error) {
	g, err := newDBusGroup()
	if err != nil {
		return nil, err
	}
	return publishWithGroup(ctx, g, s)
}

func publishWithGroup(ctx context.Context, g entryGroup, s Service) (func(), error) {
	txt, err := txtBytes(s.TXT)
	if err != nil {
		g.Free()
		return nil, err
	}
	// Avahi only answers A/AAAA for its own (possibly conflict-renamed) host name, so an
	// explicit SRV target equal to this machine's name would go stale after a rename.
	host := ""
	if h := bareHost(s.Host); h != "" && !strings.EqualFold(h, systemHost()) {
		host = h + ".local"
	}
	register := func(name string) error {
		for _, t := range ServiceTypes {
			if err := g.AddService(name, t, host, uint16(s.Port), txt); err != nil {
				return fmt.Errorf("mdns: avahi AddService %s: %w", t, err)
			}
		}
		for _, t := range ServiceTypes {
			for _, sub := range Subtypes {
				if err := g.AddServiceSubtype(name, t, SubtypeName(sub, t)); err != nil {
					return fmt.Errorf("mdns: avahi AddServiceSubtype %s: %w", SubtypeName(sub, t), err)
				}
			}
		}
		if err := g.Commit(); err != nil {
			return fmt.Errorf("mdns: avahi Commit: %w", err)
		}
		return nil
	}

	if err := register(s.Instance); err != nil {
		g.Free()
		return nil, err
	}

	done := make(chan struct{})
	settled := make(chan error, 1)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		n := 1
		report := func(err error) {
			select {
			case settled <- err:
			default:
			}
		}
		for {
			select {
			case <-done:
				return
			case st, ok := <-g.States():
				if !ok {
					report(errors.New("mdns: avahi entry group went away"))
					return
				}
				switch st {
				case groupEstablished:
					report(nil)
				case groupCollision:
					n++
					if n > 100 {
						report(errors.New("mdns: avahi: no free instance name after 100 renames"))
						return
					}
					if err := g.Reset(); err != nil {
						report(fmt.Errorf("mdns: avahi Reset: %w", err))
						return
					}
					if err := register(renamed(s.Instance, n)); err != nil {
						report(err)
						return
					}
				case groupFailure:
					report(errors.New("mdns: avahi entry group failed"))
					return
				}
			}
		}
	}()

	stop := onceFunc(func() {
		close(done)
		wg.Wait()
		g.Free()
	})

	timer := time.NewTimer(avahiEstablishTimeout)
	defer timer.Stop()
	select {
	case err := <-settled:
		if err != nil {
			stop()
			return nil, err
		}
	case <-timer.C:
	case <-ctx.Done():
		stop()
		return nil, ctx.Err()
	}

	go func() {
		select {
		case <-ctx.Done():
			stop()
		case <-done:
		}
	}()
	return stop, nil
}

type dbusGroup struct {
	conn   *dbus.Conn
	obj    dbus.BusObject
	states chan int32
	quit   chan struct{}
}

func newDBusGroup() (*dbusGroup, error) {
	conn, err := dbus.ConnectSystemBus()
	if err != nil {
		return nil, fmt.Errorf("mdns: system bus: %w", err)
	}
	var path dbus.ObjectPath
	if err := conn.Object(avahiName, "/").Call(avahiServer+".EntryGroupNew", 0).Store(&path); err != nil {
		conn.Close()
		return nil, fmt.Errorf("mdns: avahi EntryGroupNew: %w", err)
	}
	if err := conn.AddMatchSignal(
		dbus.WithMatchObjectPath(path),
		dbus.WithMatchInterface(avahiEntryGroup),
		dbus.WithMatchMember("StateChanged"),
	); err != nil {
		conn.Close()
		return nil, fmt.Errorf("mdns: avahi signal match: %w", err)
	}
	sigs := make(chan *dbus.Signal, 16)
	conn.Signal(sigs)
	g := &dbusGroup{conn: conn, obj: conn.Object(avahiName, path), states: make(chan int32, 16), quit: make(chan struct{})}
	go func() {
		defer close(g.states)
		for sig := range sigs {
			if sig.Path != path || sig.Name != avahiEntryGroup+".StateChanged" || len(sig.Body) == 0 {
				continue
			}
			if st, ok := sig.Body[0].(int32); ok {
				select {
				case g.states <- st:
				case <-g.quit:
					return
				}
			}
		}
	}()
	return g, nil
}

func (g *dbusGroup) AddService(name, typ, host string, port uint16, txt [][]byte) error {
	return g.obj.Call(avahiEntryGroup+".AddService", 0,
		avahiIfUnspec, avahiProtoUnspec, uint32(0), name, typ, "", host, port, txt).Err
}

func (g *dbusGroup) AddServiceSubtype(name, typ, subtype string) error {
	return g.obj.Call(avahiEntryGroup+".AddServiceSubtype", 0,
		avahiIfUnspec, avahiProtoUnspec, uint32(0), name, typ, "", subtype).Err
}

func (g *dbusGroup) Commit() error { return g.obj.Call(avahiEntryGroup+".Commit", 0).Err }
func (g *dbusGroup) Reset() error  { return g.obj.Call(avahiEntryGroup+".Reset", 0).Err }

func (g *dbusGroup) States() <-chan int32 { return g.states }

func (g *dbusGroup) Free() error {
	err := g.obj.Call(avahiEntryGroup+".Free", 0).Err
	close(g.quit)
	g.conn.Close()
	return err
}

func systemHost() string {
	h, err := os.Hostname()
	if err != nil {
		return ""
	}
	h, _, _ = strings.Cut(h, ".")
	return h
}
