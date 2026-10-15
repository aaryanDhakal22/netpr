# NetPr: print from anywhere

A small Go service that lets you print to your home printer from any device,
anywhere. Upload a file (or share it from your phone), check the preview, and
it goes straight to the printer over IPP. No drivers, no CUPS, no VPN.

Printers that take PDF get PDF. Printers that don't (many AirPrint-only
models, including the Canon LBP246dw) get each page rendered at the printer's
resolution as PWG Raster, the format AirPrint and IPP Everywhere use.

## Features

- **Prints** PDF, JPG/PNG, Word/Excel/PowerPoint (and OpenDocument), and plain text.
  Non-PDFs are converted by a [Gotenberg](https://gotenberg.dev) sidecar.
- **Preview** pages before printing (pdf.js, in the browser).
- **Options:** copies, double-sided (on by default), page ranges like `1-3,5`.
- **Multi-file** drag & drop; files are typed by their contents, not their names.
- **Big-job check:** asks before printing more than 30 sheets.
- **Live status** for each job: a progress bar through each stage, and a toast
  when it has printed (or failed).
- **Printer health** everywhere: toner level, paper in each tray, jams, open
  doors; out-of-paper shows in the header even on printers that only report it
  as a warning.
- **Printer page** for troubleshooting: full status and diagnostics, the printer's
  own queue (cancel stuck jobs from any device), a test page, a test
  notification, print defaults, and changing the printer's address.
- **Pushover** alerts for finished or failed jobs, printer problems, and low toner.
- **History** of every job (who, what, when, outcome). Documents themselves
  are deleted as soon as they are sent.
- **Android app:** install as a PWA and print from any app's share sheet.
- **Auth** by your existing Authelia (passkey/YubiKey or password) via Traefik.
- **Ops:** `/healthz`, `/readyz`, Prometheus `/metrics` on an internal port,
  JSON logs, a ~50 MB Alpine image running as non-root.

## How it works

```
Phone / laptop ──HTTPS──▶ Traefik ──ForwardAuth──▶ Authelia
                             │
                             ▼
                          netpr ──IPP (Print-Job)──▶ printer
                             │
                             └──▶ Gotenberg (Office/images/text → PDF, page trimming)
```

1. Upload: each file is streamed to disk, typed by content, converted to PDF,
   and its pages counted.
2. Preview & options: you confirm; large jobs ask once more.
3. Print: if the printer can't print PDF, pages are rendered with poppler's
   `pdftoppm` (300 dpi grayscale for the LBP246dw) and encoded as PWG Raster.
   Jobs are sent one at a time with IPP `Print-Job`, then followed with
   `Get-Job-Attributes` until the printer reports them done.

## Deploy

Requires Docker with Compose, and an existing Traefik + Authelia on the same host.

```sh
cp .env.example .env    # fill in host, printer IP, Traefik names, trusted proxy range
docker compose up -d --build
docker compose logs -f netpr
```

On startup netpr asks the printer what it supports and logs it, for example:

```json
{"msg":"printer found","model":"Canon LBP245/246/248","sends_as":"Page images (PWG Raster, 300 dpi)","formats":[...],"sides":[...]}
```

It logs an error if the printer accepts neither PDF nor grayscale PWG Raster.
Not sure of the address? Open the **Printer** page, type the printer's IP into
*Printer address* and press *Test*: it checks the address read-only and finds
the `ipps://` form if the printer refuses plain `ipp://`.

### Printer notes: Canon imageCLASS LBP246dw

- No PDF, PostScript or PCL over IPP; netpr sends 300 dpi PWG Raster.
- Plain `ipp://` on port 631 answers `426 Upgrade Required`; use
  `ipps://<printer-ip>:443/ipp/print`.
- Out of paper is reported as `media-needed-warning`; jobs wait in the printer
  until paper is loaded and then print by themselves.

See [docs/authelia.md](docs/authelia.md) for the Traefik middleware, the
Authelia rule, and passkey login.

### Configuration

All settings are environment variables; see [`.env.example`](.env.example).
The printer address and the print defaults (copies, double-sided, paper size,
big-job check, toner alert) can also be changed on the **Printer** page. Values
saved there are stored in the database and override `.env` until you press
"Reset to .env" / "Use the .env address".

| Variable | Default | |
|---|---|---|
| `NETPR_PRINTER_URI` | (required) | e.g. `ipps://<printer-ip>:443/ipp/print`; can be overridden on the Printer page |
| `NETPR_TRUSTED_PROXIES` | (required) | CIDRs allowed to send `Remote-User` |
| `NETPR_HOST`, `TRAEFIK_*` | | Used by compose.yaml labels |
| `NETPR_MAX_UPLOAD_MB` | `50` | Per file |
| `NETPR_CONFIRM_SHEETS` | `30` | Ask before printing more sheets than this (0 disables) |
| `NETPR_TONER_LOW_PCT` | `15` | Toner alert threshold |
| `NETPR_PAPER_SIZE` | `letter` | `letter` or `a4`, used when printing photos |
| `NETPR_STAGING_TTL` | `1h` | Unprinted uploads are deleted after this |
| `PUSHOVER_TOKEN`, `PUSHOVER_USER` | | Enable Pushover notifications |
| `NETPR_DRY_RUN` | `false` | Record jobs but never send them |
| `NETPR_DEV_USER` | | Local development only: skip the Authelia check |

### Monitoring

The internal listener on `:9090` is never routed through Traefik:

- `/healthz`: process is up (used by the container healthcheck)
- `/readyz`: database, Gotenberg and printer status as JSON (503 if any is down)
- `/metrics`: `netpr_jobs_total{state}`, `netpr_pages_printed_total`,
  `netpr_sheets_printed_total`, `netpr_conversions_total`, `netpr_printer_up`,
  `netpr_printer_marker_level_percent{marker}`, `netpr_printer_tray_level{tray}`,
  and more.

## Development

Needs Go 1.26, and poppler's `pdftoppm`/`pdfinfo` (`poppler-utils`) for
printers without PDF support.

```sh
docker run -d --rm -p 127.0.0.1:3999:3000 gotenberg/gotenberg:8.37.0

NETPR_PRINTER_URI=ipps://<printer-ip>:443/ipp/print \
NETPR_GOTENBERG_URL=http://127.0.0.1:3999 \
NETPR_DEV_USER=me NETPR_DRY_RUN=true NETPR_DATA_DIR=./data \
go run ./cmd/netpr
# → http://localhost:8080
```

Tests:

```sh
go test ./...                       # unit + HTTP tests (fake IPP printers, fake Gotenberg);
                                    # raster tests are skipped without pdftoppm
NETPR_TEST_GOTENBERG=http://127.0.0.1:3999 \
  go test -tags integration ./internal/convert   # real conversions
```

### Layout

```
cmd/netpr/          entry point, internal listener, healthcheck subcommand
internal/config     environment configuration
internal/server     Echo routes, Authelia header check, CSRF, templates
internal/jobs       stage → convert → plan → send → follow
internal/printer    IPP client (print, status, queue, cancel), monitor, alert rules
internal/settings   Printer-page overrides of .env values
internal/raster     PDF → PWG Raster for printers without PDF support
internal/testpage   built-in one-page test PDF
internal/convert    content sniffing and the Gotenberg client
internal/pagerange  page-range parsing and sheet estimates
internal/staging    uploaded files awaiting print
internal/store      SQLite job history
internal/notify     Pushover
internal/metrics    Prometheus text exposition
web/                templates, CSS/JS, vendored htmx + pdf.js (embedded)
```

## Roadmap

- [x] Authentication (Authelia)
- [x] Print queue / history
- [x] Mobile UI and share sheet
- [x] Preview before printing
- [x] Printer troubleshooting page and settings
- [x] Printers without PDF support (PWG Raster)
- [ ] Email-to-print (IMAP poller; plugs in via `jobs.Source`)
- [ ] Multiple printers
