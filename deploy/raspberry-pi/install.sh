#!/usr/bin/env sh
set -eu

APP_DIR="/opt/hall-clock"
CONFIG_DIR="/etc/hall-clock"
BIN_SRC="${1:-/tmp/hall-clock}"
BIN_DST="${APP_DIR}/hall-clock"
UNIT_DIR="/etc/systemd/system"
CADDY_DIR="/etc/caddy"

# The mDNS name this Pi answers on: http://$HALL_HOST.local. It sets the system
# hostname and the URL the pairing QR code sends phones to. (Caddy needs no name —
# it matches any *.local host.)
#
# Two halls on one LAN need two different names, or their Pis fight over
# hallclock.local and phones pair with whichever one won:
#
#   sudo HALL_HOST=hallclock-b ./install.sh /tmp/hall-clock
HALL_HOST="${HALL_HOST:-hallclock}"

case "$HALL_HOST" in
  '' | *[!a-z0-9-]* | -* | *-)
    echo "HALL_HOST must be lowercase letters, digits, and inner hyphens (e.g. hallclock-b)"
    exit 1
    ;;
esac

# The hall's identity lives here, in /etc, and nowhere else. Updates reinstall the
# units and the Caddyfile from the release payload (see hall-clock-update.sh), so
# anything written into *those* files would be reverted on the next update. This
# file is not part of the payload, so it survives.
write_hall_env() {
  install -d -m 0755 "$CONFIG_DIR"
  printf '# Set by install.sh (HALL_HOST). The .local name this Pi answers on\n' >"$CONFIG_DIR/hall.env"
  printf '# and advertises in its pairing QR code. Survives updates.\n' >>"$CONFIG_DIR/hall.env"
  printf 'HALL_HOST=%s\n' "$HALL_HOST" >>"$CONFIG_DIR/hall.env"
  chmod 0644 "$CONFIG_DIR/hall.env"
}

# The control token lives in config.json and is only minted when absent, so a
# config carried over from another hall (imaging a second Pi from the first one's
# SD card) leaves both halls sharing one token — and a phone paired to either can
# then drive both. A hostname change is exactly the signal that this box is
# becoming a *different* hall than the config it is holding.
warn_shared_token() {
  if ! grep -sq '"controlToken": *"[^"]' "$CONFIG_DIR/config.json"; then
    return 0
  fi

  echo
  echo "WARNING: $CONFIG_DIR/config.json already holds a control token, and this"
  echo "         Pi is being renamed to $HALL_HOST."
  echo "         If this card was imaged from another hall's Pi, both halls now"
  echo "         share one token and one phone can control both. Start clean:"
  echo
  echo "           sudo rm $CONFIG_DIR/config.json && sudo systemctl restart hall-clock"
  echo
  echo "         (Renaming this same hall's own Pi? Then keep it — the token, the"
  echo "         schedule, and the paired phones all stay valid.)"
  echo
}

set_hostname() {
  current="$(hostname)"
  if [ "$current" = "$HALL_HOST" ]; then
    return 0
  fi

  warn_shared_token
  echo "Hostname: $current -> $HALL_HOST"
  hostnamectl set-hostname "$HALL_HOST"
  # Raspberry Pi OS resolves its own hostname through this 127.0.1.1 line. Leave
  # it pointing at the old name and every later sudo stalls on the lookup.
  if grep -q '^127\.0\.1\.1' /etc/hosts; then
    sed -i "s/^127\.0\.1\.1.*/127.0.1.1\t${HALL_HOST}/" /etc/hosts
  else
    printf '127.0.1.1\t%s\n' "$HALL_HOST" >>/etc/hosts
  fi
  # Avahi keeps advertising the old .local name until it re-reads the hostname.
  systemctl restart avahi-daemon 2>/dev/null || true
}

ensure_caddy() {
  if command -v caddy >/dev/null 2>&1 && [ -d "$CADDY_DIR" ]; then
    return 0
  fi

  echo "Caddy not found; installing with apt..."
  export DEBIAN_FRONTEND=noninteractive
  apt-get update
  apt-get install -y caddy
}

# Hides the X pointer on the display (see hall-clock-kiosk.sh). Not fatal if it
# will not install: the kiosk script skips it when absent, and the display page
# still carries cursor:none.
ensure_unclutter() {
  command -v unclutter >/dev/null 2>&1 && return 0
  echo "unclutter not found; installing with apt..."
  export DEBIAN_FRONTEND=noninteractive
  apt-get install -y unclutter && return 0
  # Stale package lists are the usual reason the first attempt fails on a Pi
  # that has not seen apt in months. ensure_caddy only updates them when Caddy
  # is missing too, which on an existing box it is not.
  apt-get update && apt-get install -y unclutter && return 0
  echo "could not install unclutter; the pointer may show on the display"
}

if [ ! -f "$BIN_SRC" ]; then
  echo "Missing binary at $BIN_SRC"
  echo "Build and copy it first, or pass the binary path: sudo ./install.sh /path/to/hall-clock"
  exit 1
fi

ensure_caddy
ensure_unclutter

if ! systemctl list-unit-files caddy.service >/dev/null 2>&1; then
  echo "Caddy service not found."
  echo "Caddy installed but no systemd service was found."
  exit 1
fi

set_hostname
write_hall_env

install -d -m 0755 "$APP_DIR"
install -d -m 0755 "$CONFIG_DIR"
install -m 0755 "$BIN_SRC" "$BIN_DST"
install -m 0644 hall-clock.service "$UNIT_DIR/hall-clock.service"
install -m 0644 hall-clock-kiosk.service "$UNIT_DIR/hall-clock-kiosk.service"
install -m 0644 hall-clock-update.service "$UNIT_DIR/hall-clock-update.service"
install -m 0644 hall-clock-update-check.service "$UNIT_DIR/hall-clock-update-check.service"
install -m 0644 hall-clock-update.timer "$UNIT_DIR/hall-clock-update.timer"
install -m 0644 hall-clock-update.path "$UNIT_DIR/hall-clock-update.path"
install -m 0644 hall-clock-housekeeping.service "$UNIT_DIR/hall-clock-housekeeping.service"
install -m 0644 hall-clock-housekeeping.timer "$UNIT_DIR/hall-clock-housekeeping.timer"
install -m 0755 hall-clock-kiosk.sh "$APP_DIR/hall-clock-kiosk.sh"
install -m 0755 hall-clock-update.sh "$APP_DIR/hall-clock-update.sh"
install -m 0755 hall-clock-housekeeping.sh "$APP_DIR/hall-clock-housekeeping.sh"
install -m 0644 Caddyfile "$CADDY_DIR/Caddyfile"
# Root owns the app directory: hall-clock-update.service executes
# hall-clock-update.sh from here as root, so nothing the unprivileged app user
# can write may live on that path. The app only needs to read/exec the binary.
chown -R root:root "$APP_DIR"
# The app (running as pi) writes config.json here, so pi owns the directory —
# but update.env feeds the root updater, so it must stay root's alone; the
# updater independently refuses a non-root-owned env file.
chown -R pi:pi "$CONFIG_DIR"
if [ -f "$CONFIG_DIR/update.env" ]; then
  chown root:root "$CONFIG_DIR/update.env"
  chmod 0644 "$CONFIG_DIR/update.env"
fi

# The app listens on a Unix socket owned by pi:pi (mode 0660). Add the caddy
# user to the pi group so the reverse proxy can connect to it. (Restarting
# caddy below picks up the new group membership.)
if id caddy >/dev/null 2>&1; then
  usermod -aG pi caddy
fi

systemctl daemon-reload
# Clean up units and kiosk state from the pre-rename wall-clock install. Leaving
# the old Chromium kiosk running can fill /home with browser metrics/cache data.
systemctl disable --now wall-clock.service wall-clock-kiosk.service 2>/dev/null || true
rm -rf /home/pi/.config/wall-clock-kiosk
systemctl enable hall-clock.service
systemctl enable hall-clock-kiosk.service
systemctl enable hall-clock-update.timer
systemctl enable hall-clock-update.path
systemctl enable hall-clock-housekeeping.timer
systemctl restart hall-clock.service
systemctl restart hall-clock-kiosk.service
systemctl restart hall-clock-update.timer
systemctl restart hall-clock-update.path
systemctl restart hall-clock-housekeeping.timer
systemctl restart caddy.service

echo "Hall Clock installed."
echo "Version: $("$BIN_DST" -version)"
echo "Display: http://${HALL_HOST}.local/display"
echo "Pair:    http://${HALL_HOST}.local/pair"
echo "Name it: http://${HALL_HOST}.local/setup > Device name (shown on the controller)"
echo "Updates: checked nightly; installed from Setup > Software, or with"
echo "         sudo systemctl start hall-clock-update.service"
