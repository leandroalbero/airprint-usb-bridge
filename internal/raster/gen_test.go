package raster

import (
	"math/rand"

	"github.com/leandroalbero/airprint-usb-bridge/internal/raster/testutil"
)

func whiteGray(w, h int) []byte {
	g := make([]byte, w*h)
	for i := range g {
		g[i] = 255
	}
	return g
}

func fillRect(g []byte, w, x0, y0, rw, rh int, v byte) {
	for y := y0; y < y0+rh; y++ {
		for x := x0; x < x0+rw; x++ {
			g[y*w+x] = v
		}
	}
}

// textGray draws glyph-like strokes inside the margins. With grid > 1 every edge
// lands on a multiple of grid plus parity, which pins the 600 dpi pairing phase.
func textGray(seed int64, w, h, margin, grid, parity int, antialias bool) []byte {
	r := rand.New(rand.NewSource(seed))
	g := whiteGray(w, h)
	snap := func(v int) int { return v - v%grid + parity }
	left, right := w/10, w-w/10
	for i := 0; i < 160; i++ {
		rw := grid * (1 + r.Intn(12/grid+1))
		rh := grid * (1 + r.Intn(30/grid+1))
		if right-left-rw <= grid || h-2*margin-rh <= grid {
			continue
		}
		x := snap(left + r.Intn(right-left-rw))
		y := snap(margin + r.Intn(h-2*margin-rh))
		if x < left || y < margin || x+rw > right || y+rh > h-margin {
			continue
		}
		fillRect(g, w, x, y, rw, rh, 0)
	}
	if antialias {
		for i := 0; i < w*h/400; i++ {
			g[r.Intn(w*(h-2*margin))+w*margin] = byte(64 + r.Intn(128))
		}
	}
	return g
}

func gradientGray(seed int64, w, h int) []byte {
	r := rand.New(rand.NewSource(seed))
	g := make([]byte, w*h)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			v := x*255/w + (y*40)/h - 20 + r.Intn(17) - 8
			g[y*w+x] = byte(max(0, min(255, v)))
		}
	}
	return g
}

// colourize expands gray to bpp samples whose first three channels average back
// to roughly the gray value, plus noise in the ignored fourth channel.
func colourize(seed int64, gray []byte, bpp int, subtractive bool) []byte {
	r := rand.New(rand.NewSource(seed))
	bypp := bpp / 8
	out := make([]byte, len(gray)*bypp)
	for i, v := range gray {
		c := int(v)
		if subtractive {
			c = 255 - c
		}
		if bypp == 1 {
			out[i] = byte(c)
			continue
		}
		d := r.Intn(21) - 10
		ch := [3]int{c + d, c - d, c}
		for k := 0; k < 3; k++ {
			out[i*bypp+k] = byte(max(0, min(255, ch[k])))
		}
		if bypp == 4 {
			out[i*bypp+3] = byte(r.Intn(256))
		}
	}
	return out
}

func colourPage(seed int64, w, h, dpi, bpp, cs int, subtractive bool, gray []byte) testutil.Page {
	return testutil.Page{Width: w, Height: h, XDPI: dpi, BPP: bpp, ColorSpace: cs, Pix: colourize(seed, gray, bpp, subtractive)}
}

func invert(g []byte) []byte {
	out := make([]byte, len(g))
	for i, v := range g {
		out[i] = 255 - v
	}
	return out
}
