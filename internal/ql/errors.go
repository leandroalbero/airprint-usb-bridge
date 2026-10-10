package ql

import "errors"

var (
	ErrNoMedia       = errors.New("no media")
	ErrEndOfMedia    = errors.New("end of media")
	ErrCutterJam     = errors.New("cutter jam")
	ErrMainUnitInUse = errors.New("main unit in use")
	ErrFan           = errors.New("fan")
	ErrMediaMismatch = errors.New("media mismatch")
	ErrTransmission  = errors.New("transmission")
	ErrCoverOpen     = errors.New("cover open")
	ErrCannotFeed    = errors.New("cannot feed")
	ErrSystem        = errors.New("system error")
	ErrPrinter       = errors.New("printer error")

	ErrFraming       = errors.New("framing error")
	ErrStatusLength  = errors.New("status length")
	ErrTimeout       = errors.New("timeout")
	ErrDeviceBusy    = errors.New("device busy")
	ErrDeviceGone    = errors.New("device disappeared")
	ErrNoPrinter     = errors.New("no printer connected")
	ErrUnsupportedOS = errors.New("unsupported on this OS")
	ErrBitmapWidth   = errors.New("bitmap wider than print pins")
	ErrCooling       = errors.New("cooling")
	ErrPartial       = errors.New("may have printed partially")
)

func namedError(name string) error {
	switch name {
	case ErrNoMedia.Error():
		return ErrNoMedia
	case ErrEndOfMedia.Error():
		return ErrEndOfMedia
	case ErrCutterJam.Error():
		return ErrCutterJam
	case ErrMainUnitInUse.Error():
		return ErrMainUnitInUse
	case ErrFan.Error():
		return ErrFan
	case ErrMediaMismatch.Error():
		return ErrMediaMismatch
	case ErrTransmission.Error():
		return ErrTransmission
	case ErrCoverOpen.Error():
		return ErrCoverOpen
	case ErrCannotFeed.Error():
		return ErrCannotFeed
	case ErrSystem.Error():
		return ErrSystem
	default:
		return errors.New(name)
	}
}
