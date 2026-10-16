#!/usr/bin/env bash
#
# Eye of Lotus installer for a systemd host.
#
# Run it from the deploy directory, so it can find the unit file next to
# itself, and point it at the binary you built or downloaded:
#
#   sudo ./install.sh -b ./eye-of-lotus
#
# It is safe to run again: the binary and the unit are replaced, an
# existing configuration is left alone. That makes the same command the
# upgrade path.
#
# Options:
#   -b, --binary PATH   the eye-of-lotus binary to install (required)
#   -l, --listen ADDR   address to bind (default 127.0.0.1:9110)
#       --dry-run       show what would be done, change nothing
#   -h, --help          this message
#
# Environment:
#   EOL_PASSWORD        set the password without a prompt (for automation)
#
set -euo pipefail

BIN_SRC=""
LISTEN="127.0.0.1:9110"
DRY_RUN=0

BIN_DST="/usr/local/bin/eye-of-lotus"
CONF_DIR="/etc/eye-of-lotus"
CONF="${CONF_DIR}/config.json"
DATA_DIR="/var/lib/eye-of-lotus"
UNIT_DST="/etc/systemd/system/eye-of-lotus.service"
SERVICE="eye-of-lotus"
RUN_USER="eye-of-lotus"

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
UNIT_SRC="${HERE}/eye-of-lotus.service"

die() { printf 'install: %s\n' "$*" >&2; exit 1; }
step() { printf '\n==> %s\n' "$*"; }
say() { printf '    %s\n' "$*"; }

usage() {
	# Everything after the shebang that is still a comment is the help
	# text, so the header can be edited without touching a line range.
	awk 'NR > 1 { if ($0 !~ /^#/) exit; sub(/^# ?/, ""); print }' "${BASH_SOURCE[0]}"
	exit 0
}

while [ $# -gt 0 ]; do
	case "$1" in
	-b | --binary)
		BIN_SRC="${2:-}"
		shift 2
		;;
	-l | --listen)
		LISTEN="${2:-}"
		shift 2
		;;
	--dry-run)
		DRY_RUN=1
		shift
		;;
	-h | --help) usage ;;
	*) die "unknown option: $1 (try --help)" ;;
	esac
done

[ -n "$BIN_SRC" ] || die "no binary given; use -b PATH (try --help)"
[ -f "$BIN_SRC" ] || die "no such file: $BIN_SRC"
[ -f "$UNIT_SRC" ] || die "cannot find eye-of-lotus.service next to this script (looked in ${HERE})"

if [ "$DRY_RUN" -eq 0 ]; then
	[ "$(id -u)" -eq 0 ] || die "run this as root (sudo $0 ...)"
	command -v systemctl >/dev/null 2>&1 ||
		die "systemctl not found: this installer is for systemd hosts; see the README for the manual steps"
fi

# A binary that cannot run is worse than no install, so ask it who it is.
# A file copied with scp arrives without the execute bit, and that is not a
# reason to refuse an otherwise good binary, so the bit is set here.
chmod 0755 "$BIN_SRC" 2>/dev/null || true
if ! "$BIN_SRC" -version >/dev/null 2>&1; then
	die "$BIN_SRC does not run here; check that it is built for this architecture (GOARCH) and is executable"
fi
VERSION="$("$BIN_SRC" -version | awk '{print $2}')"

step "Eye of Lotus ${VERSION}"
say "binary  ${BIN_SRC} -> ${BIN_DST}"
say "config  ${CONF}"
say "data    ${DATA_DIR}"
say "listen  ${LISTEN}"

if [ "$DRY_RUN" -eq 1 ]; then
	step "Dry run: nothing was changed"
	exit 0
fi

step "Service account"
# The group is created explicitly: the unit says Group=eye-of-lotus, and
# useradd does not always make a matching group on its own.
if getent group "$RUN_USER" >/dev/null 2>&1; then
	say "group ${RUN_USER} already exists"
else
	groupadd --system "$RUN_USER"
	say "created group ${RUN_USER}"
fi
if id -u "$RUN_USER" >/dev/null 2>&1; then
	say "user ${RUN_USER} already exists"
else
	useradd --system --gid "$RUN_USER" --home-dir "$DATA_DIR" \
		--shell /usr/sbin/nologin "$RUN_USER" ||
		useradd --system --gid "$RUN_USER" --home-dir "$DATA_DIR" \
			--shell /sbin/nologin "$RUN_USER"
	say "created user ${RUN_USER}"
fi

step "Directories"
install -d -m 0755 -o "$RUN_USER" -g "$RUN_USER" "$CONF_DIR"
install -d -m 0750 -o "$RUN_USER" -g "$RUN_USER" "$DATA_DIR"
say "ready"

step "Binary"
# Installed through a temporary file so that a half written binary can
# never be left behind for systemd to start.
install -m 0755 -o root -g root "$BIN_SRC" "${BIN_DST}.new"
mv -f "${BIN_DST}.new" "$BIN_DST"
say "installed ${BIN_DST}"

step "Configuration"
if [ -f "$CONF" ]; then
	say "${CONF} exists; left untouched"
	say "delete it and run again if you want to set a new password"
else
	if [ -n "${EOL_PASSWORD:-}" ]; then
		password="$EOL_PASSWORD"
	else
		[ -t 0 ] || die "no terminal to prompt on; set EOL_PASSWORD instead"
		read -r -s -p "    Password for the panel: " password
		printf '\n'
		read -r -s -p "    Repeat it: " again
		printf '\n'
		[ "$password" = "$again" ] || die "the passwords do not match"
	fi
	[ -n "$password" ] || die "an empty password is not allowed"

	hash="$("$BIN_DST" -hash-password "$password" 2>/dev/null)"
	unset password again
	[ -n "$hash" ] || die "could not hash the password"

	# The session secret is generated here rather than by the service,
	# because the unit mounts /etc read-only and the service could not
	# write it back.
	secret="$(head -c 32 /dev/urandom | base64 | tr -d '=\n' | tr '/+' '_-')"

	umask 077
	cat >"$CONF" <<JSON
{
  "listen": "$LISTEN",
  "password": "$hash",
  "session_secret": "$secret",
  "database": "$DATA_DIR/history.db",
  "systemd": true,
  "systemd_detail": true,
  "docker": true,
  "log_level": "info"
}
JSON
	chmod 0600 "$CONF"
	chown "$RUN_USER:$RUN_USER" "$CONF"
	say "wrote ${CONF}"
fi

step "Checking the port"
# A process already holding the address looks exactly like a broken
# service from the outside: the unit starts, exits with status 1, and
# restarts in a loop. Catching it here turns a puzzling failure into a
# sentence.
PORT="${LISTEN##*:}"
if command -v ss >/dev/null 2>&1 && ss -ltn 2>/dev/null | grep -q ":${PORT} "; then
	printf '\n' >&2
	ss -ltnp 2>/dev/null | grep ":${PORT} " >&2 || true
	printf '\n' >&2
	die "something is already listening on port ${PORT}; stop it and run this again (it may be a panel you started by hand earlier)"
fi
say "port ${PORT} is free"

step "systemd unit"
install -m 0644 -o root -g root "$UNIT_SRC" "$UNIT_DST"
systemctl daemon-reload
say "installed ${UNIT_DST}"

step "Starting"
systemctl enable --now "$SERVICE" >/dev/null
sleep 2
if systemctl is-active --quiet "$SERVICE"; then
	say "the service is running"
else
	printf '\n' >&2
	# The journal is the only place the reason appears, and reading it
	# needs root, which is why this runs while the installer still has it.
	journalctl -u "$SERVICE" --no-pager --lines=30 >&2 || true
	printf '\n' >&2
	systemctl --no-pager --lines=20 status "$SERVICE" >&2 || true
	die "the service failed to start; the output above says why"
fi

step "Done"
say "panel:   http://${LISTEN}"
say "logs:    journalctl -u ${SERVICE} -f"
say "restart: systemctl restart ${SERVICE}"
printf '\n'
say "If the panel is bound to 127.0.0.1 as above, reach it with an SSH"
say "tunnel:  ssh -L 9110:127.0.0.1:9110 ${SUDO_USER:-$(whoami)}@this-host"
printf '\n'
