package ql

import (
	"context"
	"errors"
	"fmt"
	"time"
)

func (pr *Printer) run(ctx context.Context, j Job) (res JobResult) {
	if ctx == nil {
		ctx = context.Background()
	}
	res = JobResult{State: JobFailed, PageCount: len(j.Pages)}
	if err := ctx.Err(); err != nil {
		res.State = JobCancelled
		res.Err = err
		return res
	}
	enc := &RasterEncoder{}
	pages, err := enc.encodePages(pr.profile, j)
	res.Warnings = append(res.Warnings, enc.Warnings()...)
	if err != nil {
		res.Err = err
		return res
	}
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

	for i, page := range pages {
		if err := ctx.Err(); err != nil {
			res = pr.cancel(dev, res, i > 0, true)
			return res
		}
		res.State = JobSending
		start := time.Now()
		if _, werr := dev.Write(page); werr != nil {
			res.State = JobFailed
			res.Err = deviceErr(werr)
			return res
		}
		if pr.stall > 0 && time.Since(start) > pr.stall {
			res.Warnings = append(res.Warnings, fmt.Sprintf("write stall mid-page %d", i))
		}
		res.State = JobPrinting
		confirmed, idle, statuses, werr := pr.waitPage(ctx, dev, j, i)
		res.Statuses = append(res.Statuses, statuses...)
		if confirmed {
			res.PagesConfirmed = i + 1
		}
		if werr != nil {
			if errors.Is(werr, ErrCooling) {
				res.State = JobPrinting
				res.Cooling = true
				res.Err = ErrCooling
				return res
			}
			if errors.Is(werr, context.Canceled) || errors.Is(werr, context.DeadlineExceeded) {
				res = pr.cancel(dev, res, true, idle)
				return res
			}
			res.State = JobFailed
			res.Err = werr
			return res
		}
	}
	res.State = JobDone
	pr.mu.Lock()
	pr.uncut = !j.Options.CutAtEnd
	pr.mu.Unlock()
	return res
}

func (pr *Printer) cancel(dev Device, res JobResult, sent, idle bool) JobResult {
	res.State = JobCancelled
	if sent {
		res.Err = ErrPartial
	} else {
		res.Err = context.Canceled
	}
	if !idle || dev == nil {
		return res
	}
	if _, err := dev.Write(invalidate(pr.profile)); err != nil {
		res.Err = fmt.Errorf("%w: %w", ErrPartial, deviceErr(err))
	}
	return res
}

func (pr *Printer) waitPage(ctx context.Context, dev Device, j Job, index int) (confirmed, idle bool, got []Status, err error) {
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
		waiting := !st.Printing && (st.Type == StatusPhase || st.Type == StatusReply || st.Type == StatusCompleted)
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

func deviceErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrDeviceGone) || errors.Is(err, ErrTimeout) || errors.Is(err, ErrDeviceBusy) || errors.Is(err, ErrFraming) {
		return err
	}
	return fmt.Errorf("device disappeared: %w", ErrDeviceGone)
}
