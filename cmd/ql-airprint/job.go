package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/leandroalbero/airprint-usb-bridge/internal/backend"
	"github.com/leandroalbero/airprint-usb-bridge/internal/ipp"
	"github.com/leandroalbero/airprint-usb-bridge/internal/ql"
	"github.com/leandroalbero/airprint-usb-bridge/internal/raster"
)

type logFunc func(format string, args ...any)

// Memory caps sized for a 1 GB board: 62 mm x 1 m at 600x600 dpi is 35 MB of
// gray, and label documents are a few MB at most. Every page of a job is decoded
// before the first label prints, so the gray cap bounds the whole job (about 150
// labels of 62x100 mm at 300 dpi). packaging/ql-airprint.service sets MemoryMax
// and GOMEMLIMIT to match.
const (
	maxDocumentBytes = 64 << 20
	maxGrayBytes     = 128 << 20
)

// printer turns documents into labels and sends them to the backend.
type printer struct {
	prof     *ql.Profile
	media    ql.Media
	be       backend.Backend
	opts     raster.Options // HighRes is set per job
	highRes  bool           // print-quality high is offered
	retry    time.Duration
	debugDir string
	logf     logFunc
}

func newPrinter(o *options, logf logFunc) (*printer, error) {
	prof, err := loadProfile(o.profile)
	if err != nil {
		return nil, err
	}
	media, ok := prof.MediaByID(o.media)
	if !ok {
		var ids []string
		for _, m := range prof.Media {
			ids = append(ids, m.ID)
		}
		return nil, fmt.Errorf("profile %s has no media %q (have %s)", prof.Model, o.media, strings.Join(ids, ", "))
	}
	if media.Kind != ql.MediaContinuous {
		return nil, fmt.Errorf("media %q is not a continuous roll; only rolls are supported", o.media)
	}
	trim, err := parseTrim(o.trim)
	if err != nil {
		return nil, err
	}
	dither, err := parseDither(o.dither)
	if err != nil {
		return nil, err
	}
	if o.debugDir != "" {
		if err := os.MkdirAll(o.debugDir, 0o700); err != nil {
			return nil, err
		}
	}
	// Clients rasterise the full roll width (6200/2540*300 = 732 dots for 62 mm);
	// anything far from it means the page arrived on the wrong axis or media.
	full := int(math.Round(float64(media.WidthMM) * 300 / 25.4))
	return &printer{
		prof:  prof,
		media: media,
		be:    newBackend(o, prof),
		opts: raster.Options{
			Pins:     media.PrintPins,
			WidthMin: full - 8,
			WidthMax: full + 8,
			TrimMax:  trim,
			Dither:   dither,
		},
		highRes:  o.highRes,
		retry:    o.retryDeadline,
		debugDir: o.debugDir,
		logf:     logf,
	}, nil
}

func loadProfile(name string) (*ql.Profile, error) {
	switch strings.ToLower(strings.ReplaceAll(name, "-", "")) {
	case "", "ql700":
		return ql.EmbeddedProfile()
	}
	return ql.LoadProfile(name)
}

func newBackend(o *options, prof *ql.Profile) backend.Backend {
	switch {
	case o.dryRun != "":
		return backend.NewDryRun(prof, o.dryRun)
	case strings.HasPrefix(o.device, "tcp://"):
		return backend.NewTCP(prof, o.device)
	}
	return backend.NewUSB(prof, o.device)
}

// printDocument converts and checks every page before the first label prints, so a bad
// page never leaves a half-printed job on the roll. Each non-blank page is then printed
// as its own device job. onLabel gets the running count after each printed label.
func (p *printer) printDocument(ctx context.Context, data []byte, highRes bool, logf logFunc, debugPrefix string, onLabel func(int)) (int, error) {
	pages, err := raster.DecodeLimited(data, raster.Limits{
		MaxGrayBytes: maxGrayBytes,
		WidthMin:     p.opts.WidthMin,
		WidthMax:     p.opts.WidthMax,
	})
	if errors.Is(err, raster.ErrNoPages) {
		logf("document has no pages; nothing to print")
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("%w: %w", ipp.ErrDocumentFormat, err)
	}
	opts := p.opts
	opts.HighRes = highRes
	type label struct {
		page int
		bmp  *ql.Bitmap
	}
	var labels []label
	// One 600 dpi page puts the whole job in high-res mode, so every label of a
	// job is fed at the same resolution.
	jobHighRes := false
	for i, pg := range pages {
		n := i + 1
		logf("page %d: %dx%d @%dx%ddpi", n, pg.Width, pg.Height, pg.XDPI, pg.YDPI)
		if highRes && pg.XDPI == 600 {
			jobHighRes = true
		}
		lab, err := raster.Prepare(pg, opts)
		pages[i].Gray = nil
		if err != nil {
			if errors.Is(err, raster.ErrWidth) || errors.Is(err, raster.ErrResolution) {
				err = fmt.Errorf("%w: %w", ipp.ErrDocumentFormat, err)
			}
			return 0, fmt.Errorf("page %d: %w; not printing", n, err)
		}
		if pg.XDPI == 600 {
			mode := "300x300"
			if lab.HighRes {
				mode = "300x600 high-res"
			}
			logf("page %d: 600 dpi reduced to %s", n, mode)
		}
		if lab.Dithered {
			logf("page %d: dithered (photo-like content)", n)
		}
		if lab.Dropped > 0 {
			logf("page %d: %d black dots outside the %d printable columns were dropped", n, lab.Dropped, opts.Pins)
		}
		if lab.Blank {
			logf("page %d is blank; skipped", n)
			continue
		}
		dpi := 300.0
		if lab.HighRes {
			dpi = 600
		}
		logf("page %d: label %d dots long (%.2f mm)", n, lab.Bitmap.H, float64(lab.Bitmap.H)*25.4/dpi)
		if p.debugDir != "" {
			path := filepath.Join(p.debugDir, fmt.Sprintf("%s-p%d-label.png", debugPrefix, n))
			if err := writePNG(path, lab.Bitmap); err != nil {
				logf("debug: %v", err)
			}
		}
		labels = append(labels, label{n, lab.Bitmap})
	}
	printed := 0
	for i, l := range labels {
		if err := ctx.Err(); err != nil {
			return printed, err
		}
		job := backend.JobFor(p.prof, p.media, l.bmp, jobHighRes)
		retryLog := func(s string) { logf("label %d: %s", i+1, s) }
		if err := backend.PrintWithRetry(ctx, p.be, job, p.retry, retryLog); err != nil {
			return printed, fmt.Errorf("label %d of %d (page %d): %w", i+1, len(labels), l.page, err)
		}
		printed++
		logf("label %d of %d printed (page %d)", i+1, len(labels), l.page)
		if onLabel != nil {
			onLabel(printed)
		}
	}
	return printed, nil
}

func (p *printer) processor() ipp.Processor {
	return func(ctx context.Context, job *ipp.Job, doc io.Reader, format string) error {
		jlog := func(f string, args ...any) {
			p.logf("job %d: "+f, append([]any{job.ID}, args...)...)
		}
		data, err := readDoc(doc)
		if err != nil {
			return err
		}
		a := job.Attrs
		mediaCol := "-"
		if mc := a.MediaCol; mc != nil {
			mediaCol = fmt.Sprintf("%dx%d", mc.XDimension, mc.YDimension)
			if mc.SizeName != "" {
				mediaCol += " " + mc.SizeName
			}
		}
		jlog("%q from %s: %s, %d bytes, media=%q media-col=%s print-quality=%d",
			job.Name, job.User, format, len(data), a.Media, mediaCol, a.PrintQuality)
		prefix := fmt.Sprintf("job%d", job.ID)
		if p.debugDir != "" {
			ext := ".urf"
			if format == "image/pwg-raster" {
				ext = ".pwg"
			}
			if err := os.WriteFile(filepath.Join(p.debugDir, prefix+"-in"+ext), data, 0o600); err != nil {
				jlog("debug: %v", err)
			}
		}
		highRes := p.highRes && a.PrintQuality == 5
		job.SetStateMessage("Converting.")
		n, err := p.printDocument(ctx, data, highRes, jlog, prefix, func(n int) {
			job.SetImpressions(n)
			job.SetStateMessage(fmt.Sprintf("Printed %d label(s).", n))
		})
		if err != nil {
			jlog("failed after %d label(s): %v", n, err)
			job.SetStateMessage(err.Error())
			return err
		}
		jlog("completed, %d label(s)", n)
		return nil
	}
}

// readDoc reads a spooled document in one allocation when its size is known;
// io.ReadAll would grow to twice a large document's size.
func readDoc(doc io.Reader) ([]byte, error) {
	if f, ok := doc.(interface{ Stat() (os.FileInfo, error) }); ok {
		if st, err := f.Stat(); err == nil && st.Mode().IsRegular() && st.Size() <= maxDocumentBytes {
			buf := make([]byte, st.Size())
			if _, err := io.ReadFull(doc, buf); err != nil {
				return nil, err
			}
			return buf, nil
		}
	}
	return io.ReadAll(doc)
}
