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
# existing configuration is left alone except for the listen address, which
# is rewritten when -l is given. That makes the same command both the
# upgrade path and the way to move the panel onto another address.
#
# Options:
#   -b, --binary PATH     the eye-of-lotus binary to install (required)
#   -l, --listen ADDR     address to bind (default 127.0.0.1:9110). Use
#                         0.0.0.0:9110 to serve the whole local network.
#       --no-docker-group leave the service account out of the docker
#                         group, at the cost of container visibility
#       --no-firewall     do not open the firewall
#       --uninstall       remove everything this script installed: the
#                         service, the binary, the configuration, the
#                         history and the service account
#       --dry-run         show what would be done, change nothing
#   -h, --help            this message
#
# Environment:
#   EOL_PASSWORD          set the password without a prompt (for automation)
#
set -euo pipefail

BIN_SRC=""
LISTEN="127.0.0.1:9110"
LISTEN_SET=0
UNINSTALL=0
DRY_RUN=0
ADD_DOCKER_GROUP=1
FIREWALL=1

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

# has_listener reports whether anything is listening on a TCP port.
#
# It captures the listing instead of piping into `grep -q`, and that is not
# a stylistic choice. grep -q exits at the first match, the writer dies of
# SIGPIPE, and with `set -o pipefail` the whole pipeline reports failure, so
# a test written that way quietly answers "nothing there" exactly when the
# match succeeded. That mistake skipped stopping a running service once.
has_listener() {
	local listing
	listing="$(ss -ltn 2>/dev/null || true)"
	case "$listing" in
	*":$1 "*) return 0 ;;
	esac
	return 1
}

# default_iface prints the interface that carries the default route. It is
# the one a firewall rule has to name in order to reach the panel from the
# local network, and going through the route table avoids guessing names
# like eth0 or enp0s3.
default_iface() {
	local routes
	routes="$(ip -4 route show default 2>/dev/null || true)"
	printf '%s\n' "$routes" | awk '
		{ for (i = 1; i <= NF; i++) if ($i == "dev") { print $(i + 1); exit } }'
}

# pid_of_listener prints the process id owning a listening TCP port, and
# nothing when the port is free or belongs to another user.
pid_of_listener() {
	ss -ltnp 2>/dev/null | awk -v needle=":$1 " '
		index($0, needle) {
			if (match($0, /pid=[0-9]+/)) {
				print substr($0, RSTART + 4, RLENGTH - 4)
				exit
			}
		}'
}

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
		LISTEN_SET=1
		shift 2
		;;
	--no-docker-group)
		ADD_DOCKER_GROUP=0
		shift
		;;
	--no-firewall)
		FIREWALL=0
		shift
		;;
	--uninstall)
		UNINSTALL=1
		shift
		;;
	--dry-run)
		DRY_RUN=1
		shift
		;;
	-h | --help) usage ;;
	*) die "unknown option: $1 (try --help)" ;;
	esac
done

# Removing needs no binary and no unit file, so those requirements only
# apply to an install.
if [ "$UNINSTALL" -eq 0 ]; then
	[ -n "$BIN_SRC" ] || die "no binary given; use -b PATH (try --help)"
	[ -f "$BIN_SRC" ] || die "no such file: $BIN_SRC"
	[ -f "$UNIT_SRC" ] || die "cannot find eye-of-lotus.service next to this script (looked in ${HERE})"
fi

if [ "$DRY_RUN" -eq 0 ]; then
	[ "$(id -u)" -eq 0 ] || die "run this as root (sudo $0 ...)"
	command -v systemctl >/dev/null 2>&1 ||
		die "systemctl not found: this installer is for systemd hosts; see the README for the manual steps"
fi

if [ "$UNINSTALL" -eq 1 ]; then
	[ "$DRY_RUN" -eq 0 ] || die "--uninstall and --dry-run together make no sense"
	step "Removing Eye of Lotus"
	# Only what this installer created is touched. The docker group, the
	# firewall and anything else on the host are left exactly as they are,
	# because this script cannot know who else depends on them.
	#
	# The service is stopped before its unit file is removed. Once the file
	# is gone systemd no longer knows how to stop the unit, while the
	# process itself keeps running and holding the port.
	#
	# The unit file is what is checked, not systemctl's output: a test that
	# piped into grep -q once answered "no such unit" while the unit was
	# running, and skipped this whole block.
	if [ -f "$UNIT_DST" ]; then
		systemctl disable "$SERVICE" >/dev/null 2>&1 || true
		systemctl stop "$SERVICE" >/dev/null 2>&1 || true
		say "service stopped and disabled"
	fi

	# A panel started by an earlier install, or by hand, outlives the
	# removal of its unit and keeps the port. It also holds the service
	# account open, which is what makes the user removal below fail, so it
	# is dealt with first.
	port="${LISTEN##*:}"
	if has_listener "$port"; then
		pid="$(pid_of_listener "$port")"
		say "something is still listening on port ${port}"
		if [ -n "$pid" ] && kill "$pid" 2>/dev/null; then
			sleep 1
			say "stopped process ${pid}, left over from an earlier install"
		fi
		if has_listener "$port"; then
			say "port ${port} is still busy; free it before installing again"
		else
			say "port ${port} is free now"
		fi
	fi

	rm -f "$UNIT_DST"
	systemctl daemon-reload >/dev/null 2>&1 || true
	say "removed ${UNIT_DST}"
	rm -f "$BIN_DST"
	say "removed ${BIN_DST}"
	rm -rf "$CONF_DIR"
	say "removed ${CONF_DIR} (the password lived here)"
	rm -rf "$DATA_DIR"
	say "removed ${DATA_DIR} (the metric history lived here)"

	# The account is only reported as gone when it really is. userdel
	# refuses while a process still runs as that user, and announcing
	# success anyway is how an uninstall ends up half done without saying so.
	if id -u "$RUN_USER" >/dev/null 2>&1; then
		if userdel "$RUN_USER" 2>/dev/null; then
			say "removed user ${RUN_USER}"
		else
			say "could not remove user ${RUN_USER}: something is still running as it"
		fi
	else
		say "user ${RUN_USER} was already gone"
	fi
	if getent group "$RUN_USER" >/dev/null 2>&1; then
		if groupdel "$RUN_USER" 2>/dev/null; then
			say "removed group ${RUN_USER}"
		else
			say "could not remove group ${RUN_USER}: it is still in use"
		fi
	else
		say "group ${RUN_USER} was already gone"
	fi

	printf '\n'
	say "The firewall was not touched, so any rule opened for the panel is"
	say "still there. Check with: ufw status"
	printf '\n'
	exit 0
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

step "Container visibility"
# The Docker API socket is owned by root:docker, and reading it needs group
# membership. Without this the panel runs perfectly well and reports the
# container section as unreachable, which reads like a fault rather than a
# permission that was never granted.
if ! getent group docker >/dev/null 2>&1; then
	say "no docker group on this host; nothing to do"
else
	user_groups="$(id -nG "$RUN_USER" 2>/dev/null || true)"
	case " ${user_groups} " in
	*" docker "*)
		say "${RUN_USER} is already in the docker group"
		;;
	*)
		if [ "$ADD_DOCKER_GROUP" -eq 0 ]; then
			say "skipped by request: the panel will report Docker as unreachable"
		else
			usermod -aG docker "$RUN_USER"
			say "added ${RUN_USER} to the docker group"
			say "note: membership of docker is root access to this host in practice."
			say "      The panel only issues read requests, but the account could do"
			say "      more. Pass --no-docker-group to leave it out."
		fi
		;;
	esac
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
	say "${CONF} exists; password and history are left untouched"
	if [ "$LISTEN_SET" -eq 1 ]; then
		current="$(sed -n 's/.*"listen"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$CONF" | head -1)"
		if [ -z "$current" ]; then
			say "no listen key found in ${CONF}; set it by hand"
		elif [ "$current" = "$LISTEN" ]; then
			say "listen is already ${LISTEN}"
		else
			cp -a "$CONF" "${CONF}.bak"
			sed -i "s|\"listen\"[[:space:]]*:[[:space:]]*\"[^\"]*\"|\"listen\": \"${LISTEN}\"|" "$CONF"
			say "listen changed from ${current} to ${LISTEN} (old file kept as ${CONF}.bak)"
		fi
	else
		say "pass -l ADDR to change the address it listens on"
	fi
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
#
# The panel's own service holding the port is not that case. It is what an
# upgrade always finds, because the service is still running on the binary
# that is being replaced, so it is stopped here rather than treated as a
# conflict. Anything else on the port still stops the install.
PORT="${LISTEN##*:}"
if has_listener "$PORT" && systemctl is-active --quiet "$SERVICE" 2>/dev/null; then
	say "the running panel holds port ${PORT}; stopping it for the upgrade"
	systemctl stop "$SERVICE" >/dev/null 2>&1 || true
	sleep 1
fi
if command -v ss >/dev/null 2>&1 && has_listener "$PORT"; then
	printf '\n' >&2
	ss -ltnp 2>/dev/null | awk -v n=":${PORT} " 'index($0, n)' >&2 || true
	printf '\n' >&2
	die "something else is already listening on port ${PORT}; stop it and run this again"
fi
say "port ${PORT} is free"

step "Firewall"
# Opening a port is a security decision, so it is scoped to the interface
# the local network arrives on and, separately, to the Docker bridge
# networks. The second one is not obvious: a reverse proxy running in a
# container reaches the panel from its bridge address, not from the local
# network, and when the firewall drops that silently the proxy waits and
# the browser spins. Neither rule opens the port to the internet.
if [ "$FIREWALL" -eq 0 ]; then
	say "skipped by request; open port ${PORT} yourself if you need it"
elif ! command -v ufw >/dev/null 2>&1; then
	say "ufw is not installed, so nothing was opened"
	say "if another firewall is active, allow TCP ${PORT} yourself"
else
	ufw_state="$(ufw status 2>/dev/null || true)"
	case "$ufw_state" in
	*"Status: active"*) ;;
	*) ufw_state="" ;;
	esac
	if [ -z "$ufw_state" ]; then
		say "ufw is installed but not active; nothing to open"
	else
		case "$LISTEN" in
		127.0.0.1:* | localhost:* | "[::1]:"*)
			say "the panel is bound to the loopback address, so no rule is needed"
			;;
		*)
			iface="$(default_iface)"
			if [ -n "$iface" ]; then
				if ufw allow in on "$iface" to any port "$PORT" proto tcp >/dev/null 2>&1; then
					say "allowed the local network on ${iface} to reach port ${PORT}"
				else
					say "could not add the rule for ${iface}; open port ${PORT} yourself"
				fi
			else
				say "could not work out the local interface; open port ${PORT} yourself"
			fi
			if getent group docker >/dev/null 2>&1; then
				if ufw allow from 172.16.0.0/12 to any port "$PORT" proto tcp >/dev/null 2>&1; then
					say "allowed container networks to reach port ${PORT} (a proxy in Docker needs this)"
				fi
			fi
			;;
		esac
	fi
fi

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

case "$LISTEN" in
127.0.0.1:* | localhost:* | "[::1]:"*)
	say "The panel is bound to the loopback address, so only this machine"
	say "can reach it. Either tunnel in:"
	say "  ssh -L 9110:127.0.0.1:9110 ${SUDO_USER:-$(whoami)}@this-host"
	say "or serve the local network instead, by running this installer again"
	say "with -l 0.0.0.0:${LISTEN##*:}"
	;;
*)
	host="$(hostname -I 2>/dev/null | awk '{print $1}')"
	say "The panel is reachable from the network at:"
	if [ -n "$host" ]; then
		say "  http://${host}:${LISTEN##*:}"
	fi
	say "Anyone who can reach that port sees a login prompt, and the login is"
	say "throttled, but the session cookie travels in the clear over plain"
	say "HTTP. Keep it inside a network you trust, or put a reverse proxy"
	say "with a certificate in front of it and set trust_proxy to true."
	;;
esac
printf '\n'
