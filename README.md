# ql-airprint

`ql-airprint` makes a Brother QL label printer show up on your network as an
AirPrint / IPP Everywhere printer for continuous-roll labels. iPhones, iPads and Macs
print to it with no driver and no app-specific plugin.

It is a single static Go binary. It contains:

- an IPP/2.0 server (IPP and IPPS on one port, with a self-signed certificate);
- DNS-SD advertising through avahi-daemon, or a built-in mDNS responder;
- an Apple URF and PWG raster decoder;
- the QL raster encoder and a USB (`usblp`) or raw TCP 9100 printer driver.

## Hardware

| Printer | Media | Status |
|---|---|---|
| Brother QL-700 | 62 mm continuous roll (DK-22205) | verified |
| Other QL models | | unverified |

Only the QL-700 with the 62 mm continuous roll has been tested end to end. The encoder
follows Brother's public raster command reference, so other QL models may work with a
matching profile (`--profile path/to/profile.json`), but they are unverified. Die-cut
labels are not supported: the server offers a continuous roll and trims each label to
its content.

## Install

You need Go 1.24 or later. The binary has no cgo dependencies, so cross-compiling for a
Raspberry Pi or a small server is one command:

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o ql-airprint ./cmd/ql-airprint
# or GOARCH=amd64
```

On the Linux machine the printer is plugged into:

```sh
sudo install -m 0755 ql-airprint /usr/local/bin/
sudo install -d /etc/ql-airprint
sudo install -m 0644 packaging/ql-airprint.conf /etc/ql-airprint/
sudo install -m 0644 packaging/ql-airprint.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now ql-airprint
journalctl -u ql-airprint -f
```

The unit runs as a dynamic user in the `lp` group, keeps its state (printer identity,
TLS key pair, spool) in `/var/lib/ql-airprint`, and is sandboxed. Edit
`/etc/ql-airprint/ql-airprint.conf` to set the name and location.

### USB permissions

The printer appears as `/dev/usb/lp0` (or `lp1`, ...) through the kernel's `usblp`
driver. Most distributions give these nodes to the `lp` group. If yours does not, add a
udev rule, e.g. `/etc/udev/rules.d/60-brother-ql.rules`:

```
SUBSYSTEM=="usbmisc", KERNEL=="lp*", ATTRS{idVendor}=="04f9", GROUP="lp", MODE="0660"
```

then `sudo udevadm control --reload && sudo udevadm trigger`.

The QL-700 has an "Editor Lite" switch. With it on (green LED), the printer shows up as a
USB drive instead of a printer. Turn it off.

## Usage

```
ql-airprint [flags] [serve | print FILE | status | version]
```

`serve` is the default. Flags may come before or after the command, and every flag can
also be set in a `key = value` config file given with `--config`; the command line wins.

| Flag | Default | |
|---|---|---|
| `--name` | `Brother <model>` | name shown to clients |
| `--location` | | printer location |
| `--port` | `8631` | IPP and IPPS port |
| `--device` | `auto` | `auto`, `/dev/usb/lpN` or `tcp://host:9100` |
| `--profile` | `ql700` | embedded profile name or a profile JSON file |
| `--media` | `62` | media id from the profile |
| `--media-policy` | `roll` | `roll` or `roll+presets` (see below) |
| `--presets` | `29,50` | preset label lengths in mm |
| `--trim` | `max` | blank rows trimmed from each label end: `max` or a count |
| `--dither` | `auto` | `auto`, `on` or `off` |
| `--high-res` | off | offer print-quality high (300×600 dpi) |
| `--state-dir` | see below | printer identity, TLS key pair, spool |
| `--mdns` | `auto` | `auto`, `avahi`, `builtin` or `off` |
| `--dry-run DIR` | | write encoded jobs to `DIR` instead of printing |
| `--debug-dir DIR` | | keep each job's input and label PNGs |
| `--status-interval` | `30s` | how often to query the printer while idle; `0` never |
| `--retry-deadline` | `2m0s` | how long to retry a label while the printer is busy or absent |
| `-v` | | log every IPP request |

The state directory is `$STATE_DIRECTORY` under systemd, `/var/lib/ql-airprint` for root
and `~/.local/state/ql-airprint` otherwise. The printer's UUID is derived from a random
seed stored there, so clients keep recognising the printer across restarts. Delete the
directory to give it a new identity.

Try it without a printer:

```sh
ql-airprint --dry-run /tmp/ql-jobs --name "Test Labels" -v
```

Print from an iPhone or Mac. Each label lands in `/tmp/ql-jobs/job-<n>.bin`, the exact
bytes the printer would get, next to a `job-<n>.txt` with the decoded commands.

Other commands:

```sh
ql-airprint print label.png      # one label, padded to the printable width
ql-airprint print document.urf   # URF or PWG raster, same pipeline as IPP jobs
ql-airprint status               # query the printer once
```

## How a job is printed

1. The document (URF from Apple devices, PWG raster from other IPP Everywhere clients) is
   decoded to grey.
2. 600 dpi pages are halved to the 300 dpi head.
3. Each page is binarized. Photo-like pages (many mid-grey pixels) are dithered;
   text and line art are thresholded.
4. The page must be the roll's width (724-740 dots for 62 mm at 300 dpi). Otherwise the
   whole job is refused before anything prints, because the page came in on the wrong
   axis or for the wrong media.
5. The page is centre-cropped to the 696 printable pins and blank rows are trimmed from
   both ends. Blank pages are skipped.
6. Every page is converted and checked first. Then each label is sent as its own printer
   job and cut.

If the printer is busy (another program has the device open) or not connected, a label is
retried with back-off for up to two minutes. It is never retried once bytes have reached
the printer, so a label is not printed twice.

## iOS behaviour and the media policy

The server advertises the loaded medium as a 62 mm roll with a length range
(`media-col-ready` with a `y-dimension` range, `printer-kind` `label,roll`). For a roll,
iOS asks the app how long the label should be: UIKit's
`printInteractionController(_:cutLengthFor:)`. It then rasterises one page exactly as
long as the content. Label apps that implement it print tight labels of any length.

The preset sizes (62×29 mm and 62×50 mm by default) are always listed as *supported*
media. With `--media-policy roll+presets` they are also advertised as *loaded*:

- **Pro:** the iOS print sheet shows a paper-size menu.
- **Con:** apps that choose the "best loaded paper" stop using the roll. They print on a
  fixed-size page with the content centred in it. Trimming removes the blank space, but
  the label length is then decided by the content rather than the app.

`roll` is the default because it gives the most predictable results.

Clients may send any `media-col` whose width matches the roll (±1 mm) and whose length is
inside the roll's range or equal to a preset. This is accepted even with
`ipp-attribute-fidelity=true`.

### Why print quality is "normal" only

The QL print head is 300 dpi across the tape. `print-quality=high` can only mean 600 dpi
along the feed, which the printer's high-resolution mode supports. Advertising `high`
makes some clients send 600×600 dpi rasters, four times the data, which are halved across
anyway. The high-resolution mode also prints more slowly. So by default the server offers
only `normal` and `RS300`. `--high-res` adds `high` and `RS300-600`; jobs that ask for
`high` then keep 600 dpi along the feed.

## Running next to CUPS and avahi

- **Port.** The default port 8631 does not clash with CUPS on 631. Clients find the port
  through DNS-SD, so it does not matter which one you use.
- **Avahi.** With `--mdns auto`, `ql-airprint` registers its services with avahi-daemon
  over D-Bus when avahi is running, and only uses its own responder when it is not. Never
  force `builtin` on a host that runs avahi-daemon: two responders on UDP 5353 give
  conflicting answers.
- **CUPS.** Do not also set up a CUPS queue for the same USB printer. CUPS's USB backend
  can claim the device while it prints, and `ql-airprint` then sees it as busy and
  retries.

## Troubleshooting

- **The printer does not show up.**
  - Check the log for the "advertising" line.
  - Run `avahi-browse -rt _ipps._tcp` (Linux) or `dns-sd -B _ipps._tcp` (macOS) from
    another machine.
  - Open TCP 8631 and UDP 5353 in the firewall.
  - mDNS does not cross subnets or most guest Wi-Fi networks.
- **"device busy" or "no printer connected" in the log.** Another process holds
  `/dev/usb/lpN`, or the printer is off, or it is in Editor Lite mode. Check with
  `ql-airprint status` and `ls -l /dev/usb/`.
- **Permission denied on `/dev/usb/lp0`.** The service user must be in the device's group;
  see USB permissions above.
- **A job is aborted with "page width does not match the roll".** The client rendered for
  a different paper size or orientation. Use the 62 mm roll in the print sheet.
- **The label is longer or shorter than expected.** Run with `--debug-dir DIR`: the input
  document and the label PNG of each page are kept there for inspection.
- **The printer reports a cover-open, media-empty or jam error.** The state is shown as
  `printer-state-reasons` on clients and on the status page at `http://<host>:8631/`.

## Licence

MIT. See [LICENSE](LICENSE).
