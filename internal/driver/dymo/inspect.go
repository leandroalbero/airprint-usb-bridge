package dymo

import (
	"errors"
	"fmt"
	"math/bits"
	"strings"
)

// Inspect decodes a LabelWriter command stream into one line of " | "-separated steps, for dry
// runs and tests. Raster lines are summarised as a count with their black dots.
func (d *Driver) Inspect(data []byte) (string, error) { return Decode(data) }

// Decode is Inspect without a driver.
func Decode(data []byte) (string, error) {
	var steps []string
	bpl := 84
	lines, ink := 0, 0
	flush := func() {
		if lines > 0 {
			steps = append(steps, fmt.Sprintf("lines %d (%d dots)", lines, ink))
			lines, ink = 0, 0
		}
	}
	step := func(s string, a ...any) { flush(); steps = append(steps, fmt.Sprintf(s, a...)) }
	need := func(i, n int) error {
		if i+n > len(data) {
			return fmt.Errorf("truncated at byte %d", i)
		}
		return nil
	}
	fail := func(err error) (string, error) { flush(); return strings.Join(steps, " | "), err }

	for i := 0; i < len(data); {
		switch b := data[i]; b {
		case syn:
			if err := need(i+1, bpl); err != nil {
				return fail(err)
			}
			for _, v := range data[i+1 : i+1+bpl] {
				ink += bits.OnesCount8(v)
			}
			lines++
			i += 1 + bpl
		case etb:
			n, black, err := compressedLine(data[i+1:], bpl*8)
			if err != nil {
				return fail(fmt.Errorf("byte %d: %w", i, err))
			}
			ink += black
			lines++
			i += 1 + n
		case esc:
			if err := need(i, 2); err != nil {
				return fail(err)
			}
			if data[i+1] == esc {
				j := i
				for j < len(data) && data[j] == esc {
					j++
				}
				// The run ends with the ESC that introduces the next command.
				if j < len(data) {
					j--
				}
				step("resync %d", j-i)
				i = j
				continue
			}
			cmd := data[i+1]
			i += 2
			arg := func(n int) ([]byte, error) {
				if err := need(i, n); err != nil {
					return nil, err
				}
				i += n
				return data[i-n : i], nil
			}
			switch cmd {
			case 'B':
				a, err := arg(1)
				if err != nil {
					return fail(err)
				}
				step("dot-tab %d", a[0])
			case 'D':
				a, err := arg(1)
				if err != nil {
					return fail(err)
				}
				bpl = int(a[0])
				if bpl == 0 {
					return fail(errors.New("bytes-per-line 0"))
				}
				step("bytes-per-line %d", bpl)
			case 'L':
				a, err := arg(2)
				if err != nil {
					return fail(err)
				}
				if n := int(a[0])<<8 | int(a[1]); n >= 0x8000 {
					step("label-length continuous")
				} else {
					step("label-length %d", n)
				}
			case 'f':
				a, err := arg(2)
				if err != nil {
					return fail(err)
				}
				step("skip %d lines", a[1])
			case 'q':
				a, err := arg(1)
				if err != nil {
					return fail(err)
				}
				step("roll %c", a[0])
			case 'E':
				step("form-feed")
			case 'G':
				step("short-form-feed")
			case 'h':
				step("text-mode 300x300")
			case 'i':
				step("graphics-mode 300x600")
			case 'c', 'd', 'e', 'g':
				step("density %s", densityName(cmd))
			case '@':
				step("reset")
			case '*':
				step("restore-defaults")
			case 'A':
				step("status?")
			case 'V':
				step("revision?")
			default:
				return fail(fmt.Errorf("unknown command ESC 0x%02x at byte %d", cmd, i-2))
			}
		default:
			return fail(fmt.Errorf("unexpected byte 0x%02x at byte %d", b, i))
		}
	}
	flush()
	return strings.Join(steps, " | "), nil
}

// compressedLine reads one <etb> line (Appendix A: bit 7 colour, bits 0-6 run length - 1) of
// exactly want pixels and returns the bytes used and the black pixels.
func compressedLine(p []byte, want int) (n, black int, err error) {
	got := 0
	for got < want {
		if n >= len(p) {
			return 0, 0, errors.New("truncated compressed line")
		}
		run := int(p[n]&0x7f) + 1
		if p[n]&0x80 != 0 {
			black += run
		}
		got += run
		n++
	}
	if got != want {
		return 0, 0, fmt.Errorf("compressed line has %d pixels, want %d", got, want)
	}
	return n, black, nil
}

func densityName(c byte) string {
	for k, v := range densities {
		if v == c {
			return k
		}
	}
	return "?"
}
