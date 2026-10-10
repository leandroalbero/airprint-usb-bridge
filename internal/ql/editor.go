package ql

import (
	"os"
	"path/filepath"
	"strings"
)

// DetectEditorLite reports a 04f9:2049 device under sysRoot/bus/usb/devices.
// An unreadable tree is not Editor Lite; the caller keeps the offline state.
func DetectEditorLite(sysRoot string) bool {
	if sysRoot == "" {
		return false
	}
	dir := filepath.Join(sysRoot, "bus", "usb", "devices")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		base := filepath.Join(dir, e.Name())
		vendor, errV := os.ReadFile(filepath.Join(base, "idVendor"))
		product, errP := os.ReadFile(filepath.Join(base, "idProduct"))
		if errV != nil || errP != nil {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(string(vendor)), "04f9") &&
			strings.EqualFold(strings.TrimSpace(string(product)), "2049") {
			return true
		}
	}
	return false
}
