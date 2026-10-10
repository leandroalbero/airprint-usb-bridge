package driver

import (
	"errors"
	"fmt"
	"sync"
)

// MaxSheetBytes is the bridge's budget per job for sheets placed at the printer's resolution
// (one byte of gray per dot): about 15 A4 sheets at 300 dpi, or 3 at 600. Placed sheets, not the
// decoded document, are what the output grows with: a page of a few pixels at 1 dpi still
// becomes a whole sheet. A driver should not advertise a size whose single sheet exceeds it.
const MaxSheetBytes = 128 << 20

// ErrJobTooLarge marks a job refused because its sheets would exceed the job's Budget.
var ErrJobTooLarge = errors.New("job too large")

// Budget is what is left of a job's allowance for placed sheets. A nil *Budget allows anything.
type Budget struct {
	mu          sync.Mutex
	limit, used int64
}

// NewBudget returns a budget of limit bytes.
func NewBudget(limit int64) *Budget { return &Budget{limit: limit} }

// Charge takes n bytes from the budget, or fails with ErrJobTooLarge (marked as a document error)
// when they are not there. A negative n is an overflowed size and always fails.
func (b *Budget) Charge(n int64) error {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if n < 0 || n > b.limit-b.used {
		return DocumentError(fmt.Errorf("%w: its sheets need more than %d MiB at the printer's resolution "+
			"(about %d A4 sheets at 300 dpi); print fewer pages or at a lower resolution", ErrJobTooLarge, b.limit>>20, b.limit/a4Dots300))
	}
	b.used += n
	return nil
}

// Release returns n bytes charged earlier, once the sheet they paid for is gone; a driver that
// streams pages releases each sheet after sending it, so the budget bounds one page at a time.
func (b *Budget) Release(n int64) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.used = max(b.used-n, 0)
}

// Check fails like Charge when a single allocation of n bytes is larger than the whole budget,
// without taking anything from it.
func (b *Budget) Check(n int64) error {
	if b == nil || (n >= 0 && n <= b.limit) {
		return nil
	}
	return DocumentError(fmt.Errorf("%w: a page needs %d MiB at the printer's resolution, over the %d MiB a job may use",
		ErrJobTooLarge, n>>20, b.limit>>20))
}

const a4Dots300 = 2480 * 3508
