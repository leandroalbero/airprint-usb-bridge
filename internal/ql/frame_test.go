package ql

import (
	"bytes"
	"testing"
)

func statusRaw(n byte) []byte {
	b := make([]byte, 32)
	b[0], b[1], b[2] = 0x80, 0x20, 0x42
	b[31] = n
	return b
}

func TestFrameBuf_splitMergeAndResync(t *testing.T) {
	var f frameBuf
	a := statusRaw(1)
	f.push(a[:10])
	if _, ok := f.next(); ok {
		t.Fatal("partial")
	}
	f.push(a[10:])
	got, ok := f.next()
	if !ok || !bytes.Equal(got, a) {
		t.Fatalf("%v %x", ok, got)
	}

	var merged frameBuf
	merged.push(append(append([]byte{}, statusRaw(2)...), statusRaw(3)...))
	first, ok := merged.next()
	second, ok2 := merged.next()
	if !ok || !ok2 || first[31] != 2 || second[31] != 3 {
		t.Fatalf("%x %x", first, second)
	}

	var dirty frameBuf
	junk := []byte{0x11, 0x22, 0x80, 0x20}
	dirty.push(append(junk, statusRaw(9)...))
	got, ok = dirty.next()
	if !ok || got[31] != 9 {
		t.Fatalf("resync %v %x", ok, got)
	}
}
