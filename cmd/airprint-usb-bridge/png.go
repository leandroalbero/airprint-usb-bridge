package main

import (
	"bytes"
	"image"
	"image/png"
	"os"
)

var pngMagic = []byte("\x89PNG\r\n\x1a\n")

func isPNG(b []byte) bool { return bytes.HasPrefix(b, pngMagic) }

// writePNG stores a debug image with owner-only permissions.
func writePNG(path string, img image.Image) error {
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return err
	}
	return os.WriteFile(path, buf.Bytes(), 0o600)
}
