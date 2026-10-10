// Command ql-airprint makes a Brother QL label printer an AirPrint / IPP Everywhere
// printer for continuous-roll labels.
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

const usageText = `Usage: ql-airprint [flags] [command]

Commands:
  serve        run the IPP/AirPrint server (default)
  print FILE   print a URF, PWG raster or PNG file directly, without IPP
  status       query the printer once and print its state
  version      print the version

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
			fmt.Fprintf(stderr, "ql-airprint: config: %v\n", err)
			return 2
		}
	}
	cmd := "serve"
	if len(pos) > 0 {
		cmd, pos = pos[0], pos[1:]
	}
	want := map[string]int{"serve": 0, "print": 1, "status": 0, "version": 0}
	n, ok := want[cmd]
	if !ok {
		fmt.Fprintf(stderr, "ql-airprint: unknown command %q\n", cmd)
		fs.Usage()
		return 2
	}
	if len(pos) != n {
		fmt.Fprintf(stderr, "ql-airprint: %s takes %d argument(s), got %d\n", cmd, n, len(pos))
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
		fmt.Fprintf(stdout, "ql-airprint %s\n", version())
		return 0
	case "serve":
		err = runServe(ctx, &o, logf)
	case "print":
		err = runPrint(ctx, &o, pos[0], stdout, logf)
	case "status":
		err = runStatus(ctx, &o, stdout, logf)
	}
	if err != nil {
		fmt.Fprintf(stderr, "ql-airprint: %v\n", err)
		return 1
	}
	return 0
}
