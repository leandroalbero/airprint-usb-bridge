package ql

// Observer follows one job (§3.5). Calls come from the printing goroutine, in order.
type Observer interface {
	// Precheck runs after open and the post-open drain, before the first byte. request sends ESC i S
	// (drain, FirstStatusReply then StatusReply). A non-nil error aborts the job with no byte sent;
	// printq calls store.StartJob on its success path (§3.5 step 3).
	Precheck(request func() (Status, error)) error
	Sending(page int)
	Printing(page int)
	Status(st Status)
	PageDone(page int)
	// StopBefore is asked before every page after the first; true ends the job at this boundary.
	StopBefore(page int) bool
}

// OpenAuto scans lp0..lp3 and /dev/usb/lp*, probes each candidate (first attempt FirstStatusReply,
// second StatusReply) and returns the open device, its path and the probe's status, which the caller
// uses as that operation's status (§3.1). ErrDeviceBusy is preferred over ErrNoPrinter, as today.
func OpenAuto(p *Profile, t Timeouts) (dev Device, path string, st Status, err error) {
	return openAuto(p, t)
}
