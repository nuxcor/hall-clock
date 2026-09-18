# Hall Clock

Local-only Raspberry Pi hall-clock appliance.

The Pi plugs into a TV/projector over HDMI, runs the hall-clock server locally,
and opens the display in Chromium kiosk mode. Operators control the timer from a
phone on the same Wi-Fi network. Pairing happens from `/pair`; the normal TV
display stays clean and QR-free during meetings.

A phone pairs once by entering the hall's PIN, so being on the Wi-Fi is not by
itself enough to drive the clock — see [Pairing and access](#pairing-and-access).
Nothing about pairing ever appears on the TV.

No cloud service is required during a meeting.

## Architecture

```text
Phone controller -> local Wi-Fi -> Raspberry Pi -> HDMI -> TV/projector
```

The Raspberry Pi runs a single Go binary:

- `/display` full-screen TV clock/countdown
- `/control` mobile operator remote
- `/setup` local device/settings page
- `/api/state` current timer state
- `/api/control/*` start, stop, reset, adjust, bell, and schedule commands
- `/events` Server-Sent Events stream for live display/controller updates
- `/pair` always-available pairing page
- `/qr.png` local QR code for pairing phones to the controller (carries no token)
- `/api/pairing/claim` exchanges the hall PIN for a control token
- `/api/pairing/pin` reads (GET) or changes (POST) the PIN; both need a token
- `/api/pairing/enable` opens a short PIN-free window to add another phone

The app intentionally uses Server-Sent Events instead of WebSockets for the
first version. Displays and controllers only need server-to-client state pushes;
commands are simple local HTTP POST requests. This keeps the Pi runtime smaller
and easier to debug.

Pairing stays available at `/pair` so a printed or bookmarked QR code can always
add a controller phone on the local network.

## Pairing and access

Every write endpoint (`/api/control/*`, `/api/config`, the importers, and
`/api/update`) requires a shared control token. A phone gets that token once, by
entering the PIN the congregation set in `/setup`:

1. The controller or setup page opens with no token and prompts for the PIN.
2. A correct PIN returns the token, which the browser keeps in `localStorage`.
3. That phone never asks again.

The PIN is stored as typed, so `/setup` can show the hall which PIN is current
(behind a Show/Hide toggle) — a shared secret nobody can look up is one that
gets forgotten and reset every few months. Hashing it would buy less than it
looks: `controlToken` sits in the same config file in the clear and is strictly
more powerful, so anyone who can read `config.json` already owns the clock. The
real cost is PIN reuse — **treat this PIN as readable by whoever can read the SD
card, and do not reuse one that unlocks anything else.**

It is still never served with the settings: `/api/config` is built from an
explicit allowlist, and the PIN appears in no state broadcast. Only the
token-protected `GET /api/pairing/pin` returns it, so reading it back requires
an already-paired controller.

Supporting rules:

- Five wrong PINs lock further guessing for five minutes. The attempt is spent
  in the same critical section that reads the lockout, before the PIN is
  compared, so guessing in parallel buys no more tries than guessing in series.
  A success resets the count. The lockout is global rather than per-client, so
  a stranger can block *new* pairings for five minutes — phones already paired
  are unaffected, which is the trade that matters on a meeting night.
- **First boot** opens a PIN-free window so the appliance can be set up at all,
  for 15 minutes. Pairing a phone does not close it: the controller pairs
  silently on load, so closing on the first phone would lock out the laptop the
  PIN is about to be set from. The installer pairs a phone, then sets a PIN.
  Setting one closes the window immediately, and a restart with a PIN
  configured never reopens it. Until a PIN exists the startup log says so
  plainly, and `/setup` shows a warning.
- An already-paired controller can `POST /api/pairing/enable` to open a
  five-minute window, so a second phone can join without being told the PIN.
  One window pairs one phone.
- Changing the PIN does **not** evict paired phones — the control token is
  unchanged — so tidying settings can never log out an operator mid-meeting.
- Printed QR codes carry no token, so they stay valid indefinitely and a photo
  of one grants nothing on its own.
- The TV display is untouched by all of this. It shows the clock and nothing
  else, during meetings and outside them.

If the PIN is forgotten and no phone is still paired, recovery means editing
`/etc/hall-clock/config.json` on the Pi (clear `controlPin`, restart to get a
fresh grace window). With a phone still paired, just read the PIN back in
`/setup` — that is what it is there for.

Every save also writes `config.json.bak` beside it, and the app falls back to
that copy when `config.json` is there but unreadable, so a hall with a PIN never
comes up open to anyone after a bad write. It holds the same PIN and token, with
the same permissions. It means a hand edit that breaks the JSON brings back the
*old* config, PIN included — the log says `restored the last good copy` when
that happens. Deleting `config.json` outright is still a full reset: the copy is
never read when the file is missing.

This is a trusted-LAN appliance, not an internet-facing service: keep it on an
isolated network. The PIN raises the bar from "anyone on the Wi-Fi" to "anyone
the hall gave the PIN to", which is the property the hardware can support.

## Installing on a phone

`/control` and `/setup` ship web app manifests and icons, so a paired phone can
install them from the browser's **Add to Home Screen**. Each gets its own icon
(clock for control, gear for setup) and opens full screen like a native app.

Platform notes, both consequences of serving plain HTTP on a `.local` name (see
the Caddyfile for why HTTP is the right call here):

- **iOS**: installs open standalone with no browser chrome. Safari gives the
  installed app its own `localStorage`, so it asks for the hall PIN once on
  first launch, then stays paired like any other controller.
- **Android**: Chrome only offers a full standalone install over HTTPS, so Add
  to Home Screen creates a shortcut that opens in the browser. Same icon on the
  home screen, one tap to the controller — just with the address bar visible.
  For a real standalone install there is a thin WebView shell in
  [android/](android/README.md), distributed through a closed Play testing
  track. It also holds the screen awake for the length of a meeting, which the
  web app cannot do for itself over plain HTTP.

There is no service worker: one would not register over plain HTTP, and the app
is only meaningful while it can reach the Pi anyway.

## Meeting Data

Weekend meetings use a fixed local template:

- Public Talk: 30 minutes
- Watchtower Study: 60 minutes

The app switches to the weekend schedule automatically on Saturday and Sunday.
Monday through Friday use the saved midweek schedule.

Midweek meetings are expected to change weekly. The setup page supports pasting
weekly timing text or a WOL midweek program URL and parsing only the part titles
and minute values for review before saving. Once saved, the normal TV display
does not depend on internet access.

The setup page can also enable automatic weekly import. The server computes the
date-addressable weekly meetings page (`wol.jw.org/<lang>/wol/meetings/<r>/<lib>/<year>/<isoweek>`),
follows its link to the midweek workbook document, and applies the parsed
timings once per ISO week, starting Monday at 3:00 AM in the Pi's local time.
Failures are retried hourly and always keep the last saved schedule, so
meetings still work offline. The language and library
segments of a previously imported URL are reused, so non-English configurations
keep importing in their own language.

Each device can store multiple weekly start times on any day, including
weekends, with several start times per day for halls shared by congregations
that meet at different hours. The automatic pre-meeting countdown uses the
next configured start time for the current day. New installs are seeded with
Monday-Friday evening starts plus Sunday 10:00.

## Recommended Pi Setup

- Raspberry Pi 5, 4GB or 8GB
- Official USB-C power supply
- Case with active cooling
- microSD card or SSD
- Micro-HDMI to HDMI cable
- Raspberry Pi OS with desktop/Chromium

## Project layout

```text
src/hall-clock/        single Go binary + embedded web/ assets
  main.go              flags and startup
  server.go            routing, SSE, snapshots
  handlers.go          HTTP control/config/import handlers
  timer.go             timer + schedule state machine
  schedule.go          midweek/weekend/circuit-overseer schedules
  config.go            config load/save/normalise
  autoimport.go        WOL weekly-timing import
  pairing.go           token, PIN pairing, QR, advertised-URL resolution
  model.go             core types
android/               thin WebView shell (Play closed track) — see its README
deploy/raspberry-pi/   systemd + Caddy appliance install
deploy/local/          run the same stack on a Mac
scripts/               dev helpers
```

## Development

Common tasks are in the `Makefile` (run `make` to list them):

```sh
make run       # live-reload assets on http://localhost:8480
make test      # go test ./...
make race      # tests with the race detector
make vet       # go vet ./...
make build     # ./hall-clock
make build-pi  # dist/hall-clock-linux-arm64 for the Pi
```

`make run` serves the web assets straight from disk (`-web-dir`), so edits to
HTML/CSS/JS show up on a browser refresh without a rebuild.

Open:

- Controller: http://localhost:8480 (also `/control`)
- Display: http://localhost:8480/display
- Pairing: http://localhost:8480/pair
- Setup: http://localhost:8480/setup

A fresh config has no PIN, so the first run opens a bootstrap window and the
controller pairs without prompting. Set a PIN in `/setup` to exercise the
prompt; delete the config file to get the window back.

## Raspberry Pi Deployment

See [deploy/raspberry-pi/README.md](deploy/raspberry-pi/README.md).

The appliance runs the Go app on a Unix socket behind Caddy on port 80, so
phones use a clean URL such as `http://hallclock.local` with no port. On a Mac
you can preview the same stack — see [deploy/local/README.md](deploy/local/README.md).

## License

[MIT](LICENSE) © Daniel Ehoneah
