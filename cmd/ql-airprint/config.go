package main

import (
	"bufio"
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type options struct {
	configFile     string
	name           string
	location       string
	port           int
	device         string
	profile        string
	media          string
	mediaPolicy    string
	presets        string
	trim           string
	dither         string
	highRes        bool
	stateDir       string
	mdns           string
	dryRun         string
	debugDir       string
	statusInterval time.Duration
	retryDeadline  time.Duration
	verbose        bool
}

func newFlagSet(o *options, out io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet("ql-airprint", flag.ContinueOnError)
	fs.SetOutput(out)
	fs.StringVar(&o.configFile, "config", "", "key=value config `file`; flags given on the command line win")
	fs.StringVar(&o.name, "name", "", "printer name shown to clients (default \"Brother <model>\")")
	fs.StringVar(&o.location, "location", "", "printer location")
	fs.IntVar(&o.port, "port", 8631, "TCP port for IPP and IPPS")
	fs.StringVar(&o.device, "device", "auto", "printer: auto, /dev/usb/lpN or tcp://host:9100")
	fs.StringVar(&o.profile, "profile", "ql700", "embedded profile name or path to a profile JSON")
	fs.StringVar(&o.media, "media", "62", "media id from the profile (continuous roll)")
	fs.StringVar(&o.mediaPolicy, "media-policy", "roll", "loaded media advertised to clients: roll or roll+presets")
	fs.StringVar(&o.presets, "presets", "29,50", "preset label lengths in mm, comma separated")
	fs.StringVar(&o.trim, "trim", "max", "blank rows trimmed per label end: max (no limit) or N")
	fs.StringVar(&o.dither, "dither", "auto", "dithering: auto, on or off")
	fs.BoolVar(&o.highRes, "high-res", false, "offer print-quality high (300x600 dpi); for print, print in high resolution")
	fs.StringVar(&o.stateDir, "state-dir", "", "directory for the UUID, TLS key pair and spool (default $STATE_DIRECTORY, /var/lib/ql-airprint as root, else ~/.local/state/ql-airprint)")
	fs.StringVar(&o.mdns, "mdns", "auto", "DNS-SD advertising: auto, avahi, builtin or off")
	fs.StringVar(&o.dryRun, "dry-run", "", "write job-<n>.bin and .txt to `DIR` instead of printing")
	fs.StringVar(&o.debugDir, "debug-dir", "", "keep each job's input and label PNGs in `DIR`")
	fs.DurationVar(&o.statusInterval, "status-interval", 30*time.Second, "how often to query the printer status while idle; 0 never queries it")
	fs.DurationVar(&o.retryDeadline, "retry-deadline", 120*time.Second, "how long to retry a label while the printer is busy or absent")
	fs.BoolVar(&o.verbose, "v", false, "log every IPP request")
	return fs
}

// parseArgs parses flags anywhere on the command line, so both
// "ql-airprint --dry-run d print f" and "ql-airprint print f --dry-run d" work.
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

func parseTrim(s string) (int, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "max", "unlimited", "-1":
		return -1, nil
	}
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n < 0 {
		return 0, fmt.Errorf("--trim %q: want max or a row count", s)
	}
	return n, nil
}

func parseDither(s string) (string, error) {
	switch s {
	case "auto", "on", "off":
		return s, nil
	}
	return "", fmt.Errorf("--dither %q: want auto, on or off", s)
}

// parsePresets turns "29,50" (mm) into hundredths of a millimetre.
func parsePresets(s string) ([]int, error) {
	var out []int
	for _, f := range strings.Split(s, ",") {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		mm, err := strconv.ParseFloat(strings.TrimSuffix(f, "mm"), 64)
		if err != nil || mm <= 0 || mm > 1000 {
			return nil, fmt.Errorf("--presets: bad length %q", f)
		}
		out = append(out, int(math.Round(mm*100)))
	}
	return out, nil
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
		return "/var/lib/ql-airprint", nil
	}
	if x := os.Getenv("XDG_STATE_HOME"); x != "" {
		return filepath.Join(x, "ql-airprint"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", errors.New("no --state-dir and no home directory")
	}
	return filepath.Join(home, ".local", "state", "ql-airprint"), nil
}
