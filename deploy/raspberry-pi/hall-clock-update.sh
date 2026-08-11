#!/usr/bin/env bash
# Pull-based updater, in two modes.
#
#   --check   Compare the running binary against the latest GitHub release and
#             record the answer for the setup page. Installs nothing. This is
#             what the nightly timer runs: no Pi ever restarts unattended.
#   (default) Install the latest release if it differs from what is running.
#             Triggered by a person: the Update button on the setup page (via
#             hall-clock-update.path), or `systemctl start hall-clock-update`
#             over ssh.
#
# Designed to be safe on a box nobody can ssh into: it refuses to restart a
# meeting in progress, verifies what it downloads, swaps the binary atomically,
# and rolls back if the new one does not come up.
set -euo pipefail

MODE="install"
if [ "${1:-}" = "--check" ]; then
  MODE="check"
fi

APP_DIR="/opt/hall-clock"
STATE_DIR="/var/lib/hall-clock"
BIN="${APP_DIR}/hall-clock"
PREVIOUS="${APP_DIR}/hall-clock.previous"
DEPLOY_ASSET="hall-clock-raspberry-pi.tar.gz"
TRIGGER="${STATE_DIR}/update-requested"
STATUS="${STATE_DIR}/update-status.json"
SOCKET="/run/hall-clock/app.sock"
SERVICE="hall-clock.service"
REPO="nuxcor/hall-clock"
ENV_FILE="/etc/hall-clock/update.env"
LOCK="${STATE_DIR}/update.lock"
UNIT_DIR="/etc/systemd/system"
CADDY_DIR="/etc/caddy"

log() { echo "hall-clock-update: $*"; }

# Normally systemd's StateDirectory= has already created this, owned by pi so the
# app can drop its trigger file. Recreate it with the same ownership if a run
# beats the service to it.
install -d -m 0755 -o pi -g pi "$STATE_DIR"

# Consume the on-demand trigger BEFORE anything that can fail. hall-clock-update.path
# re-fires for as long as the file exists, so a run that aborts with the trigger
# still in place (a malformed update.env would do it) gets restarted immediately,
# aborts again, and pins the box in a loop. The check-only run must leave the
# trigger alone, or the nightly check would swallow a request made moments earlier.
if [ "$MODE" = install ]; then
  rm -f "$TRIGGER"
fi

# One run at a time. The nightly check and an operator's install are separate
# units with no ordering between them; without this they interleave and stomp on
# each other's status file, and two installs could race the same binary.
exec 9>"$LOCK"
if ! flock -n 9; then
  log "another update run is in progress; nothing to do"
  exit 0
fi

# Operators can pin a fork or a different repo without editing this script.
# update.env feeds this root process (REPO alone decides whose binary root
# installs), so refuse it unless root owns it and only root can write it — the
# config directory is owned by the unprivileged app user, who must not be able
# to smuggle settings into a root run by replacing the file.
if [ -f "$ENV_FILE" ]; then
  if [ -n "$(find "$ENV_FILE" -maxdepth 0 -uid 0 ! -perm /022 2>/dev/null)" ]; then
    # shellcheck source=/dev/null
    . "$ENV_FILE"
  else
    log "ignoring ${ENV_FILE}: it must be owned by root and writable only by root"
  fi
fi

current="$("$BIN" -version 2>/dev/null || echo unknown)"
latest=""

# write_status publishes progress for the setup page to read back. Written by
# rename so the app never reads a half-written file. Kept world-readable: the
# app runs as pi and this script as root.
write_status() {
  phase="$1"
  message="${2:-}"
  tmp="${STATUS}.tmp"
  printf '{"phase":"%s","message":"%s","version":"%s","latest":"%s","at":"%s"}\n' \
    "$phase" "$message" "$current" "$latest" "$(date --iso-8601=seconds)" >"$tmp"
  chmod 0644 "$tmp"
  mv -f "$tmp" "$STATUS"
}

fail() {
  log "$1"
  write_status failed "$1"
  exit 1
}

# app_status prints the timer's status ("idle", "running", "paused"), or nothing
# when the app is unreachable. /api/state is unauthenticated but reachable only
# through the Unix socket, so this needs no pairing token.
#
# Trailing `|| true` on both fetches: under `set -o pipefail` a failed curl would
# otherwise abort the script from inside a `$(...)`, instead of leaving the empty
# output their callers are written to handle.
app_status() {
  curl -fsS --max-time 5 --unix-socket "$SOCKET" http://localhost/api/state 2>/dev/null |
    sed -n 's/.*"status":"\([^"]*\)".*/\1/p' || true
}

asset_for_arch() {
  case "$(uname -m)" in
    aarch64 | arm64) echo "hall-clock-linux-arm64" ;;
    armv7l) echo "hall-clock-linux-armv7" ;;
    # Pi Zero / Pi 1. The armv7 binary uses instructions this CPU does not have
    # and dies with SIGILL, so these need their own GOARM=6 build.
    armv6l) echo "hall-clock-linux-armv6" ;;
    *) return 1 ;;
  esac
}

latest_tag() {
  # -L: a renamed repo answers with a redirect whose body has no tag_name.
  curl -fsSL --max-time 20 "https://api.github.com/repos/${REPO}/releases/latest" 2>/dev/null |
    sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' |
    head -1 || true
}

# Only an install has phases worth watching. A check that announced "checking"
# would overwrite a recorded "v1.2.3 available" and, if the network were down,
# leave it stuck there.
if [ "$MODE" = install ]; then
  write_status checking
fi

asset="$(asset_for_arch)" || fail "unsupported architecture $(uname -m)"

latest="$(latest_tag)"
if [ -z "$latest" ]; then
  # A nightly check on a hall with flaky Wi-Fi is not a failure worth recording:
  # it must not clobber a known-available release, nor park the unit in systemd's
  # failed state every time the connection blips. The app does its own live
  # check, so the setup page still reports the outage.
  if [ "$MODE" = check ]; then
    log "could not reach GitHub; leaving the last known status in place"
    exit 0
  fi
  fail "could not reach GitHub to check for updates"
fi

if [ "$current" = "$latest" ]; then
  log "already on ${current}"
  write_status up-to-date "Up to date"
  exit 0
fi

# Check-only: record that a release is waiting and stop. Installing is a restart,
# and a restart is something a person asks for, not something that happens to a
# hall at 4 AM while nobody is watching.
if [ "$MODE" = check ]; then
  log "${latest} available (running ${current}); not installing"
  write_status available "${latest} available"
  exit 0
fi

# A restart rebuilds state from config with the timer reset to idle, so updating
# mid-meeting would blank a running countdown on the projector. The setup page's
# Update button is disabled while a meeting runs; this guards the ssh path and
# any race between the tap and the meeting starting.
status="$(app_status)"
if [ -n "$status" ] && [ "$status" != "idle" ]; then
  log "meeting in progress (status ${status}); refusing to update to ${latest}"
  write_status deferred "Meeting in progress; reset the timer and try again"
  exit 0
fi

log "updating ${current} -> ${latest}"
write_status downloading "Downloading ${latest}"

# Stage inside APP_DIR so the install is a same-filesystem rename, which is
# atomic: no window where the binary is half-written.
staging="$(mktemp -d "${APP_DIR}/.update.XXXXXX")"
trap 'rm -rf "$staging"' EXIT
cd "$staging"

base="https://github.com/${REPO}/releases/download/${latest}"
# Generous deadlines: hall Wi-Fi can be slow, and a link that needs ten minutes
# for a ~15 MB binary should still complete rather than never updating at all.
curl -fsSL --max-time 600 -o "$asset" "${base}/${asset}" || fail "download failed"
curl -fsSL --max-time 600 -o "$DEPLOY_ASSET" "${base}/${DEPLOY_ASSET}" || fail "deploy bundle download failed"
curl -fsSL --max-time 30 -o SHA256SUMS "${base}/SHA256SUMS" || fail "download failed"

# Anything that can tamper with the download owns a box sitting in a public
# hall, so refuse to install a binary whose digest is not the published one.
if ! grep " ${asset}\$" SHA256SUMS | sha256sum -c - >/dev/null 2>&1; then
  fail "checksum mismatch for ${asset}"
fi
if ! grep " ${DEPLOY_ASSET}\$" SHA256SUMS | sha256sum -c - >/dev/null 2>&1; then
  fail "checksum mismatch for ${DEPLOY_ASSET}"
fi

mkdir deploy
tar -xzf "$DEPLOY_ASSET" -C deploy

chmod 0755 "$asset"
chown root:root "$asset"

# Keep copies of the units and Caddyfile about to be replaced: a rollback that
# restores only the old binary under incompatible new units leaves the box just
# as dead as the failed update did.
prev_deploy="${staging}/previous-deploy"
mkdir -p "$prev_deploy"
for f in hall-clock.service hall-clock-kiosk.service hall-clock-update.service \
  hall-clock-update-check.service hall-clock-update.timer hall-clock-update.path \
  hall-clock-housekeeping.service hall-clock-housekeeping.timer; do
  if [ -f "$UNIT_DIR/$f" ]; then
    cp -p "$UNIT_DIR/$f" "$prev_deploy/$f"
  fi
done
if [ -f "$CADDY_DIR/Caddyfile" ]; then
  cp -p "$CADDY_DIR/Caddyfile" "$prev_deploy/Caddyfile"
fi

cp -p "$BIN" "$PREVIOUS"
# Flush the staged binary before renaming it into place: on an SD card the
# rename can land before the data does, and a power cut then leaves a valid
# name pointing at zeroes — a box that can never start its own app again.
sync "$asset"
mv -f "$asset" "$BIN"
sync -f "$BIN"

install -m 0644 deploy/hall-clock.service "$UNIT_DIR/hall-clock.service"
install -m 0644 deploy/hall-clock-kiosk.service "$UNIT_DIR/hall-clock-kiosk.service"
install -m 0644 deploy/hall-clock-update.service "$UNIT_DIR/hall-clock-update.service"
install -m 0644 deploy/hall-clock-update-check.service "$UNIT_DIR/hall-clock-update-check.service"
install -m 0644 deploy/hall-clock-update.timer "$UNIT_DIR/hall-clock-update.timer"
install -m 0644 deploy/hall-clock-update.path "$UNIT_DIR/hall-clock-update.path"
install -m 0644 deploy/hall-clock-housekeeping.service "$UNIT_DIR/hall-clock-housekeeping.service"
install -m 0644 deploy/hall-clock-housekeeping.timer "$UNIT_DIR/hall-clock-housekeeping.timer"
install -m 0755 deploy/hall-clock-kiosk.sh "$APP_DIR/hall-clock-kiosk.sh"
install -m 0755 deploy/hall-clock-update.sh "$APP_DIR/hall-clock-update.sh"
install -m 0755 deploy/hall-clock-housekeeping.sh "$APP_DIR/hall-clock-housekeeping.sh"
if [ -d "$CADDY_DIR" ]; then
  install -m 0644 deploy/Caddyfile "$CADDY_DIR/Caddyfile"
fi

# Heal boxes installed before the root-boundary fix, where install.sh chowned
# this whole directory to pi. Everything here was just replaced from the
# checksummed bundle, so taking root ownership now is safe. update.env is NOT
# auto-chowned: root-blessing a file the app user could have rewritten would
# defeat the guard at the top — an operator who pins a fork re-chowns it to
# root deliberately after checking its contents.
chown -R root:root "$APP_DIR"

# The kiosk script hides the X pointer with unclutter when it is present, but an
# existing box has never run install.sh again, so nothing else would ever put it
# there. Best-effort and never fatal: an update must not fail over a cosmetic
# package, and a box with no apt mirror reachable still gets the new binary.
if ! command -v unclutter >/dev/null 2>&1; then
  log "installing unclutter to hide the pointer on the display"
  DEBIAN_FRONTEND=noninteractive apt-get install -y unclutter >/dev/null 2>&1 ||
    log "could not install unclutter; the pointer may show on the display"
fi

systemctl daemon-reload
systemctl enable hall-clock-update.timer hall-clock-update.path hall-clock-housekeeping.timer >/dev/null
systemctl restart hall-clock-update.timer hall-clock-update.path hall-clock-housekeeping.timer
systemctl try-restart caddy.service >/dev/null 2>&1 || true

rollback() {
  log "rolling back to ${current}"
  mv -f "$PREVIOUS" "$BIN"
  sync -f "$BIN" || true
  # Restore the units and Caddyfile that were live before this update: the old
  # binary must come back up under the configuration it was proven with.
  for f in "$prev_deploy"/*; do
    if [ -f "$f" ]; then
      case "$(basename "$f")" in
        Caddyfile)
          install -m 0644 "$f" "$CADDY_DIR/Caddyfile"
          systemctl try-restart caddy.service >/dev/null 2>&1 || true
          ;;
        *)
          install -m 0644 "$f" "$UNIT_DIR/$(basename "$f")"
          ;;
      esac
    fi
  done
  systemctl daemon-reload || true
  # Never let a failing restart abort the script here: `set -e` would kill it
  # before fail() records why the update was rolled back, leaving the setup page
  # stuck on "Restarting..." forever.
  systemctl restart "$SERVICE" || log "rollback restart failed; ${SERVICE} needs attention"
}

write_status restarting "Restarting on ${latest}"
if ! systemctl restart "$SERVICE"; then
  rollback
  fail "update to ${latest} failed to restart; rolled back"
fi

# The unit restarts on failure, so "active" alone does not mean the new binary
# works. Wait for it to answer on the socket before calling the update good.
for _ in $(seq 1 15); do
  if [ -n "$(app_status)" ]; then
    log "updated to ${latest}"
    current="$latest"
    write_status updated "Updated to ${latest}"
    systemctl try-restart hall-clock-kiosk.service >/dev/null 2>&1 || true
    exit 0
  fi
  sleep 1
done

rollback
fail "update to ${latest} did not become healthy; rolled back"
