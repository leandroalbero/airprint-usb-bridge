package ql

// frameBuf splits a byte stream into 32-byte status frames and resyncs on a bad header.
type frameBuf struct {
	b []byte
}

func (f *frameBuf) push(p []byte) {
	f.b = append(f.b, p...)
}

func (f *frameBuf) next() ([]byte, bool) {
	f.align()
	if len(f.b) < 32 {
		return nil, false
	}
	if f.b[0] != 0x80 || f.b[1] != 0x20 || f.b[2] != 0x42 {
		return nil, false
	}
	raw := append([]byte(nil), f.b[:32]...)
	f.b = append([]byte(nil), f.b[32:]...)
	return raw, true
}

func (f *frameBuf) align() {
	if len(f.b) < 3 {
		return
	}
	if f.b[0] == 0x80 && f.b[1] == 0x20 && f.b[2] == 0x42 {
		return
	}
	for i := 1; i+2 < len(f.b); i++ {
		if f.b[i] == 0x80 && f.b[i+1] == 0x20 && f.b[i+2] == 0x42 {
			f.b = append([]byte(nil), f.b[i:]...)
			return
		}
	}
	keep := 0
	n := len(f.b)
	if n >= 2 && f.b[n-2] == 0x80 && f.b[n-1] == 0x20 {
		keep = 2
	} else if n >= 1 && f.b[n-1] == 0x80 {
		keep = 1
	}
	if keep == 0 {
		f.b = nil
		return
	}
	f.b = append([]byte(nil), f.b[n-keep:]...)
}
