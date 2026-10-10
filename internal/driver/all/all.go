// Package all links every driver into the binary; each registers itself from init.
package all

import (
	_ "github.com/leandroalbero/airprint-usb-bridge/internal/driver/brotherql"
	_ "github.com/leandroalbero/airprint-usb-bridge/internal/driver/cups"
	_ "github.com/leandroalbero/airprint-usb-bridge/internal/driver/dymo"
	_ "github.com/leandroalbero/airprint-usb-bridge/internal/driver/pcl"
	_ "github.com/leandroalbero/airprint-usb-bridge/internal/driver/postscript"
	_ "github.com/leandroalbero/airprint-usb-bridge/internal/driver/zpl"
)
