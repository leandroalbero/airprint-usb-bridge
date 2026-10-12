package pcl

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
)

// Inspect explains a job the driver wrote: PJL lines, PCL commands, and one summary line per
// raster block instead of every row.
func (d *Driver) Inspect(data []byte) (string, error) { return Inspect(data) }

// Inspect decodes the PJL and PCL commands in data. It accepts the subset the driver writes and
// fails on anything else, such as stray text or a row longer than the data.
func Inspect(data []byte) (string, error) {
	var out strings.Builder
	line := func(f string, a ...any) { fmt.Fprintf(&out, f+"\n", a...) }
	var rows, skipped, rowBytes int
	inRaster := false
	pages := 0
	for i := 0; i < len(data); {
		switch {
		case bytes.HasPrefix(data[i:], []byte(uel)):
			line("UEL")
			i += len(uel)
		case bytes.HasPrefix(data[i:], []byte("@PJL")):
			end := bytes.IndexByte(data[i:], '\n')
			if end < 0 {
				return out.String(), fmt.Errorf("pcl: unterminated PJL line at byte %d", i)
			}
			line("%s", strings.TrimRight(string(data[i:i+end]), "\r"))
			i += end + 1
		case data[i] == '\f':
			pages++
			line("FF (page %d)", pages)
			i++
		case data[i] == 0x1b && i+1 < len(data) && data[i+1] == 'E':
			line("ESC E (reset)")
			i += 2
		case data[i] == 0x1b:
			cmds, n, err := parseEscape(data[i:])
			if err != nil {
				return out.String(), fmt.Errorf("pcl: byte %d: %w", i, err)
			}
			i += n
			for _, c := range cmds {
				switch c.key {
				case "*bW":
					if c.value < 0 || i+c.value > len(data) {
						return out.String(), fmt.Errorf("pcl: row of %d bytes past the end at byte %d", c.value, i)
					}
					if !inRaster {
						return out.String(), fmt.Errorf("pcl: raster row outside raster graphics at byte %d", i)
					}
					rows++
					rowBytes += c.value
					i += c.value
				case "*vW":
					if c.value < 0 || i+c.value > len(data) {
						return out.String(), fmt.Errorf("pcl: configure image data of %d bytes past the end at byte %d", c.value, i)
					}
					line("%s (configure image data: % x%s)", c.text, data[i:i+c.value], cidText(data[i:i+c.value]))
					i += c.value
				case "*bY":
					skipped += c.value
				case "*rA":
					inRaster, rows, skipped, rowBytes = true, 0, 0, 0
					line("%s", c.describe())
				case "*rB", "*rC":
					if inRaster {
						line("  raster: %d rows sent (%d bytes), %d rows skipped", rows, rowBytes, skipped)
					}
					inRaster = false
					line("%s", c.describe())
				default:
					line("%s", c.describe())
				}
			}
		default:
			return out.String(), fmt.Errorf("pcl: unexpected byte 0x%02x at %d", data[i], i)
		}
	}
	if inRaster {
		return out.String(), fmt.Errorf("pcl: raster graphics not ended")
	}
	return out.String(), nil
}

type command struct {
	key   string // parameter and group characters plus the upper-case terminator, e.g. "&lA"
	text  string // as written, e.g. "ESC &l26A"
	value int
}

// parseEscape reads one parameterized escape sequence, which may combine several commands of the
// same group ("ESC *p0x0Y" is ESC *p0X and ESC *p0Y).
func parseEscape(b []byte) ([]command, int, error) {
	if len(b) < 3 || b[1] < 0x21 || b[1] > 0x2f || b[2] < 0x60 || b[2] > 0x7e {
		return nil, 0, fmt.Errorf("unknown escape sequence %q", b[:min(len(b), 4)])
	}
	prefix := string(b[1:3])
	var cmds []command
	for i := 3; ; {
		start := i
		if i < len(b) && (b[i] == '+' || b[i] == '-') {
			i++
		}
		for i < len(b) && (b[i] >= '0' && b[i] <= '9' || b[i] == '.') {
			i++
		}
		if i >= len(b) {
			return nil, 0, fmt.Errorf("escape sequence ESC %s cut short", prefix)
		}
		num := string(b[start:i])
		v, _ := strconv.Atoi(strings.SplitN(num, ".", 2)[0])
		t := b[i]
		i++
		switch {
		case t >= 0x40 && t <= 0x5e:
			cmds = append(cmds, command{key: prefix + string(t), text: "ESC " + prefix + num + string(t), value: v})
			return cmds, i, nil
		case t >= 0x60 && t <= 0x7e:
			up := t - 0x20
			cmds = append(cmds, command{key: prefix + string(up), text: "ESC " + prefix + num + string(up), value: v})
		default:
			return nil, 0, fmt.Errorf("bad terminator %q in ESC %s", t, prefix)
		}
	}
}

// cidText names the short-form Configure Image Data settings the driver uses.
func cidText(b []byte) string {
	if len(b) == 6 && b[0] == 0 && b[1] == 3 && b[3] == 8 && b[4] == 8 && b[5] == 8 {
		return "; device RGB, direct by pixel, 24 bits"
	}
	return ""
}

var pageSizes = map[int]string{1: "executive", 2: "letter", 3: "legal", 25: "a5", 26: "a4"}

func (c command) describe() string {
	what := ""
	switch c.key {
	case "&lA":
		what = "page size " + pageSizes[c.value]
		if pageSizes[c.value] == "" {
			what = "page size (unknown)"
		}
	case "&lO":
		what = "orientation"
	case "&lS":
		what = [...]string{"simplex", "duplex long edge", "duplex short edge", "?"}[min(max(c.value, 0), 3)]
	case "&lL":
		what = "perforation skip"
	case "&lE":
		what = "top margin"
	case "&aG":
		what = "duplex side (1 = front)"
	case "*tR":
		what = "raster resolution"
	case "*pX":
		what = "cursor x"
	case "*pY":
		what = "cursor y"
	case "*rS":
		what = "raster width"
	case "*bM":
		what = "compression mode"
	case "*rA":
		what = "start raster graphics"
	case "*rB", "*rC":
		what = "end raster graphics"
	}
	if what == "" {
		return c.text
	}
	return c.text + " (" + what + ")"
}
