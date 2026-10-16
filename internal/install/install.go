// Package install puts this program on a host as a service, and takes it
// away again.
//
// It exists so that the program is its own installer. Copying one file to a
// machine and running it with -install is the whole procedure: there is no
// shell script beside it, no unit file that can drift from the code that
// writes it, and no second place where the paths are written down.
//
// Everything this touches is named by the constants below, and everything it
// removes is one of them. It does not know about any other service on the
// host and does not touch one.
package install

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Where things live. These are the only paths the installer creates or
// removes, and uninstall is written as the exact inverse of install.
const (
	ServiceName  = "eye-of-lotus"
	RunUser      = "eye-of-lotus"
	BinaryPath   = "/usr/local/bin/eye-of-lotus"
	ConfigDir    = "/etc/eye-of-lotus"
	ConfigPath   = ConfigDir + "/config.json"
	DataDir      = "/var/lib/eye-of-lotus"
	DatabasePath = DataDir + "/history.db"
	SystemdUnit  = "/etc/systemd/system/" + ServiceName + ".service"
	OpenRCScript = "/etc/init.d/" + ServiceName
	LogPath      = "/var/log/" + ServiceName + ".log"
	RegistryDir  = "/etc/" + ServiceName
)

// DefaultListen is where the panel binds when nothing says otherwise.
const DefaultListen = "127.0.0.1:9110"

// Options carries what the operator asked for on the command line.
type Options struct {
	// Listen is the address to bind. When ListenSet is false an existing
	// configuration is left alone and a new one uses DefaultListen.
	Listen    string
	ListenSet bool

	// Version is what to print in the banner. It comes from the caller
	// because main owns the variable that the linker sets.
	Version string

	// Password is the panel passphrase. Empty means ask on the terminal,
	// unless a configuration already exists, in which case it is kept.
	Password string

	// NoDockerGroup leaves the service account out of the docker group,
	// at the cost of the container section reporting an unreachable
	// daemon.
	NoDockerGroup bool

	// NoFirewall skips opening the port.
	NoFirewall bool

	Out io.Writer
	Err io.Writer

	// Run, Output and LookPath are the outside world, injectable so that
	// the order of operations can be tested without root.
	Run      func(name string, args ...string) error
	Output   func(name string, args ...string) (string, error)
	LookPath func(string) (string, error)

	// Now is injectable for tests.
	Now func() time.Time

	// Prompt asks for a password twice and returns it. Nil means read from
	// the terminal with echoing off.
	Prompt func() (string, error)
}

func (o *Options) listen() string {
	if o.Listen != "" {
		return o.Listen
	}
	return DefaultListen
}

func (o *Options) say(format string, args ...any) {
	fmt.Fprintf(o.Out, "  "+format+"\n", args...)
}

func (o *Options) step(format string, args ...any) {
	fmt.Fprintf(o.Out, "\n==> "+format+"\n", args...)
}

func (o *Options) setDefaults() {
	if o.Out == nil {
		o.Out = os.Stdout
	}
	if o.Err == nil {
		o.Err = os.Stderr
	}
	if o.Run == nil {
		o.Run = runQuiet
	}
	if o.Output == nil {
		o.Output = output
	}
	if o.LookPath == nil {
		o.LookPath = exec.LookPath
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Prompt == nil {
		o.Prompt = promptForPassword
	}
}

// Run does the whole job: account, directories, binary, configuration,
// service definition, firewall and start.
func Run(opts Options) error {
	opts.setDefaults()
	if os.Geteuid() != 0 {
		return errors.New("run this as root, for example: sudo eye-of-lotus -install")
	}
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("cannot find my own path: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(self); err == nil {
		self = resolved
	}

	kind := DetectServiceManager(opts.LookPath)
	if kind == KindNone {
		return errors.New("no service manager found: this installer knows systemd and OpenRC")
	}

	opts.step("Eye of Lotus %s", opts.Version)
	opts.say("binary   %s -> %s", self, BinaryPath)
	opts.say("config   %s", ConfigPath)
	opts.say("data     %s", DataDir)
	opts.say("listen   %s", opts.listen())
	opts.say("service  %s", ServiceManagerName(kind))

	if err := ensureAccount(&opts); err != nil {
		return err
	}
	if err := ensureDirectories(&opts); err != nil {
		return err
	}
	if err := installBinary(&opts, self); err != nil {
		return err
	}
	if err := ensureConfiguration(&opts); err != nil {
		return err
	}
	if err := stopOwnService(&opts, kind); err != nil {
		return err
	}
	if err := writeService(&opts, kind); err != nil {
		return err
	}
	openFirewall(&opts)
	if err := startService(&opts, kind); err != nil {
		return err
	}

	opts.step("Done")
	opts.say("panel    http://%s", opts.listen())
	opts.say("logs     %s", logsHint(kind))
	opts.say("restart  %s", restartHint(kind))
	fmt.Fprintln(opts.Out)
	if isLoopback(opts.listen()) {
		opts.say("Bound to the loopback address, so only this machine can reach it.")
		opts.say("Either tunnel in with ssh -L 9110:127.0.0.1:9110 this-host,")
		opts.say("or run this again with -listen 0.0.0.0:9110 to serve the local network.")
	} else {
		opts.say("Reachable from the network on this machine's address.")
		opts.say("Anyone who can reach that port sees a login prompt and the login is")
		opts.say("throttled, but the session cookie travels in the clear over plain HTTP.")
		opts.say("Keep it inside a network you trust, or put a certificate in front of it.")
	}
	fmt.Fprintln(opts.Out)
	return nil
}

// RunUninstall reverses Run, and nothing else.
func RunUninstall(opts Options) error {
	opts.setDefaults()
	if os.Geteuid() != 0 {
		return errors.New("run this as root, for example: sudo eye-of-lotus -uninstall")
	}
	kind := DetectServiceManager(opts.LookPath)

	opts.step("Removing Eye of Lotus")

	// Stop before the definition is removed. Once it is gone the service
	// manager no longer knows how to stop the unit, while the process
	// keeps running and holding the port.
	if err := stopService(&opts, kind); err != nil {
		return err
	}

	// A panel started by an earlier install, or by hand, outlives its
	// service definition and keeps the port. It also holds the account
	// open, which is what makes removing the account fail, so it goes
	// first and by the port rather than by name.
	if pid := listenerPID(opts.Output, portOf(opts.listen())); pid > 0 {
		opts.say("something is still listening on port %s", portOf(opts.listen()))
		if opts.Run("kill", fmt.Sprint(pid)) == nil {
			time.Sleep(time.Second)
			opts.say("stopped process %d, left over from an earlier install", pid)
		}
	}

	removeFile(&opts, SystemdUnit)
	removeFile(&opts, OpenRCScript)
	removeFile(&opts, BinaryPath)
	opts.Run("systemctl", "daemon-reload")
	removeTree(&opts, ConfigDir)
	removeTree(&opts, DataDir)
	removeTree(&opts, RegistryDir)
	removeAccount(&opts)

	fmt.Fprintln(opts.Out)
	opts.say("The firewall was not touched, so any rule opened for the panel is")
	opts.say("still there. Check with: ufw status")
	fmt.Fprintln(opts.Out)
	return nil
}

func removeFile(opts *Options, path string) {
	if _, err := os.Stat(path); err != nil {
		return
	}
	if err := os.Remove(path); err != nil {
		opts.say("could not remove %s: %v", path, err)
		return
	}
	opts.say("removed %s", path)
}

func removeTree(opts *Options, path string) {
	if _, err := os.Stat(path); err != nil {
		return
	}
	if err := os.RemoveAll(path); err != nil {
		opts.say("could not remove %s: %v", path, err)
		return
	}
	opts.say("removed %s", path)
}

// randomSecret is the value the session cookie is signed with.
func randomSecret() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// portOf takes the port from a listen address such as 0.0.0.0:9110.
func portOf(listen string) string {
	if i := strings.LastIndex(listen, ":"); i >= 0 {
		return listen[i+1:]
	}
	return listen
}

func isLoopback(listen string) bool {
	host := listen
	if i := strings.LastIndex(listen, ":"); i >= 0 {
		host = listen[:i]
	}
	switch host {
	case "127.0.0.1", "localhost", "::1", "[::1]":
		return true
	}
	return false
}

func logsHint(kind ServiceManager) string {
	if kind == KindSystemd {
		return "journalctl -u " + ServiceName + " -f"
	}
	return "tail -f " + LogPath
}

func restartHint(kind ServiceManager) string {
	if kind == KindSystemd {
		return "systemctl restart " + ServiceName
	}
	return "rc-service " + ServiceName + " restart"
}

// runQuiet runs a command, discarding its output, and reports only whether
// it worked.
func runQuiet(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	return cmd.Run()
}

func output(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).Output()
	return string(out), err
}
