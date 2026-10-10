package ql

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// PrintObserved is Print with progress, precheck and page-boundary stops. It drains after the open
// and again right before page 0's bytes, even when Precheck sent no request. JobResult.Stopped and
// PagesWritten are set; open errors are returned in Err unchanged (ErrDeviceBusy, ErrNoPrinter).
func (pr *Printer) PrintObserved(ctx context.Context, j Job, o Observer) JobResult {
	if pr == nil {
		return JobResult{State: JobFailed, Err: fmt.Errorf("print: nil printer")}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	res := JobResult{State: JobFailed, PageCount: len(j.Pages)}
	if o == nil {
		res.Err = fmt.Errorf("print: nil observer")
		return res
	}
	enc := &RasterEncoder{}
	pages, err := enc.encodePages(pr.profile, j)
	res.Warnings = append(res.Warnings, enc.Warnings()...)
	if err != nil {
		res.Err = err
		return res
	}
	if err := pr.acquire(ctx); err != nil {
		res.State = JobCancelled
		res.Err = err
		return res
	}
	defer pr.release()
	if pr.open == nil {
		res.Err = fmt.Errorf("print: nil open")
		return res
	}
	dev, err := pr.open()
	if err != nil {
		res.Err = err
		return res
	}
	defer func() {
		cerr := dev.Close()
		if cerr != nil && res.Err == nil {
			res.Err = fmt.Errorf("close: %w", cerr)
			res.State = JobFailed
		}
	}()

	drain(dev)
	if err := o.Precheck(func() (Status, error) {
		return requestStatusTimeouts(dev, pr.timeouts)
	}); err != nil {
		res.Err = err
		return res
	}
	drain(dev)

	for i, page := range pages {
		if i > 0 && o.StopBefore(i) {
			res.Stopped = true
			res.State = JobCancelled
			return pr.invalidateWhenIdle(ctx, dev, res, true)
		}
		if err := ctx.Err(); err != nil {
			res.Stopped = true
			res.State = JobCancelled
			res.Err = err
			return pr.invalidateWhenIdle(ctx, dev, res, i > 0)
		}
		o.Sending(i)
		res.State = JobSending
		n, werr := dev.Write(page)
		if n > 0 {
			res.LastByteAt = time.Now()
			res.InFlightBytes = true
		}
		if werr != nil {
			res.State = JobFailed
			res.Err = deviceErr(werr)
			return res
		}
		res.PagesWritten = i + 1
		o.Printing(i)
		res.State = JobPrinting
		confirmed, idle, statuses, werr := pr.waitPageObserved(ctx, dev, j, i, o)
		res.Statuses = append(res.Statuses, statuses...)
		if confirmed {
			res.PagesConfirmed = i + 1
			res.InFlightBytes = false
			o.PageDone(i)
		}
		if werr != nil {
			return pr.finishWait(ctx, dev, res, werr, idle, confirmed)
		}
	}
	res.State = JobDone
	pr.mu.Lock()
	pr.uncut = !j.Options.CutAtEnd
	pr.mu.Unlock()
	return res
}

func (pr *Printer) finishWait(ctx context.Context, dev Device, res JobResult, werr error, idle, confirmed bool) JobResult {
	if errors.Is(werr, ErrCooling) {
		res.Cooling = true
		res.Err = ErrCooling
		res.State = JobUnknown
		res.InFlightBytes = true
		return res
	}
	if errors.Is(werr, context.Canceled) || errors.Is(werr, context.DeadlineExceeded) {
		res.Stopped = true
		res.State = JobCancelled
		if res.Err == nil {
			res.Err = werr
		}
		return pr.invalidateWhenIdle(ctx, dev, res, idle || confirmed)
	}
	if errors.Is(werr, ErrTimeout) {
		res.State = JobUnknown
		res.Err = werr
		res.InFlightBytes = true
		return res
	}
	res.State = JobFailed
	res.Err = werr
	if res.PagesWritten > res.PagesConfirmed {
		res.InFlightBytes = true
	}
	return res
}

func (pr *Printer) invalidateWhenIdle(ctx context.Context, dev Device, res JobResult, alreadyIdle bool) JobResult {
	if !alreadyIdle {
		idle, statuses, err := pr.readUntilIdle(ctx, dev)
		res.Statuses = append(res.Statuses, statuses...)
		if !idle {
			if res.Err == nil {
				res.Err = err
			}
			return res
		}
	}
	if _, err := dev.Write(invalidate(pr.profile)); err != nil {
		res.Err = fmt.Errorf("%w: %w", ErrPartial, deviceErr(err))
		res.State = JobFailed
	}
	return res
}

func (pr *Printer) readUntilIdle(ctx context.Context, dev Device) (bool, []Status, error) {
	deadline := time.Now().Add(pr.timeouts.PhasePrinting)
	var got []Status
	for {
		rem := time.Until(deadline)
		if rem <= 0 {
			return false, got, ErrTimeout
		}
		if err := ctx.Err(); err != nil && len(got) > 0 {
			return false, got, err
		}
		st, err := dev.ReadStatus(rem)
		if err != nil {
			if errors.Is(err, ErrTimeout) {
				return false, got, ErrTimeout
			}
			return false, got, deviceErr(err)
		}
		got = append(got, st)
		if waitingToReceive(st) {
			return true, got, nil
		}
	}
}

func waitingToReceive(st Status) bool {
	return !st.Printing && (st.Type == StatusPhase || st.Type == StatusReply || st.Type == StatusCompleted)
}

func (pr *Printer) waitPageObserved(ctx context.Context, dev Device, j Job, index int, o Observer) (confirmed, idle bool, got []Status, err error) {
	height := 0
	if index < len(j.Pages) && j.Pages[index] != nil {
		height = j.Pages[index].H
	}
	dpi := 300
	if pr.profile != nil && pr.profile.DPI > 0 {
		dpi = pr.profile.DPI
	}
	mm := LengthMM(dpi, height, j.Options.HighRes)
	limit := pr.timeouts.CompletedBase + time.Duration(mm)*pr.timeouts.CompletedPerMM
	if pr.timeouts.PhasePrinting > limit {
		limit = pr.timeouts.PhasePrinting
	}
	deadline := time.Now().Add(limit)
	cooling := false
	for {
		if ctx.Err() != nil && idle {
			return confirmed, true, got, ctx.Err()
		}
		rem := time.Until(deadline)
		if rem <= 0 {
			if ctx.Err() != nil {
				return confirmed, idle, got, ctx.Err()
			}
			if cooling {
				return confirmed, idle, got, ErrCooling
			}
			return confirmed, idle, got, fmt.Errorf("page %d: %w", index, ErrTimeout)
		}
		slice := rem
		if ctx.Done() != nil && slice > 200*time.Millisecond {
			slice = 200 * time.Millisecond
		}
		st, rerr := dev.ReadStatus(slice)
		if rerr != nil {
			if errors.Is(rerr, ErrTimeout) {
				continue
			}
			return confirmed, idle, got, deviceErr(rerr)
		}
		got = append(got, st)
		o.Status(st)
		if st.Type == StatusError || len(st.Errors) > 0 {
			return confirmed, false, got, statusError(st)
		}
		if st.Type == StatusNotify && st.Notification == NotifyCoolingStart {
			cooling = true
			ext := time.Now().Add(pr.timeouts.Cooling)
			if ext.After(deadline) {
				deadline = ext
			}
			continue
		}
		if st.Type == StatusNotify && st.Notification == NotifyCoolingFinish {
			cooling = false
			floor := time.Now().Add(pr.timeouts.CompletedBase)
			if deadline.Before(floor) {
				deadline = floor
			}
			continue
		}
		if st.Type == StatusCompleted {
			confirmed = true
		}
		waiting := waitingToReceive(st)
		if waiting {
			idle = true
		}
		if confirmed && waiting {
			if ctx.Err() != nil {
				return true, true, got, ctx.Err()
			}
			return true, true, got, nil
		}
		if ctx.Err() != nil && waiting {
			return confirmed, true, got, ctx.Err()
		}
	}
}

func (pr *Printer) acquire(ctx context.Context) error {
	pr.mu.Lock()
	stop := context.AfterFunc(ctx, func() { pr.cond.Broadcast() })
	defer stop()
	for pr.worker || pr.busy || len(pr.queue) > 0 {
		if err := ctx.Err(); err != nil {
			pr.mu.Unlock()
			return err
		}
		pr.cond.Wait()
	}
	pr.busy = true
	pr.mu.Unlock()
	return nil
}

func (pr *Printer) release() {
	pr.mu.Lock()
	pr.busy = false
	pr.cond.Broadcast()
	pr.mu.Unlock()
}
