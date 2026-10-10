// Command airprint-usb-bridge makes a USB (or raw TCP) printer an AirPrint / IPP Everywhere
// printer, with a pluggable driver that turns raster pages into printer bytes.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"

	"github.com/leandroalbero/airprint-usb-bridge/internal/backend"
	_ "github.com/leandroalbero/airprint-usb-bridge/internal/driver/all"
)

// Set with -ldflags "-X main.buildVersion=v1.2.3".
var buildVersion = ""

func version() string {
	if buildVersion != "" {
		return buildVersion
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return "dev"
}

const progName = "airprint-usb-bridge"

const usageText = `Usage: airprint-usb-bridge [flags] [command]

Commands:
  serve          run the IPP/AirPrint server (default)
  print FILE     print a URF, PWG raster or PNG file directly, without IPP
  status         query the printer once and print its state
  list-printers  list USB printers, their IEEE 1284 device IDs and suggested drivers
  drivers        list the drivers, their media, options and verification status
  version        print the version

Flags may appear before or after the command.

`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	var o options
	fs := newFlagSet(&o, stderr)
	fs.Usage = func() {
		fmt.Fprint(stderr, usageText)
		fs.PrintDefaults()
	}
	pos, err := parseArgs(fs, args)
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err != nil {
		return 2
	}
	if o.configFile != "" {
		if err := applyConfigFile(fs, o.configFile); err != nil {
			fmt.Fprintf(stderr, progName+": config: %v\n", err)
			return 2
		}
	}
	cmd := "serve"
	if len(pos) > 0 {
		cmd, pos = pos[0], pos[1:]
	}
	want := map[string]int{"serve": 0, "print": 1, "status": 0, "version": 0, "list-printers": 0, "drivers": 0}
	n, ok := want[cmd]
	if !ok {
		fmt.Fprintf(stderr, progName+": unknown command %q\n", cmd)
		fs.Usage()
		return 2
	}
	if len(pos) != n {
		fmt.Fprintf(stderr, progName+": %s takes %d argument(s), got %d\n", cmd, n, len(pos))
		return 2
	}

	flags := log.LstdFlags
	if os.Getenv("JOURNAL_STREAM") != "" {
		flags = 0
	}
	logger := log.New(stderr, "", flags)
	logf := logFunc(logger.Printf)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch cmd {
	case "version":
		fmt.Fprintf(stdout, progName+" %s\n", version())
		return 0
	case "drivers":
		err = runDrivers(stdout)
	case "list-printers":
		err = runListPrinters(&o, stdout, backend.DiscoverSystem)
	case "serve":
		err = runServe(ctx, &o, logf)
	case "print":
		err = runPrint(ctx, &o, pos[0], stdout, logf)
	case "status":
		err = runStatus(ctx, &o, stdout, logf)
	}
	if err != nil {
		fmt.Fprintf(stderr, progName+": %v\n", err)
		return 1
	}
	return 0
}
