package ipp

import (
	"bytes"
	"html/template"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"sync"
	"time"
)

// Handler serves IPP on POST /ipp/print and the small web interface.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+printerPath, s.serveIPP)
	mux.HandleFunc("POST "+printerPath+"/", s.serveIPP)
	mux.HandleFunc("GET /{$}", s.serveStatus)
	mux.HandleFunc("GET /supplies", s.serveStatus)
	mux.HandleFunc("GET /en.strings", serveStrings)
	mux.HandleFunc("GET /icon.png", serveIcon(128))
	mux.HandleFunc("GET /icon-sm.png", serveIcon(48))
	mux.HandleFunc("GET /icon-lg.png", serveIcon(512))
	return mux
}

var statusTmpl = template.Must(template.New("status").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>{{.Name}}</title>
<style>
body{font:15px/1.4 system-ui,sans-serif;margin:1rem;max-width:48rem;color:#222;background:#fff}
table{border-collapse:collapse;width:100%}td,th{text-align:left;padding:.25rem .5rem;border-bottom:1px solid #ddd}
@media (prefers-color-scheme:dark){body{color:#ddd;background:#111}td,th{border-color:#333}}
</style></head><body>
<h1>{{.Name}}</h1>
<p>{{.MakeModel}}{{if .Location}} &middot; {{.Location}}{{end}}</p>
<p>State: <b>{{.State}}</b> &middot; {{.Message}}{{range .Reasons}}{{if ne . "none"}} &middot; {{.}}{{end}}{{end}}</p>
<h2>Jobs</h2>
{{if .Jobs}}<table><tr><th>#</th><th>Name</th><th>User</th><th>State</th><th>Labels</th><th>Message</th><th>Created</th></tr>
{{range .Jobs}}<tr><td>{{.ID}}</td><td>{{.Name}}</td><td>{{.User}}</td><td>{{.State}}</td><td>{{.Impressions}}</td><td>{{.Message}}</td><td>{{.Created.Format "2006-01-02 15:04:05"}}</td></tr>
{{end}}</table>{{else}}<p>No jobs.</p>{{end}}
</body></html>
`))

func (s *Server) serveStatus(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	st := s.statusLocked()
	s.mu.Unlock()
	jobs := s.Jobs()
	if len(jobs) > 10 {
		jobs = jobs[:10]
	}
	data := struct {
		Name, MakeModel, Location, State, Message string
		Reasons                                   []string
		Jobs                                      []JobInfo
	}{s.cfg.Name, s.cfg.MakeModel, s.cfg.Location, st.state.String(), st.message, st.reasons, jobs}
	var buf bytes.Buffer
	if err := statusTmpl.Execute(&buf, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(buf.Bytes())
}

// en.strings localizes keywords in the attributes; only the roll media type is
// not a standard keyword.
const enStrings = `"media-type.roll" = "Roll";
"media-source.main" = "Roll";
`

func serveStrings(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/strings; charset=utf-8")
	_, _ = w.Write([]byte(enStrings))
}

var (
	iconMu    sync.Mutex
	iconCache = map[int][]byte{}
)

func serveIcon(size int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		iconMu.Lock()
		b, ok := iconCache[size]
		if !ok {
			b = drawIcon(size)
			iconCache[size] = b
		}
		iconMu.Unlock()
		w.Header().Set("Content-Type", "image/png")
		http.ServeContent(w, r, "icon.png", time.Time{}, bytes.NewReader(b))
	}
}

// drawIcon renders a label printer: a dark body with a white label coming out.
func drawIcon(size int) []byte {
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	body := color.NRGBA{0x3a, 0x3f, 0x47, 0xff}
	slot := color.NRGBA{0x1e, 0x21, 0x26, 0xff}
	label := color.NRGBA{0xff, 0xff, 0xff, 0xff}
	ink := color.NRGBA{0x22, 0x22, 0x22, 0xff}
	fill := func(x0, y0, x1, y1 float64, c color.NRGBA) {
		for y := int(y0 * float64(size)); y < int(y1*float64(size)); y++ {
			for x := int(x0 * float64(size)); x < int(x1*float64(size)); x++ {
				img.SetNRGBA(x, y, c)
			}
		}
	}
	fill(0.10, 0.30, 0.90, 0.80, body)
	fill(0.20, 0.38, 0.80, 0.44, slot)
	fill(0.25, 0.08, 0.75, 0.41, label)
	fill(0.32, 0.16, 0.68, 0.20, ink)
	fill(0.32, 0.25, 0.58, 0.29, ink)
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}
