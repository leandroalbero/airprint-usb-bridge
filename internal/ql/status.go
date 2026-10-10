package ql

import (
	"errors"
	"fmt"
	"time"
)

const (
	NotifyCoolingStart  byte = 0x03
	NotifyCoolingFinish byte = 0x04
	PhaseWaiting        byte = 0x00
	PhasePrinting       byte = 0x01
)

type bitName struct {
	mask byte
	name string
}

var err1Names = []bitName{
	{0x01, "no media"},
	{0x02, "end of media"},
	{0x04, "cutter jam"},
	{0x10, "main unit in use"},
	{0x80, "fan"},
}

var err2Names = []bitName{
	{0x01, "media mismatch"},
	{0x04, "transmission"},
	{0x10, "cover open"},
	{0x40, "cannot feed"},
	{0x80, "system error"},
}

func (t StatusType) String() string {
	switch t {
	case StatusReply:
		return "reply"
	case StatusCompleted:
		return "completed"
	case StatusError:
		return "error"
	case StatusNotify:
		return "notify"
	case StatusPhase:
		return "phase"
	case StatusSettings:
		return "settings report"
	default:
		return fmt.Sprintf("0x%02x", byte(t))
	}
}

func parseStatus(p *Profile, raw []byte) (Status, error) {
	if len(raw) != 32 {
		return Status{}, fmt.Errorf("status: %w: got %d", ErrStatusLength, len(raw))
	}
	if raw[0] != 0x80 || raw[1] != 0x20 || raw[2] != 0x42 {
		return Status{}, fmt.Errorf("status: %w: %02x %02x %02x", ErrFraming, raw[0], raw[1], raw[2])
	}
	var s Status
	copy(s.Raw[:], raw)
	s.ModelByte3 = raw[3]
	s.ModelByte4 = raw[4]
	s.Err1 = raw[8]
	s.Err2 = raw[9]
	s.MediaWidthMM = int(raw[10])
	s.MediaKind = MediaKind(raw[11])
	s.MediaLengthMM = int(raw[17])
	s.Type = StatusType(raw[18])
	s.Printing = raw[19] == PhasePrinting
	s.PhaseNumber = uint16(raw[20]) | uint16(raw[21])<<8
	s.Notification = raw[22]
	s.At = time.Now()
	s.Errors, s.Unexpected = classifyErrors(p, raw[8], raw[9])
	return s, nil
}

func classifyErrors(p *Profile, err1, err2 byte) (named, unexpected []string) {
	named, unexpected = classifyByte(p, "err1", err1, err1Names)
	n2, u2 := classifyByte(p, "err2", err2, err2Names)
	return append(named, n2...), append(unexpected, u2...)
}

func classifyByte(p *Profile, field string, value byte, table []bitName) (named, unexpected []string) {
	valid := validMasks(p, field)
	for mask := byte(1); mask != 0; mask <<= 1 {
		if value&mask == 0 {
			continue
		}
		name, known := bitLabel(table, mask)
		if valid != nil && !valid[mask] {
			unexpected = append(unexpected, unexpectedBit(field, mask, name, known))
			continue
		}
		if !known {
			unexpected = append(unexpected, unexpectedBit(field, mask, "", false))
			continue
		}
		named = append(named, name)
	}
	return named, unexpected
}

func validMasks(p *Profile, field string) map[byte]bool {
	if p == nil || p.ValidErrorBits == nil {
		return nil
	}
	raw, ok := p.ValidErrorBits[field]
	if !ok {
		return map[byte]bool{}
	}
	out := make(map[byte]bool, len(raw))
	for _, b := range raw {
		out[b] = true
	}
	return out
}

func bitLabel(table []bitName, mask byte) (string, bool) {
	for _, b := range table {
		if b.mask == mask {
			return b.name, true
		}
	}
	return "", false
}

func unexpectedBit(field string, mask byte, name string, known bool) string {
	if known {
		return fmt.Sprintf("unexpected bit %s:0x%02x (%s)", field, mask, name)
	}
	return fmt.Sprintf("unexpected bit %s:0x%02x", field, mask)
}

func statusError(s Status) error {
	if s.Type != StatusError && len(s.Errors) == 0 {
		return nil
	}
	var errs []error
	for _, name := range s.Errors {
		errs = append(errs, namedError(name))
	}
	for _, u := range s.Unexpected {
		errs = append(errs, fmt.Errorf("%s", u))
	}
	if len(errs) == 0 {
		return ErrPrinter
	}
	return errors.Join(errs...)
}
