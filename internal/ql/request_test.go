package ql

import (
	"errors"
	"testing"
	"time"
)

func TestRequestStatus_retriesOnceAfterSilentFirstRequest(t *testing.T) {
	dev := &fakeDevice{reads: []readStep{{err: ErrTimeout}, {st: Status{Type: StatusReply, ModelByte3: 0x34}}}}
	st, err := requestStatus(dev, 10*time.Millisecond)
	if err != nil || st.ModelByte3 != 0x34 {
		t.Fatalf("st=%+v err=%v", st, err)
	}
	if len(dev.writes) != 2 {
		t.Fatalf("writes %d, want 2 status requests", len(dev.writes))
	}
}

func TestRequestStatus_givesUpAfterTwoTimeouts(t *testing.T) {
	dev := &fakeDevice{reads: []readStep{{err: ErrTimeout}, {err: ErrTimeout}, {st: Status{Type: StatusReply}}}}
	if _, err := requestStatus(dev, 10*time.Millisecond); !errors.Is(err, ErrTimeout) {
		t.Fatalf("err=%v, want timeout", err)
	}
}

func TestReadSetting_skipsLateStatusReply(t *testing.T) {
	var report Status
	report.Type = StatusSettings
	report.Raw[30] = 0x06
	dev := &fakeDevice{reads: []readStep{{st: Status{Type: StatusReply}}, {st: report}}}
	st, err := readSetting(dev, 0x41, time.Second)
	if err != nil || st.Raw[30] != 0x06 {
		t.Fatalf("st=%+v err=%v", st, err)
	}
}
