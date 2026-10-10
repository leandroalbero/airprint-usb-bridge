package ql

import (
	"context"
	"errors"
	"fmt"
	"time"
)

type queuedJob struct {
	ctx  context.Context
	job  Job
	done chan JobResult
}

func (pr *Printer) print(ctx context.Context, j Job) JobResult {
	if ctx == nil {
		ctx = context.Background()
	}
	req := queuedJob{ctx: ctx, job: j, done: make(chan JobResult, 1)}
	pr.mu.Lock()
	pr.queue = append(pr.queue, req)
	start := !pr.worker
	pr.worker = true
	pr.cond.Broadcast()
	pr.mu.Unlock()
	if start {
		go pr.loop()
	}
	return <-req.done
}

func (pr *Printer) loop() {
	for {
		pr.mu.Lock()
		for pr.busy && len(pr.queue) > 0 {
			pr.cond.Wait()
		}
		if len(pr.queue) == 0 {
			pr.worker = false
			pr.cond.Broadcast()
			pr.mu.Unlock()
			return
		}
		req := pr.queue[0]
		pr.queue = pr.queue[1:]
		pr.busy = true
		pr.mu.Unlock()

		req.done <- pr.run(req.ctx, req.job)

		pr.mu.Lock()
		pr.busy = false
		pr.cond.Broadcast()
		pr.mu.Unlock()
	}
}

func (pr *Printer) exclusive(ctx context.Context, fn func(Device) error) error {
	if ctx == nil {
		ctx = context.Background()
	}
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

	defer func() {
		pr.mu.Lock()
		pr.busy = false
		pr.cond.Broadcast()
		pr.mu.Unlock()
	}()
	if pr.open == nil {
		return fmt.Errorf("printer: nil open")
	}
	dev, err := pr.open()
	if err != nil {
		return err
	}
	err = fn(dev)
	cerr := dev.Close()
	if err != nil {
		return err
	}
	if cerr != nil {
		return fmt.Errorf("close: %w", cerr)
	}
	return nil
}

func (pr *Printer) status(ctx context.Context) (Status, error) {
	var st Status
	err := pr.exclusive(ctx, func(dev Device) error {
		var rerr error
		st, rerr = requestStatusTimeouts(dev, pr.timeouts)
		return rerr
	})
	return st, err
}

func (pr *Printer) sendRaw(ctx context.Context, b []byte, capture time.Duration) ([]Status, error) {
	var out []Status
	err := pr.exclusive(ctx, func(dev Device) error {
		if len(b) > 0 {
			if _, werr := dev.Write(b); werr != nil {
				return werr
			}
		}
		if capture <= 0 {
			return nil
		}
		deadline := time.Now().Add(capture)
		for {
			rem := time.Until(deadline)
			if rem <= 0 {
				return nil
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			st, rerr := dev.ReadStatus(rem)
			if errors.Is(rerr, ErrTimeout) {
				return nil
			}
			if rerr != nil {
				return rerr
			}
			out = append(out, st)
		}
	})
	return out, err
}
