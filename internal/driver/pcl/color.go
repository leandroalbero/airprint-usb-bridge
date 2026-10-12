package pcl

import (
	"bufio"
	"fmt"
	"io"
	"strconv"

	"github.com/leandroalbero/airprint-usb-bridge/internal/driver"
	"github.com/leandroalbero/airprint-usb-bridge/internal/imaging"
	"github.com/leandroalbero/airprint-usb-bridge/internal/raster"
)

// streamColor is Stream for a colour job: each page is sent as 24-bit RGB rows (the prologue
// carries the Configure Image Data command), written as they are encoded so a page's raster is
// never held whole.
func (d *Driver) streamColor(job driver.JobInfo, pages driver.PageReader, p paper, dpi int, out *driver.Output, h jobHeader) error {
	end := epilogue(h)
	bw := bufio.NewWriterSize(out, 64<<10)
	fail := func(err error) error {
		_ = bw.Flush()
		return out.Abort(err, end)
	}
	var cw colorWriter
	var reuse []byte
	for n := 1; ; n++ {
		if err := out.Err(); err != nil {
			return fail(err)
		}
		pg, err := pages.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fail(err)
		}
		sheet, held, err := d.colorSheet(job, pg, p, dpi, n, &reuse)
		if err != nil {
			return fail(err)
		}
		if n == 1 {
			_, _ = bw.Write(prologue(h))
		}
		size, err := cw.page(bw, sheet)
		job.Budget.Release(held)
		if err != nil {
			_ = bw.Flush()
			return out.Abort(err, nil)
		}
		job.Log("page %d: %s %dx%d dots at %d dpi, colour, %d bytes", n, p.alias, sheet.Width, sheet.Height, dpi, size)
	}
	if out.Written() == 0 && bw.Buffered() == 0 {
		return driver.DocumentErrorf("pcl: no pages")
	}
	if _, err := bw.Write(end); err != nil {
		return out.Abort(err, nil)
	}
	if err := bw.Flush(); err != nil {
		return out.Abort(err, nil)
	}
	return nil
}

// colorSheet places page n on the sheet at dpi and crops it to the logical page's columns. It
// returns the Budget bytes the sheet holds, for the caller to release.
func (d *Driver) colorSheet(job driver.JobInfo, pg raster.Page, p paper, dpi, n int, reuse *[]byte) (raster.Page, int64, error) {
	o := d.placeOptions(job, dpi)
	o.InPlace, o.Reuse = true, reuse
	sheet, err := imaging.Place(pg, job.Media, o)
	if err != nil {
		return raster.Page{}, 0, driver.DocumentError(fmt.Errorf("pcl: page %d: %w", n, err))
	}
	pix, ch := sheet.Pix()
	held := int64(sheet.Width) * int64(sheet.Height) * int64(ch)
	off, lw := p.logical(dpi)
	if off+lw > sheet.Width {
		job.Budget.Release(held)
		return raster.Page{}, 0, fmt.Errorf("pcl: page %d: logical page %d+%d dots wider than the %d-dot sheet", n, off, lw, sheet.Width)
	}
	// Crop in place: each row moves to an earlier or equal index, so a forward copy is safe.
	for y := 0; y < sheet.Height; y++ {
		copy(pix[ch*y*lw:ch*(y+1)*lw], pix[ch*(y*sheet.Width+off):ch*(y*sheet.Width+off+lw)])
	}
	pix = pix[:ch*lw*sheet.Height]
	if ch == 3 {
		sheet.RGB = pix
	} else {
		sheet.Gray = pix
	}
	sheet.Width = lw
	if job.Debug != nil {
		job.DebugImage(fmt.Sprintf("p%d", n), imaging.PageImage(sheet))
	}
	return sheet, held, nil
}

// colorWriter encodes pages as 24-bit RGB raster rows with delta row compression (mode 3), the
// mode the PCL 5 Color reference uses with 24-bit raster. In Device RGB zero is black, and
// short rows and ESC *b#Y are filled with zeros, so every row sent is full width and white rows
// are only left out after the last row with ink.
type colorWriter struct {
	seed, row, buf []byte
}

// page writes one logical page and returns the bytes written.
func (c *colorWriter) page(w io.Writer, sheet raster.Page) (int, error) {
	pix, ch := sheet.Pix()
	width := sheet.Width
	n := 3 * width
	c.seed = append(c.seed[:0], make([]byte, n)...)
	if cap(c.row) < n {
		c.row = make([]byte, n)
	}
	row := c.row[:n]
	rowOf := func(y int) []byte {
		if ch == 3 {
			return pix[y*n : (y+1)*n]
		}
		for x, v := range pix[y*width : (y+1)*width] {
			row[3*x], row[3*x+1], row[3*x+2] = v, v, v
		}
		return row
	}
	last := -1
	for y := sheet.Height - 1; y >= 0 && last < 0; y-- {
		for _, v := range pix[ch*y*width : ch*(y+1)*width] {
			if v != 255 {
				last = y
				break
			}
		}
	}
	written := 0
	put := func(b []byte) error {
		k, err := w.Write(b)
		written += k
		return err
	}
	head := "\x1b*p0x0Y\x1b*r" + strconv.Itoa(width) + "S\x1b*b3M\x1b*r1A"
	if err := put([]byte(head)); err != nil {
		return written, err
	}
	for y := 0; y <= last; y++ {
		r := rowOf(y)
		c.buf = deltaRow(c.buf[:0], r, c.seed)
		copy(c.seed, r)
		if err := put([]byte("\x1b*b" + strconv.Itoa(len(c.buf)) + "W")); err != nil {
			return written, err
		}
		if err := put(c.buf); err != nil {
			return written, err
		}
	}
	err := put([]byte("\x1b*rB\f"))
	return written, err
}
