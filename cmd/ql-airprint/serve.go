package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/leandroalbero/airprint-usb-bridge/internal/ipp"
	"github.com/leandroalbero/airprint-usb-bridge/internal/mdns"
)

// service is everything serve needs, built before the listener so tests can run it on
// their own listener.
type service struct {
	p      *printer
	srv    *ipp.Server
	cfg    ipp.Config
	status *statusCache
	host   string
}

func newService(o *options, logf logFunc) (*service, error) {
	p, err := newPrinter(o, logf)
	if err != nil {
		return nil, err
	}
	svc, err := newServiceFor(o, p, logf)
	if err != nil {
		p.be.Close()
		return nil, err
	}
	return svc, nil
}

func newServiceFor(o *options, p *printer, logf logFunc) (*service, error) {
	stateDir, err := resolveStateDir(o.stateDir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, fmt.Errorf("state dir: %w", err)
	}
	uuid, err := stableUUID(stateDir)
	if err != nil {
		return nil, err
	}
	presets, err := parsePresets(o.presets)
	if err != nil {
		return nil, err
	}
	host := shortHostname()

	cfg := ipp.DefaultConfig()
	cfg.MakeModel = "Brother " + p.prof.Model
	cfg.Name = o.name
	if cfg.Name == "" {
		cfg.Name = cfg.MakeModel
	}
	cfg.Location = o.location
	cfg.UUID = uuid
	cfg.Host = host + ".local"
	cfg.StateDir = stateDir
	cfg.MaxDocumentBytes = maxDocumentBytes
	cfg.HighRes = o.highRes
	cfg.Presets = presets
	cfg.MediaPolicy = ipp.MediaPolicy(o.mediaPolicy)
	if p.media.WidthMM != 62 {
		cfg.RollWidth = p.media.WidthMM * 100
		side := (cfg.RollWidth - p.media.PrintPins*2540/300) / 2
		cfg.Margins = ipp.Margins{Left: max(side, 0), Right: max(side, 0)}
	}
	status := &statusCache{be: p.be, every: o.statusInterval, logf: logf}
	cfg.Status = status.poll
	cfg.Identify = func(actions []string, msg string) {
		logf("identify-printer: actions=%s message=%q", strings.Join(actions, ","), msg)
	}
	if o.verbose {
		cfg.Logf = logf
	}
	proc := p.processor()
	srv, err := ipp.New(cfg, func(ctx context.Context, job *ipp.Job, doc io.Reader, format string) error {
		defer status.invalidate()
		return proc(ctx, job, doc, format)
	})
	if err != nil {
		return nil, err
	}
	return &service{p: p, srv: srv, cfg: cfg, status: status, host: host}, nil
}

func shortHostname() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "localhost"
	}
	h, _, _ = strings.Cut(h, ".")
	return h
}

func runServe(ctx context.Context, o *options, logf logFunc) error {
	svc, err := newService(o, logf)
	if err != nil {
		return err
	}
	defer svc.p.be.Close()
	l, err := net.Listen("tcp", fmt.Sprintf(":%d", o.port))
	if err != nil {
		shutdownServer(svc.srv, logf)
		return err
	}
	port := l.Addr().(*net.TCPAddr).Port
	target := o.device
	if o.dryRun != "" {
		target = "dry-run " + o.dryRun
	}
	logf("ql-airprint %s: %q (%s, %d mm roll) on port %d, printer %s, uuid %s, state %s",
		version(), svc.cfg.Name, svc.cfg.MakeModel, svc.p.media.WidthMM, port, target,
		strings.TrimPrefix(svc.cfg.UUID, "urn:uuid:"), svc.cfg.StateDir)

	served := make(chan error, 1)
	go func() { served <- svc.srv.Serve(l) }()

	// The advertisement must live as long as the process: cancelling its context
	// withdraws it, so it gets its own and is stopped explicitly below.
	stopMDNS, err := mdns.Publish(context.Background(), o.mdns, mdns.Service{
		Instance: svc.cfg.Name,
		Port:     port,
		Host:     svc.host,
		TXT:      svc.cfg.TXT(svc.host+".local", port),
	})
	if err != nil {
		shutdownServer(svc.srv, logf)
		return fmt.Errorf("mdns: %w", err)
	}
	if o.mdns != mdns.ModeOff {
		logf("advertising %q as _ipp._tcp and _ipps._tcp (mdns %s)", svc.cfg.Name, o.mdns)
	}

	var serveErr error
	select {
	case <-ctx.Done():
		logf("shutting down")
	case serveErr = <-served:
	}
	stopMDNS()
	shutdownServer(svc.srv, logf)
	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		return serveErr
	}
	return nil
}

// shutdownTimeout bounds the wait for the label being printed when the service
// stops. Cancellation stops a job between labels only, and one label may wait
// out the printer cooling down (ql.Timeouts.Cooling, 10 min) after a TCP write
// of up to 2 min. packaging/ql-airprint.service sets TimeoutStopSec above it.
const shutdownTimeout = 15 * time.Minute

func shutdownServer(srv *ipp.Server, logf logFunc) {
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		logf("shutdown: %v", err)
	}
}
