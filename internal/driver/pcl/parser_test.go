package pcl

import (
	"bytes"
	"fmt"
	"strconv"
	"testing"
)

// This file is a small, strict PCL reader written from the PCL 5 reference independently of the
// encoder: it walks the escape sequences, keeps the printer state the driver relies on, and
// rebuilds every page's raster so tests can compare it dot for dot with the input.

type pclPage struct {
	sizeCode    int
	dpi         int
	duplex      int // ESC &l#S in force; -1 when never sent
	frontSide   bool
	compression int
	width       int      // ESC *r#S, dots
	rows        [][]byte // decoded rows, index = raster row from the top of the logical page
}

type pclJob struct {
	pjl    []string
	resets int
	pages  []pclPage
}

func decodePCL(t testing.TB, data []byte) pclJob {
	t.Helper()
	job, err := parsePCL(data)
	if err != nil {
		t.Fatalf("parse PCL: %v", err)
	}
	return job
}

func parsePCL(data []byte) (pclJob, error) {
	var job pclJob
	st := struct {
		size, dpi, duplex, compression, width int
		orient                                int
		front                                 bool
		cursorSet                             int // bit 1: X set to 0, bit 2: Y set to 0
		inRaster                              bool
		seed                                  []byte
		rows                                  [][]byte
		y                                     int
		drawn                                 bool
	}{duplex: -1, orient: -1}
	stride := func() int { return (st.width + 7) / 8 }
	for i := 0; i < len(data); {
		switch {
		case bytes.HasPrefix(data[i:], []byte("\x1b%-12345X")):
			i += 9
		case bytes.HasPrefix(data[i:], []byte("@PJL")):
			end := bytes.Index(data[i:], []byte("\r\n"))
			if end < 0 {
				return job, fmt.Errorf("PJL line without CR LF at %d", i)
			}
			job.pjl = append(job.pjl, string(data[i:i+end]))
			i += end + 2
		case data[i] == '\f':
			if st.inRaster {
				return job, fmt.Errorf("FF inside raster graphics at %d", i)
			}
			if st.size == 0 || st.dpi == 0 || st.orient != 0 {
				return job, fmt.Errorf("page without size, resolution or portrait orientation at %d", i)
			}
			p := pclPage{sizeCode: st.size, dpi: st.dpi, duplex: st.duplex, frontSide: st.front,
				compression: st.compression, width: st.width}
			if st.drawn {
				p.rows = st.rows
			}
			job.pages = append(job.pages, p)
			st.front, st.drawn, st.rows, st.cursorSet = false, false, nil, 0
			i++
		case bytes.HasPrefix(data[i:], []byte("\x1bE")):
			if st.inRaster || st.drawn {
				return job, fmt.Errorf("reset with an unfinished page at %d", i)
			}
			job.resets++
			st.size, st.dpi, st.duplex, st.compression, st.width, st.orient = 0, 0, -1, 0, 0, -1
			i += 2
		case data[i] == 0x1b && i+2 < len(data):
			group := string(data[i+1 : i+3])
			i += 3
			for done := false; !done; {
				start := i
				for i < len(data) && data[i] >= '0' && data[i] <= '9' {
					i++
				}
				if i >= len(data) {
					return job, fmt.Errorf("ESC %s: cut short at %d", group, start)
				}
				v, _ := strconv.Atoi(string(data[start:i]))
				term := data[i]
				i++
				if term >= 'a' && term <= 'z' {
					term -= 0x20
				} else {
					done = true
				}
				switch group + string(term) {
				case "&lA":
					st.size = v
				case "&lO":
					st.orient = v
				case "&lS":
					st.duplex = v
				case "&lL", "&lE":
					if v != 0 {
						return job, fmt.Errorf("ESC %s%d%c: want 0", group, v, term)
					}
				case "&aG":
					if v != 1 {
						return job, fmt.Errorf("duplex side %d", v)
					}
					st.front = true
				case "*tR":
					st.dpi = v
				case "*pX":
					if v != 0 {
						return job, fmt.Errorf("cursor x %d", v)
					}
					st.cursorSet |= 1
				case "*pY":
					if v != 0 {
						return job, fmt.Errorf("cursor y %d", v)
					}
					st.cursorSet |= 2
				case "*rS":
					st.width = v
				case "*bM":
					if v != 2 && v != 3 {
						return job, fmt.Errorf("compression %d", v)
					}
					st.compression = v
				case "*rA":
					if v != 1 || st.cursorSet != 3 || st.width == 0 || st.compression == 0 {
						return job, fmt.Errorf("start raster %d without cursor 0,0, width and compression", v)
					}
					st.inRaster, st.drawn = true, true
					st.seed, st.rows, st.y = make([]byte, stride()), nil, 0
				case "*rB":
					if !st.inRaster {
						return job, fmt.Errorf("end raster outside raster graphics")
					}
					st.inRaster = false
				case "*bY":
					if !st.inRaster {
						return job, fmt.Errorf("Y offset outside raster graphics")
					}
					if st.compression == 3 {
						for _, b := range st.seed {
							if b != 0 {
								return job, fmt.Errorf("mode 3 Y offset with a non-zero seed row: printers differ on clearing it")
							}
						}
					}
					st.y += v
				case "*bW":
					if !st.inRaster || i+v > len(data) {
						return job, fmt.Errorf("row of %d bytes at %d", v, i)
					}
					payload := data[i : i+v]
					i += v
					var row []byte
					var err error
					if st.compression == 2 {
						row, err = unpackMode2(payload, stride())
					} else {
						row, err = applyMode3(payload, st.seed)
					}
					if err != nil {
						return job, fmt.Errorf("row %d: %w", st.y, err)
					}
					for len(st.rows) < st.y {
						st.rows = append(st.rows, make([]byte, stride()))
					}
					st.rows = append(st.rows, row)
					st.seed = append([]byte(nil), row...)
					st.y++
				default:
					return job, fmt.Errorf("unexpected command ESC %s%d%c", group, v, term)
				}
			}
		default:
			return job, fmt.Errorf("unexpected byte 0x%02x at %d", data[i], i)
		}
	}
	if st.inRaster || st.drawn {
		return job, fmt.Errorf("unfinished page at the end")
	}
	return job, nil
}

// unpackMode2 decodes TIFF PackBits: 0..127 copies n+1 literal bytes, -1..-127 repeats the next
// byte 1-n times, -128 does nothing. A short row is zero-filled to the raster width.
func unpackMode2(src []byte, width int) ([]byte, error) {
	var out []byte
	for i := 0; i < len(src); {
		n := int(int8(src[i]))
		i++
		switch {
		case n >= 0:
			if i+n+1 > len(src) {
				return nil, fmt.Errorf("literal past the end")
			}
			out = append(out, src[i:i+n+1]...)
			i += n + 1
		case n > -128:
			if i >= len(src) {
				return nil, fmt.Errorf("repeat past the end")
			}
			out = append(out, bytes.Repeat(src[i:i+1], 1-n)...)
			i++
		}
	}
	if len(out) > width {
		return nil, fmt.Errorf("%d bytes for a %d-byte row", len(out), width)
	}
	return append(out, make([]byte, width-len(out))...), nil
}

// applyMode3 applies delta-row commands to a copy of seed.
func applyMode3(src, seed []byte) ([]byte, error) {
	row := append([]byte(nil), seed...)
	pos := 0
	for i := 0; i < len(src); {
		cmd := src[i]
		i++
		count := int(cmd>>5) + 1
		off := int(cmd & 31)
		if off == 31 {
			for {
				if i >= len(src) {
					return nil, fmt.Errorf("offset past the end")
				}
				b := int(src[i])
				i++
				off += b
				if b != 255 {
					break
				}
			}
		}
		pos += off
		if i+count > len(src) || pos+count > len(row) {
			return nil, fmt.Errorf("replacement past the end")
		}
		copy(row[pos:], src[i:i+count])
		i += count
		pos += count
	}
	return row, nil
}
