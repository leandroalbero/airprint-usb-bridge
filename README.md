# airprint-usb-bridge

`airprint-usb-bridge` makes a USB printer show up on your network as an AirPrint / IPP
Everywhere printer. iPhones, iPads, Macs and driverless Linux clients print to it without a
driver or an app plugin. Pages are turned into printer bytes by a pluggable driver, so the same
binary can serve a label printer, a mono laser, or any printer that has a CUPS raster driver.

It is a single static Go binary, with no cgo. It contains:

- an IPP/2.0 server: IPP and IPPS share one port, with a self-signed certificate;
- DNS-SD advertising through avahi-daemon, or a built-in mDNS responder;
- an Apple URF and PWG raster decoder;
- printer drivers, and USB (`usblp`), raw TCP 9100 and dry-run transports.

## Do you need this?

Often you don't.

- **Plain CUPS printer sharing is enough for most office printers.** If the machine already runs
  CUPS and the printer works there, share the queue. CUPS advertises it as AirPrint, and its
  filters handle PDF, colour and finishing far better than this tool.
- **Use [ipp-usb](https://github.com/OpenPrinting/ipp-usb) for IPP-over-USB printers.** Most
  printers sold since about 2015 already speak IPP Everywhere over USB (USB class 7, protocol
  4). ipp-usb exposes the printer's own IPP service on the network. This tool detects such
  printers and refuses them, unless you force a driver.

It helps in these cases:

- **Label printers that CUPS handles badly with iOS.** CUPS queues for label printers offer
  fixed page sizes. This tool advertises a continuous roll with a length range, so iOS asks
  the app for the cut length and prints labels of any length. It also handles cutting,
  trimming and the printer's status replies.
- **Small single-binary boxes without CUPS.** A Raspberry Pi or a router-class device with a
  label printer or an old laser attached. Memory use is capped (see `packaging/`).
- **Printers that only have a CUPS driver.** The `cups` driver runs that driver's filter
  directly, without cupsd.

## Drivers

| Driver | Printers | Verified on hardware |
|---|---|---|
| `brother-ql` | Brother QL label printers, continuous rolls | **yes**: QL-700 with the 62 mm DK-22205 roll. Other QL models: no |
| `pcl` | mono lasers that accept PCL 5 raster | no |
| `postscript` | PostScript level 2/3 printers | no |
| `cups` | any printer with a CUPS raster driver (PPD plus filter) | no |
| `dymo` | DYMO LabelWriter 400, 450, 450 Turbo, 4XL (not the 5xx series) | no |
| `zpl` | Zebra and other ZPL II label printers | no |

Only the Brother QL-700 has been tested end to end. Every other driver and model is built from
public protocol references and checked with golden-byte tests, decoders written independently
of the encoders, and IPP conformance runs against a dry-run instance. It has not printed on real
hardware. Reports of success or failure are welcome.

`airprint-usb-bridge drivers` lists every driver with its media ids and `--driver-opt`
settings.

### brother-ql

The original purpose of this tool. Each label is sent as its own printer job and cut. The
printer's status replies are read between labels, so paper-out, cover-open and jams show up on
the client.

The profile is chosen from the printer's model (IEEE 1284 ID, then USB product ID), or set with
`--profile NAME|FILE`:

| Model | Head | Notes |
|---|---|---|
| QL-700 | 720 pins, 300 dpi | **verified** with 62 mm continuous roll |
| QL-500, QL-550, QL-560, QL-650TD | 720 pins | unverified; QL-500 has no cutter |
| QL-570, QL-580N, QL-710W, QL-720NW | 720 pins | unverified |
| QL-800, QL-810W, QL-820NWB | 720 pins | unverified; prints black only (two-colour rolls are not supported) |
| QL-1050, QL-1060N, QL-1100, QL-1110NWB | 1296 pins | unverified; up to 102 mm (103 mm on the QL-1100/1110NWB) |

`--media` picks the continuous roll width in mm: `62` (default), `12`, `29`, `38`, `50`,
`54`, and `102`/`103` on the wide models. Die-cut labels are not offered: the server
advertises a continuous roll and trims each label to its content. `--driver-opt compress=true`
sends TIFF-compressed lines on models that support it; this is off by default and has not been
tried on a printer.

### pcl

PCL 5 raster for mono lasers: a PJL job wrapper, page size, 300 or 600 dpi raster with
compression mode 2 (TIFF PackBits) or 3 (delta row), and optional duplex.

- Media: A4 (default), Letter, Legal, A5, Executive (`--media a4|letter|...`).
- Margins: 4.23 mm top and bottom; the sides follow PCL's logical page, about 6 mm.
- Options: `dpi=300|600|300,600`, `duplex=true`, `compression=2|3`, `pjl=false`.
- Auto-selected when the device ID's command set lists PCL. Printers that list only PCL 3 GUI
  (many inkjets) get a warning: their PCL dialect is vendor-specific.

### postscript

Each page is sent as a PostScript level 2 `image`, by default 1-bit, RunLength and ASCII85
encoded. `setpagedevice` sets the page size and duplex.

- Media as for `pcl`.
- Options: `level=2|3` (3 uses Flate), `depth=1|8`, `encoding=ascii85|binary`,
  `resolution=300|600`, `duplex=true`, `pjl=true`.
- `passthrough=true` also accepts `application/postscript` documents and sends them unchanged.
  This is off by default, because a document can change the printer's saved settings.

### cups

Runs a CUPS raster driver without cupsd. It reads the PPD given with `--ppd`, writes CUPS raster
(v3) pages with a correct page header for the chosen options, and runs the PPD's filter for
`application/vnd.cups-raster` directly. There is no shell, and the run has an inactivity
timeout and an output cap. Pages are written to the filter as it reads them, and its output goes
to the printer as it comes. The filter's `ERROR:` and `STATE:` lines go to the job log, and `ERROR:` lines also
become the job's state message.

```sh
airprint-usb-bridge --driver cups --ppd /usr/share/ppd/example/laser.ppd
```

- Media: the PPD's page sizes (`--media A4` or its PWG name); default: the PPD's default.
- Options: `options="Key=Choice ..."` fixes PPD options for every job; `filter`, `filter-dir`,
  `timeout`, `max-output`, `kind=document|label`, `duplex=auto|true|false`, `full-page`,
  `device-uri`.
- `timeout` (default `5m`) is how long the filter may go without reading a page or writing
  output; a long job on a slow printer is not cut short.
- `max-output` (default `16G`) stops a runaway filter. The output is not held in memory.
- Page sizes whose single sheet is over 128 MiB at the PPD's lowest resolution are not offered,
  and a job falls back to a lower resolution when its size does not fit at the one asked for.
- A filter that times out or whose job is cancelled is killed with every process it started.
  When a page fails after printing has started, the filter's input ends there, so it can
  finish the pages before it.
- Filters are looked up in `/usr/lib/cups/filter` and `/usr/libexec/cups/filter`. The filter
  and the PPD must come from the same driver package.
- Not supported: custom page sizes, `UIConstraints` and installable options. Set `duplex=false`
  if no duplexer is fitted.

### dymo

LabelWriter 400, 450, 450 Turbo and 4XL raster protocol at 300 dpi. Each label is its own job.

- Media: die-cut labels by DYMO part number, e.g. `99012` (default, 36×89 mm), `99010`,
  `11352`, `11354`, `11355`, `30332`, `99014`, `30256`; on the 4XL also `1744907` (4×6 in).
  `continuous` or `continuous-<mm>` selects continuous paper.
- Options: `model` (when the device ID is unknown), `density=light|medium|normal|dark`,
  `dot-tab`, `trim`.
- The LabelWriter 5xx series uses a different protocol and authenticated labels, and is refused.

### zpl

Zebra ZPL II. Each label is sent as a `^GFA` graphic field, Z64-compressed by default.

- Media: `4x6` (default), `4x4`, `4x3`, `4x2`, `4x1`, `3x2`, `3x1`, `2.25x1.25`, `2x1`, any
  `WxH[in|mm]`, or `continuous` / `continuous-W[in|mm]`.
- Options: `dpi=203|300|600` (default 203), `darkness=0-30`, `tracking=gap|mark`,
  `encoding=z64|hex` (`hex` if a printer rejects Z64), `print-width`.
- EPL-only printers (device ID without ZPL in its command set) are not auto-selected: they
  would print the ZPL as text. Use the `cups` driver with an EPL PPD for those.

## Install

You need Go 1.24 or later. Cross-compiling for a Raspberry Pi or a small server is one command:

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o airprint-usb-bridge ./cmd/airprint-usb-bridge
# or GOARCH=amd64
```

On the Linux machine the printer is plugged into:

```sh
sudo install -m 0755 airprint-usb-bridge /usr/local/bin/
sudo install -d /etc/airprint-usb-bridge
sudo install -m 0644 packaging/airprint-usb-bridge.conf /etc/airprint-usb-bridge/
sudo install -m 0644 packaging/airprint-usb-bridge.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now airprint-usb-bridge
journalctl -u airprint-usb-bridge -f
```

The unit runs as a dynamic user in the `lp` group and is sandboxed. It keeps its state in
`/var/lib/airprint-usb-bridge`: the printer identity, the TLS key pair and the spool. Edit
`/etc/airprint-usb-bridge/airprint-usb-bridge.conf` to set the name, location and driver.

For the `cups` driver, the PPD must be readable by the service. The filter runs inside the
unit's sandbox (read-only system, private `/tmp`, restricted system calls); a filter that needs
more than that fails there.

**Upgrading from a release under the project's previous name.**

- The default state directory moved to `airprint-usb-bridge`, so a default install starts with a
  new random seed and a new self-signed TLS certificate. The salt that derives the UUID from the
  seed changed too, so copying the old state directory keeps the certificate but not the UUID.
  Either way iOS lists the bridge as a new printer and drops the old entry once nothing
  advertises it.
- `--device auto` used to look for a Brother QL among all USB printers. It now takes the first
  USB printer and picks the driver for it. With a QL and another printer attached, pass
  `--match Brother:QL` or `--driver brother-ql`, or the other printer may be chosen.

### USB permissions

The printer appears as `/dev/usb/lp0` (or `lp1`, ...) through the kernel's `usblp` driver. Most
distributions give these nodes to the `lp` group. If yours does not, add a udev rule such as
`/etc/udev/rules.d/60-usb-printer.rules`:

```
SUBSYSTEM=="usbmisc", KERNEL=="lp*", GROUP="lp", MODE="0660"
```

Then run `sudo udevadm control --reload && sudo udevadm trigger`.

The QL-700 has an "Editor Lite" switch. When it is on (green LED), the printer shows up as a
USB drive instead of a printer. Turn it off.

## Choosing the printer and driver

```sh
airprint-usb-bridge list-printers
```

`list-printers` shows every USB printer with its USB ID, its IEEE 1284 device ID and the driver
auto-selection would pick. It reads Linux sysfs, so it finds nothing on other systems.

With `--driver auto` (the default) the driver comes from the device ID:

| Device ID | Driver |
|---|---|
| MFG Brother, MDL `QL-*` | `brother-ql`, with the profile for that model |
| MFG DYMO, MDL `LabelWriter*` | `dymo` (5xx: refused) |
| a CMD token containing ZPL, or MFG Zebra with no CMD | `zpl` |
| a CMD token starting with PCL | `pcl` |
| a CMD token PS, PS2, PS3 or POSTSCRIPT* | `postscript` |
| anything else | `cups` when `--ppd` is given, otherwise an error listing the options |

`--device auto` uses the first USB printer. `--match MFG:MDL` narrows the choice to printers
whose manufacturer and model contain those substrings, e.g. `--match Brother:QL`. With an
explicit `--driver`, auto takes the first printer that driver would be auto-selected for. With
several printers and no `--match`, the bridge logs a warning listing them. Jobs keep going to the
printer (make and model) chosen at start-up. With no USB printer to examine (dry run, TCP,
printer switched off), auto falls back to `brother-ql` and logs it.

IPP-over-USB printers are skipped by `--device auto` with a warning, and refused when named
explicitly, unless `--driver` is given.

## Usage

```
airprint-usb-bridge [flags] [serve | print FILE | status | list-printers | drivers | version]
```

`serve` is the default. Flags may come before or after the command, and every flag can also be
set in a `key = value` config file given with `--config`; the command line wins.

| Flag | Default | |
|---|---|---|
| `--name` | the make and model | name shown to clients |
| `--location` | | printer location |
| `--port` | `8631` | IPP and IPPS port |
| `--device` | `auto` | `auto`, `/dev/usb/lpN` or `tcp://host:9100` |
| `--match` | | with `--device auto`: `MFG:MDL` substrings |
| `--driver` | `auto` | `auto`, `brother-ql`, `pcl`, `postscript`, `cups`, `dymo`, `zpl` |
| `--driver-opt` | | driver setting `key=value`; repeatable |
| `--ppd` | | PPD file for the `cups` driver |
| `--media` | per driver | media id; see `drivers` |
| `--dither` | `auto` | `auto`, `on` or `off` |
| `--high-res` | off | offer print-quality high where the driver can |
| `--profile` | from the model | brother-ql: built-in profile name or a profile JSON file |
| `--media-policy` | `roll` | brother-ql: `roll` or `roll+presets` (see below) |
| `--presets` | `29,50` | brother-ql: preset label lengths in mm |
| `--trim` | `max` | brother-ql: blank rows trimmed from each label end, `max` or a count |
| `--state-dir` | see below | printer identity, TLS key pair, spool |
| `--mdns` | `auto` | `auto`, `avahi`, `builtin` or `off` |
| `--dry-run DIR` | | write printer bytes to `DIR` instead of printing |
| `--debug-dir DIR` | | keep each job's input and rendered images |
| `--status-interval` | `30s` | how often to query the printer while idle; `0` never |
| `--retry-deadline` | `2m0s` | how long to retry while the printer is busy or absent |
| `-v` | | log every IPP request |

The state directory is `$STATE_DIRECTORY` under systemd, `/var/lib/airprint-usb-bridge` for
root and `~/.local/state/airprint-usb-bridge` otherwise. The printer's UUID comes from a random
seed stored there, so clients keep recognising the printer across restarts. Delete the
directory to give it a new identity.

Try a driver without a printer:

```sh
airprint-usb-bridge --driver pcl --dry-run /tmp/jobs --name "Test Laser" -v
```

Print from an iPhone or Mac. Each device job lands in `/tmp/jobs/job-<n>.bin`, the exact bytes
the printer would get. Next to it is a `job-<n>.txt` with the decoded commands.

Other commands:

```sh
airprint-usb-bridge print label.png      # an image, rendered by the driver
airprint-usb-bridge print document.urf   # URF or PWG raster, same pipeline as IPP jobs
airprint-usb-bridge status               # query the printer once
```

## How a job is printed

1. The document is decoded to grey: URF from Apple devices, PWG raster from other IPP
   Everywhere clients.
2. **Every page is checked before anything is sent**. A page that cannot be decoded or
   printed aborts the job with `document-format-error`, and nothing is half printed. Label
   drivers convert every page first. Sheet drivers (pcl, postscript, cups) decode every page
   once without keeping it, then read the document again and send each page as soon as it
   is converted.
3. Sheet drivers place each page on the sheet. Pages are scaled per `print-scaling` and turned
   to match the sheet's orientation. Unprintable margins are cleared, and the page is then
   binarized: photo-like pages are dithered, text and line art are thresholded.
4. The printer bytes go out one device job at a time: one per label for label printers, one per
   document for sheet printers. Copies are made by repeating the job, or by the printer where the
   driver supports that.

If the printer is busy or not connected, the job is retried with back-off for up to two
minutes. It is never retried once bytes have reached the printer, so nothing is printed twice.
A job that fails or is cancelled after that is aborted. Where the printer language allows, the
device job is still ended (PCL: `ESC E` and, unless `pjl=false`, the PJL end of job;
PostScript: `%%EOF` and, with `pjl=true`, the PJL end of job; cups: the filter's input is
closed), so the printer does not wait for the rest.

Sheet drivers hold about two pages in memory at a time, whatever the length of the document:
the page being decoded and the sheet it becomes at the printer's resolution (35 MB each for A4
at 600 dpi, 9 MB at 300 dpi). A document may be up to 512 MiB and 10000 pages, and the spool
holds up to 1 GiB of waiting documents. A single page or sheet over 128 MiB is refused.

Label drivers convert every page of a job before the first label prints. For them a document
may be up to 64 MiB, and a job's decoded pages are capped at 128 MiB, and so are the sheets
rendered from them at the printer's resolution (about 150 labels of 62x100 mm at 300 dpi).
Longer jobs are refused before anything is printed.

### Brother QL specifics

- 600 dpi pages are halved to the 300 dpi head.
- The page must be the roll's width (724-740 dots for 62 mm at 300 dpi). Otherwise the whole job
  is refused, because the page came in on the wrong axis or for the wrong media.
- The page is centre-cropped to the printable pins. Blank rows are trimmed from both ends, and
  blank pages are skipped.

## iOS and the media policy

### Label printers (brother-ql, and continuous media on dymo and zpl)

The server advertises the loaded medium as a roll with a length range (`media-col-ready` with a
`y-dimension` range, `printer-kind` `label,roll`). For a roll, iOS asks the app how long the label
should be (UIKit's `printInteractionController(_:cutLengthFor:)`). It then rasterises one page
exactly as long as the content. Label apps that implement it print tight labels of any length.

The brother-ql preset sizes (62×29 mm and 62×50 mm by default) are always listed as
*supported* media. With `--media-policy roll+presets` they are also advertised as *loaded*:

- **Pro:** the iOS print sheet shows a paper-size menu.
- **Con:** apps that choose the "best loaded paper" stop using the roll. They print on a
  fixed-size page with the content centred in it. Trimming removes the blank space, but the
  label length is then decided by the content rather than the app.

`roll` is the default because it gives the most predictable results.

Clients may send any `media-col` whose width matches the roll (±1 mm) and whose length is inside
the roll's range or equal to a preset. This is accepted even with `ipp-attribute-fidelity=true`.

Die-cut labels (dymo, zpl) are advertised as fixed sizes, with only the chosen size loaded.

### Why brother-ql print quality is "normal" only

The QL print head is 300 dpi across the tape. `print-quality=high` can only mean 600 dpi along
the feed, which the printer's high-resolution mode supports. Advertising `high` makes some
clients send 600×600 dpi rasters, four times the data, which are halved across anyway. The
high-resolution mode also prints more slowly. So by default the server offers only `normal` and
`RS300`. `--high-res` adds `high` and `RS300-600`, and jobs that ask for `high` then keep 600 dpi
along the feed.

### Document printers (pcl, postscript, cups)

These are advertised as IPP Everywhere / AirPrint sheet printers:

- `printer-kind document`;
- a `media-col-database` of the fixed sizes with their margins, and only the chosen size loaded;
- `URF` with `W8`, `RS300` or `RS300-600`, `CP<n>`, and `DM1` for duplex;
- `sides-supported`, `print-color-mode-supported` (monochrome only in this version),
  `output-bin` and supply attributes;
- `kind`, `PaperMax` and `pdl` in the TXT record.

They pass the IPP/1.1 and IPP/2.0 suites of `ipptool`. In CUPS's `ipp-everywhere.test` they
miss only `image/jpeg` and `overrides-supported`, which the bridge does not implement. Printing
from iOS and macOS uses URF or PWG raster and does not need either.

To check that an iPhone accepts a driver's printer before any hardware is attached, run it with
`--dry-run` and print to it.

## Running next to CUPS and avahi

- **Port.** The default port 8631 does not clash with CUPS on 631. Clients find the port through
  DNS-SD, so it does not matter which one you use.
- **Avahi.** With `--mdns auto`, the bridge registers its services with avahi-daemon over D-Bus
  when avahi is running, and uses its own responder only when it is not. Never force `builtin`
  on a host that runs avahi-daemon: two responders on UDP 5353 give conflicting answers.
- **CUPS.** Do not also set up a CUPS queue for the same USB printer. CUPS's USB backend can
  claim the device while it prints, and the bridge then sees it as busy and retries. The `cups`
  driver does not need cupsd running at all.

## Troubleshooting

- **The printer does not show up.**
  - Check the log for the "advertising" line.
  - Run `avahi-browse -rt _ipps._tcp` (Linux) or `dns-sd -B _ipps._tcp` (macOS) from another
    machine.
  - Open TCP 8631 and UDP 5353 in the firewall.
  - mDNS does not cross subnets or most guest Wi-Fi networks.
- **"no driver matches the printer".** Run `list-printers` to see the device ID, then pass
  `--driver`, or `--ppd` for a CUPS driver.
- **"supports IPP-over-USB".** Use ipp-usb instead. Pass `--driver` only if you really want this
  tool to drive it.
- **"device busy" or "no printer connected" in the log.** Another process holds
  `/dev/usb/lpN`, the printer is off, or a QL is in Editor Lite mode. Check with
  `airprint-usb-bridge status` and `ls -l /dev/usb/`.
- **Permission denied on `/dev/usb/lp0`.** The service user must be in the device's group; see
  USB permissions above.
- **A brother-ql job is aborted with "page width does not match the roll".** The client rendered
  for a different paper size or orientation. Choose the roll in the print sheet.
- **Output is shifted, cropped or the wrong size.** Run with `--debug-dir DIR` to keep the input
  document and the rendered image of each page. Run with `--dry-run DIR` to see the decoded
  printer commands.
- **The printer reports a cover-open, media-empty or jam error.** The state is shown as
  `printer-state-reasons` on clients and on the status page at `http://<host>:8631/`.

## Licence

MIT. See [LICENSE](LICENSE).
