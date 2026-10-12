package main

import (
	"bufio"
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/leandroalbero/airprint-usb-bridge/internal/driver"
	"github.com/leandroalbero/airprint-usb-bridge/internal/ql"
)

type options struct {
	configFile     string
	name           string
	location       string
	port           int
	device         string
	match          string
	driver         string
	driverOpts     stringList
	ppd            string
	profile        string
	media          string
	mediaPolicy    string
	presets        string
	trim           string
	dither         string
	color          string
	highRes        bool
	stateDir       string
	mdns           string
	dryRun         string
	debugDir       string
	statusInterval time.Duration
	retryDeadline  time.Duration
	verbose        bool
}

// stringList is a repeatable flag.
type stringList []string

func (l *stringList) String() string     { return strings.Join(*l, ",") }
func (l *stringList) Set(v string) error { *l = append(*l, v); return nil }

func newFlagSet(o *options, out io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(progName, flag.ContinueOnError)
	fs.SetOutput(out)
	fs.StringVar(&o.configFile, "config", "", "key=value config `file`; flags given on the command line win")
	fs.StringVar(&o.name, "name", "", "printer name shown to clients (default: the make and model)")
	fs.StringVar(&o.location, "location", "", "printer location")
	fs.IntVar(&o.port, "port", 8631, "TCP port for IPP and IPPS")
	fs.StringVar(&o.device, "device", "auto", "printer: auto (first USB printer), /dev/usb/lpN or tcp://host:9100")
	fs.StringVar(&o.match, "match", "", "with --device auto, use the first USB printer whose `MFG:MDL` contain these substrings (see list-printers)")
	fs.StringVar(&o.driver, "driver", "auto", "printer driver: auto (from the USB device ID) or "+strings.Join(driver.Names(), ", "))
	fs.Var(&o.driverOpts, "driver-opt", "driver setting `key=value`; repeatable")
	fs.StringVar(&o.ppd, "ppd", "", "PPD `file` for the cups driver")
	fs.StringVar(&o.profile, "profile", "", "brother-ql: built-in profile ("+strings.Join(ql.EmbeddedProfileNames(), ", ")+") or a profile JSON `path` (default: from the printer model, else ql700)")
	fs.StringVar(&o.media, "media", "", "media `id`; each driver lists its own (see the drivers command); brother-ql default 62")
	fs.StringVar(&o.mediaPolicy, "media-policy", "roll", "brother-ql: loaded media advertised to clients, roll or roll+presets")
	fs.StringVar(&o.presets, "presets", "29,50", "brother-ql: preset label lengths in mm, comma separated")
	fs.StringVar(&o.trim, "trim", "max", "brother-ql: blank rows trimmed per label end, max (no limit) or N")
	fs.StringVar(&o.dither, "dither", "auto", "dithering: auto, on or off")
	fs.StringVar(&o.color, "color", "auto", "colour printing: auto (cups: when the PPD has colour; postscript: when --ppd says *ColorDevice: True), on or off; label drivers print black only")
	fs.BoolVar(&o.highRes, "high-res", false, "offer print-quality high where the driver can (brother-ql: 300x600 dpi); for print, print in high quality")
	fs.StringVar(&o.stateDir, "state-dir", "", "directory for the UUID, TLS key pair and spool (default $STATE_DIRECTORY, /var/lib/"+progName+" as root, else ~/.local/state/"+progName+")")
	fs.StringVar(&o.mdns, "mdns", "auto", "DNS-SD advertising: auto, avahi, builtin or off")
	fs.StringVar(&o.dryRun, "dry-run", "", "write job-<n>.bin (and .txt where the driver explains its bytes) to `DIR` instead of printing")
	fs.StringVar(&o.debugDir, "debug-dir", "", "keep each job's input and rendered images in `DIR`")
	fs.DurationVar(&o.statusInterval, "status-interval", 30*time.Second, "how often to query the printer status while idle; 0 never queries it")
	fs.DurationVar(&o.retryDeadline, "retry-deadline", 120*time.Second, "how long to retry a job while the printer is busy or absent")
	fs.BoolVar(&o.verbose, "v", false, "log every IPP request")
	return fs
}

// parseDriverOpts turns --driver-opt key=value pairs into a map.
func parseDriverOpts(list []string) (map[string]string, error) {
	out := map[string]string{}
	for _, kv := range list {
		k, v, ok := strings.Cut(kv, "=")
		k = strings.TrimSpace(k)
		if !ok || k == "" {
			return nil, fmt.Errorf("--driver-opt %q: want key=value", kv)
		}
		out[k] = strings.TrimSpace(v)
	}
	return out, nil
}

// parseArgs parses flags anywhere on the command line, so both
// "airprint-usb-bridge --dry-run d print f" and "airprint-usb-bridge print f --dry-run d" work.
func parseArgs(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if n := len(args) - len(rest); n > 0 && args[n-1] == "--" {
			return append(pos, rest...), nil
		}
		if len(rest) == 0 {
			return pos, nil
		}
		pos = append(pos, rest[0])
		args = rest[1:]
	}
}

// applyConfigFile sets every flag named in the file that was not given on the command line.
func applyConfigFile(fs *flag.FlagSet, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	explicit := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { explicit[f.Name] = true })
	sc := bufio.NewScanner(bytes.NewReader(data))
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			return fmt.Errorf("%s:%d: want key = value", path, n)
		}
		key = strings.TrimSpace(key)
		val = unquote(strings.TrimSpace(val))
		if key == "config" || fs.Lookup(key) == nil {
			return fmt.Errorf("%s:%d: unknown key %q", path, n, key)
		}
		if explicit[key] {
			continue
		}
		if err := fs.Set(key, val); err != nil {
			return fmt.Errorf("%s:%d: %s: %v", path, n, key, err)
		}
	}
	return sc.Err()
}

func unquote(s string) string {
	if len(s) >= 2 && (s[0] == '"' && s[len(s)-1] == '"' || s[0] == '\'' && s[len(s)-1] == '\'') {
		return s[1 : len(s)-1]
	}
	return s
}

func parseDither(s string) (string, error) {
	switch s {
	case "auto", "on", "off":
		return s, nil
	}
	return "", fmt.Errorf("--dither %q: want auto, on or off", s)
}

func parseColor(s string) (string, error) {
	switch s {
	case "auto", "on", "off":
		return s, nil
	}
	return "", fmt.Errorf("--color %q: want auto, on or off", s)
}

func resolveStateDir(flagVal string) (string, error) {
	if flagVal != "" {
		return flagVal, nil
	}
	// systemd's StateDirectory= may list several directories, separated by colons.
	if sd := os.Getenv("STATE_DIRECTORY"); sd != "" {
		first, _, _ := strings.Cut(sd, ":")
		return first, nil
	}
	if os.Geteuid() == 0 {
		return "/var/lib/" + progName, nil
	}
	if x := os.Getenv("XDG_STATE_HOME"); x != "" {
		return filepath.Join(x, progName), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", errors.New("no --state-dir and no home directory")
	}
	return filepath.Join(home, ".local", "state", progName), nil
}
