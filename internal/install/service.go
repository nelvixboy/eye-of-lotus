package install

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// ServiceManager is how this host starts a service at boot.
type ServiceManager int

const (
	KindNone ServiceManager = iota
	KindSystemd
	KindOpenRC
)

// ServiceManagerName is the human name, for messages.
func ServiceManagerName(k ServiceManager) string {
	switch k {
	case KindSystemd:
		return "systemd"
	case KindOpenRC:
		return "OpenRC"
	}
	return "none"
}

// DetectServiceManager works out how this host starts a service.
//
// systemd is identified by its runtime directory rather than by the presence
// of the systemctl binary. A machine can carry the client while another init
// system is in charge, which is exactly the case on Artix and Devuan, where
// OpenRC runs the show and systemctl is only a wrapper that fails.
func DetectServiceManager(lookPath func(string) (string, error)) ServiceManager {
	if _, err := os.Stat("/run/systemd/system"); err == nil {
		if _, err := lookPath("systemctl"); err == nil {
			return KindSystemd
		}
	}
	if _, err := lookPath("rc-service"); err == nil {
		return KindOpenRC
	}
	return KindNone
}

// systemdUnit is the service definition.
//
// It lives in the code rather than in a file beside it on purpose. A unit
// shipped next to the installer is a second copy of every path and every
// sandbox setting in it, and the two drift: the version of this file that
// existed as a separate unit had already been edited by hand twice.
func systemdUnit() string {
	return `[Unit]
Description=Eye of Lotus server monitor
Documentation=https://github.com/nelvixboy/eye-of-lotus
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=` + RunUser + `
Group=` + RunUser + `

ExecStart=` + BinaryPath + ` -config ` + ConfigPath + `

# The panel writes exactly one file: its history database. Everything else
# it touches is read-only.
ReadWritePaths=` + DataDir + `
ProtectSystem=strict
ProtectHome=read-only
PrivateTmp=true
PrivateDevices=false
NoNewPrivileges=true
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectControlGroups=true
ProtectClock=true
ProtectHostname=true
# ProtectProc is deliberately left at its default: the panel walks
# /proc/<pid>/fd to name the process behind each listening socket, and
# hiding other processes' entries would remove that.
RestrictNamespaces=true
RestrictRealtime=true
RestrictSUIDSGID=true
LockPersonality=true
MemoryDenyWriteExecute=true
SystemCallArchitectures=native
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX
CapabilityBoundingSet=
AmbientCapabilities=
# Reading every process's /proc/<pid>/fd is how the panel names the process
# behind a listening socket. Without it the port is still listed, with no
# owner beside it.
ProcSubset=all

# Restart on failure, but not in a tight loop.
Restart=on-failure
RestartSec=5s

# Logging goes to the journal.
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
`
}

// openRCScript is the same thing for OpenRC, which is what Artix, Alpine,
// Gentoo and Devuan use.
func openRCScript() string {
	return `#!/sbin/openrc-run
# Eye of Lotus server monitor.

name="eye of lotus"
description="Eye of Lotus server monitor"

command="` + BinaryPath + `"
command_args="-config ` + ConfigPath + `"
command_user="` + RunUser + `:` + RunUser + `"
command_background="yes"
pidfile="/run/` + ServiceName + `.pid"

output_log="` + LogPath + `"
error_log="` + LogPath + `"

depend() {
	need net
	use dns logger
}
`
}

// writeService replaces the service definition for this host's manager.
func writeService(opts *Options, kind ServiceManager) error {
	opts.step("Service definition")
	switch kind {
	case KindSystemd:
		if err := os.WriteFile(SystemdUnit, []byte(systemdUnit()), 0o644); err != nil {
			return fmt.Errorf("cannot write %s: %w", SystemdUnit, err)
		}
		opts.say("wrote %s", SystemdUnit)
		opts.Run("systemctl", "daemon-reload")
	case KindOpenRC:
		if err := os.WriteFile(OpenRCScript, []byte(openRCScript()), 0o755); err != nil {
			return fmt.Errorf("cannot write %s: %w", OpenRCScript, err)
		}
		opts.say("wrote %s", OpenRCScript)
	default:
		return errors.New("no service manager to write a definition for")
	}
	return nil
}

// serviceActive reports whether our service is running right now.
func serviceActive(opts *Options, kind ServiceManager) bool {
	switch kind {
	case KindSystemd:
		return opts.Run("systemctl", "is-active", "--quiet", ServiceName) == nil
	case KindOpenRC:
		return opts.Run("rc-service", ServiceName, "status") == nil
	}
	return false
}

// stopService stops and disables it, ignoring the failures that mean it was
// not running or not enabled in the first place.
func stopService(opts *Options, kind ServiceManager) error {
	switch kind {
	case KindSystemd:
		if _, err := os.Stat(SystemdUnit); err != nil {
			return nil
		}
		opts.Run("systemctl", "disable", ServiceName)
		if opts.Run("systemctl", "stop", ServiceName) == nil {
			opts.say("service stopped and disabled")
		}
	case KindOpenRC:
		if _, err := os.Stat(OpenRCScript); err != nil {
			return nil
		}
		opts.Run("rc-update", "del", ServiceName)
		if opts.Run("rc-service", ServiceName, "stop") == nil {
			opts.say("service stopped and disabled")
		}
	}
	return nil
}

// stopOwnService makes the port free, and refuses only when something that
// is not ours holds it.
//
// An upgrade always finds the port busy, because that is what an upgrade is:
// replacing the binary that the running process was started from. Stopping
// our own service is therefore part of installing. A stranger on the port is
// an error, because starting a second panel on it would fail anyway, in a
// restart loop whose reason lives in a log the operator cannot read.
func stopOwnService(opts *Options, kind ServiceManager) error {
	opts.step("Checking the port")
	port := portOf(opts.listen())
	if !anyoneListening(opts.Output, port) {
		opts.say("port %s is free", port)
		return nil
	}
	if serviceActive(opts, kind) {
		opts.say("the running panel holds port %s; stopping it for the upgrade", port)
		if kind == KindSystemd {
			opts.Run("systemctl", "stop", ServiceName)
		} else {
			opts.Run("rc-service", ServiceName, "stop")
		}
		time.Sleep(time.Second)
	}
	if anyoneListening(opts.Output, port) {
		if pid := listenerPID(opts.Output, port); pid > 0 {
			fmt.Fprintf(opts.Err, "  process %d is holding port %s\n", pid, port)
		}
		return fmt.Errorf("something else is already listening on port %s; stop it and run this again", port)
	}
	opts.say("port %s is free", port)
	return nil
}

// startService enables the service and starts it, reporting the log if it
// does not come up.
func startService(opts *Options, kind ServiceManager) error {
	opts.step("Starting")
	switch kind {
	case KindSystemd:
		opts.Run("systemctl", "enable", ServiceName)
		opts.Run("systemctl", "restart", ServiceName)
		time.Sleep(2 * time.Second)
		if opts.Run("systemctl", "is-active", "--quiet", ServiceName) == nil {
			opts.say("the service is running")
			return nil
		}
		// The journal is the only place the reason appears, and reading it
		// needs root, which is exactly what is being held right now.
		if tail, err := opts.Output("journalctl", "-u", ServiceName, "--no-pager", "--lines=30"); err == nil {
			fmt.Fprintln(opts.Err, tail)
		}
		if status, err := opts.Output("systemctl", "--no-pager", "--lines=20", "status", ServiceName); err == nil {
			fmt.Fprintln(opts.Err, status)
		}
		return errors.New("the service failed to start; the output above says why")

	case KindOpenRC:
		opts.Run("rc-update", "add", ServiceName, "default")
		opts.Run("rc-service", ServiceName, "start")
		time.Sleep(2 * time.Second)
		if opts.Run("rc-service", ServiceName, "status") == nil {
			opts.say("the service is running")
			return nil
		}
		if tail, err := opts.Output("tail", "-n", "30", LogPath); err == nil {
			fmt.Fprintln(opts.Err, tail)
		}
		return errors.New("the service failed to start; the output above says why")
	}
	return errors.New("no service manager to start the service with")
}

// anyoneListening asks ss, which comes with iproute2 and is present on every
// system this program runs on.
//
// The listing is captured and matched rather than piped into grep, and that
// is not a style choice. grep exits at the first match, the writer dies of
// SIGPIPE, and a shell version of this check written that way answered "the
// port is free" precisely when it was not.
func anyoneListening(out func(string, ...string) (string, error), port string) bool {
	listing, err := out("ss", "-ltn")
	if err != nil {
		return false
	}
	return strings.Contains(listing, ":"+port+" ")
}

// listenerPID reports which process owns a listening port, or zero.
func listenerPID(out func(string, ...string) (string, error), port string) int {
	listing, err := out("ss", "-ltnp")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(listing, "\n") {
		if !strings.Contains(line, ":"+port+" ") {
			continue
		}
		i := strings.Index(line, "pid=")
		if i < 0 {
			continue
		}
		rest := line[i+len("pid="):]
		if end := strings.IndexAny(rest, ",)"); end >= 0 {
			rest = rest[:end]
		}
		if pid, err := strconv.Atoi(rest); err == nil {
			return pid
		}
	}
	return 0
}
