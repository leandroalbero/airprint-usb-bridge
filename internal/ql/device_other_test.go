//go:build !linux

package ql

import (
	"errors"
	"strings"
	"testing"
)

func TestOpenDevice_unsupportedOnThisOS(t *testing.T) {
	_, err := OpenDevice(&Profile{Model: "QL-700"}, "auto")
	if !errors.Is(err, ErrUnsupportedOS) {
		t.Fatal(err)
	}
	if !strings.Contains(err.Error(), "unsupported on this OS") {
		t.Fatal(err)
	}
}
