package install

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/user"
	"strconv"
	"strings"

	"github.com/nelvixboy/eye-of-lotus/internal/config"
)

// installConfig is the file written on a fresh install.
//
// A struct rather than a map, so that the keys come out in the order a
// person reading the file expects, and with json tags so that the names
// cannot drift from the ones config.go reads.
type installConfig struct {
	Listen        string `json:"listen"`
	Password      string `json:"password"`
	SessionSecret string `json:"session_secret"`
	Database      string `json:"database"`
	Systemd       bool   `json:"systemd"`
	SystemdDetail bool   `json:"systemd_detail"`
	Docker        bool   `json:"docker"`
	LogLevel      string `json:"log_level"`
}

// ensureAccount creates the service account and its group.
//
// The group is made explicitly: the unit names Group=eye-of-lotus, and
// useradd does not always create a group of the same name on its own.
func ensureAccount(opts *Options) error {
	opts.step("Service account")
	if _, err := user.LookupGroup(RunUser); err != nil {
		if err := opts.Run("groupadd", "--system", RunUser); err != nil {
			return fmt.Errorf("cannot create the group %s: %w", RunUser, err)
		}
		opts.say("created group %s", RunUser)
	} else {
		opts.say("group %s already exists", RunUser)
	}
	if _, err := user.Lookup(RunUser); err != nil {
		shell := "/usr/sbin/nologin"
		if _, err := os.Stat(shell); err != nil {
			shell = "/sbin/nologin"
		}
		err := opts.Run("useradd", "--system", "--gid", RunUser,
			"--home-dir", DataDir, "--shell", shell, RunUser)
		if err != nil {
			return fmt.Errorf("cannot create the user %s: %w", RunUser, err)
		}
		opts.say("created user %s", RunUser)
	} else {
		opts.say("user %s already exists", RunUser)
	}
	return ensureDockerGroup(opts)
}

// ensureDockerGroup puts the service account in the docker group when the
// host has one.
func ensureDockerGroup(opts *Options) error {
	opts.step("Container visibility")
	if _, err := user.LookupGroup("docker"); err != nil {
		opts.say("no docker group on this host; nothing to do")
		return nil
	}
	if inDockerGroup(opts) {
		opts.say("%s is already in the docker group", RunUser)
		return nil
	}
	if opts.NoDockerGroup {
		opts.say("skipped by request: the panel will report Docker as unreachable")
		return nil
	}
	if err := opts.Run("usermod", "-aG", "docker", RunUser); err != nil {
		return fmt.Errorf("cannot add %s to the docker group: %w", RunUser, err)
	}
	opts.say("added %s to the docker group", RunUser)
	// The daemon socket belongs to root:docker and opening it needs
	// membership. Without this the panel runs perfectly well and reports
	// the container section as unreachable, which reads like a fault rather
	// than a permission nobody granted. It is also root access in practice,
	// so it is said out loud and can be declined.
	opts.say("note: membership of docker is root access to this host in practice.")
	opts.say("      The panel only issues read requests, but the account could")
	opts.say("      do more. Pass -no-docker-group to leave it out.")
	return nil
}

func inDockerGroup(opts *Options) bool {
	// The group list comes from the operating system rather than from the
	// passwd file, because a process keeps the groups it was started with.
	out, err := opts.Output("id", "-nG", RunUser)
	if err != nil {
		return false
	}
	for _, name := range strings.Fields(out) {
		if name == "docker" {
			return true
		}
	}
	return false
}

// ensureDirectories creates the two directories the service uses.
func ensureDirectories(opts *Options) error {
	opts.step("Directories")
	if err := os.MkdirAll(ConfigDir, 0o755); err != nil {
		return fmt.Errorf("cannot create %s: %w", ConfigDir, err)
	}
	if err := os.MkdirAll(DataDir, 0o750); err != nil {
		return fmt.Errorf("cannot create %s: %w", DataDir, err)
	}
	uid, gid := accountIDs()
	if uid >= 0 {
		os.Chown(ConfigDir, uid, gid)
		os.Chown(DataDir, uid, gid)
	}
	os.Chmod(ConfigDir, 0o755)
	os.Chmod(DataDir, 0o750)
	opts.say("ready")
	return nil
}

// installBinary copies this program to where the service will run it from.
//
// The copy goes through a temporary file, because a half written binary left
// where the service manager can execute it is worse than no install at all.
func installBinary(opts *Options, self string) error {
	opts.step("Binary")
	if sameFile(self, BinaryPath) {
		opts.say("%s is already in place", BinaryPath)
		return nil
	}
	data, err := os.ReadFile(self)
	if err != nil {
		return fmt.Errorf("cannot read %s: %w", self, err)
	}
	tmp := BinaryPath + ".new"
	if err := os.WriteFile(tmp, data, 0o755); err != nil {
		return fmt.Errorf("cannot write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, BinaryPath); err != nil {
		return fmt.Errorf("cannot install %s: %w", BinaryPath, err)
	}
	opts.say("installed %s", BinaryPath)
	return nil
}

// ensureConfiguration writes a configuration when there is none, and
// otherwise leaves the password and the history alone.
func ensureConfiguration(opts *Options) error {
	opts.step("Configuration")
	if _, err := os.Stat(ConfigPath); err == nil {
		opts.say("%s exists; password and history are left untouched", ConfigPath)
		return updateListen(opts)
	}

	password := opts.Password
	if password == "" {
		given, err := opts.Prompt()
		if err != nil {
			return err
		}
		password = given
	}
	if password == "" {
		return errors.New("an empty password is not allowed")
	}
	hash, err := config.HashPassword(password)
	if err != nil {
		return fmt.Errorf("cannot hash the password: %w", err)
	}
	secret, err := randomSecret()
	if err != nil {
		return fmt.Errorf("cannot generate a session secret: %w", err)
	}

	body, err := json.MarshalIndent(installConfig{
		Listen:        opts.listen(),
		Password:      hash,
		SessionSecret: secret,
		Database:      DatabasePath,
		Systemd:       true,
		SystemdDetail: true,
		Docker:        true,
		LogLevel:      "info",
	}, "", "  ")
	if err != nil {
		return err
	}
	// Mode 0600 and owned by the service account: the only copy of the
	// password lives here, and it is a hash.
	if err := os.WriteFile(ConfigPath, append(body, '\n'), 0o600); err != nil {
		return fmt.Errorf("cannot write %s: %w", ConfigPath, err)
	}
	if uid, gid := accountIDs(); uid >= 0 {
		os.Chown(ConfigPath, uid, gid)
	}
	opts.say("wrote %s", ConfigPath)
	return nil
}

// updateListen moves an existing configuration onto another address, leaving
// every other setting alone and keeping the previous file beside it.
//
// This is the one key an upgrade is allowed to rewrite, because it is the
// one an operator asks for by passing the flag.
func updateListen(opts *Options) error {
	if !opts.ListenSet {
		opts.say("pass -listen ADDR to move it to another address")
		return nil
	}
	raw, err := os.ReadFile(ConfigPath)
	if err != nil {
		return fmt.Errorf("cannot read %s: %w", ConfigPath, err)
	}
	body, changed, err := mergeListen(raw, opts.Listen)
	if err != nil {
		return fmt.Errorf("%s: %w", ConfigPath, err)
	}
	if !changed {
		opts.say("listen is already %s", opts.Listen)
		return nil
	}
	if err := os.WriteFile(ConfigPath+".bak", raw, 0o600); err != nil {
		return fmt.Errorf("cannot write a backup of %s: %w", ConfigPath, err)
	}
	if err := os.WriteFile(ConfigPath, body, 0o600); err != nil {
		return fmt.Errorf("cannot write %s: %w", ConfigPath, err)
	}
	opts.say("listen changed to %s (the old file is kept as %s.bak)", opts.Listen, ConfigPath)
	return nil
}

// mergeListen replaces one key in a configuration file and leaves every
// other one exactly as it was.
//
// It round trips through a map rather than a struct on purpose: a setting
// this version has never heard of, written by a later one or by hand, must
// survive an address change rather than be quietly dropped.
func mergeListen(raw []byte, listen string) ([]byte, bool, error) {
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, false, errors.New("it is not valid JSON, so the address was left alone")
	}
	if current, _ := doc["listen"].(string); current == listen {
		return nil, false, nil
	}
	doc["listen"] = listen
	body, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, false, err
	}
	return append(body, '\n'), true, nil
}

// openFirewall opens the port when a firewall is present and active.
//
// Two rules, both narrow. The first names the interface the local network
// arrives on rather than a subnet, because a home network gets renumbered
// and a rule naming the old subnet then fails silently. The second is for
// container networks, and it is the one nobody predicts: a reverse proxy
// running in Docker reaches the panel from its bridge address rather than
// from the local network, and a firewall that drops that quietly leaves the
// proxy waiting for an answer while the browser spins.
func openFirewall(opts *Options) {
	opts.step("Firewall")
	port := portOf(opts.listen())
	if opts.NoFirewall {
		opts.say("skipped by request; open port %s yourself if you need it", port)
		return
	}
	if _, err := opts.LookPath("ufw"); err != nil {
		opts.say("ufw is not installed, so nothing was opened")
		opts.say("if another firewall is active, allow TCP %s yourself", port)
		return
	}
	status, err := opts.Output("ufw", "status")
	if err != nil || !strings.Contains(status, "Status: active") {
		opts.say("ufw is installed but not active; nothing to open")
		return
	}
	if isLoopback(opts.listen()) {
		opts.say("the panel is bound to the loopback address, so no rule is needed")
		return
	}
	if iface := defaultInterface(opts.Output); iface != "" {
		if opts.Run("ufw", "allow", "in", "on", iface,
			"to", "any", "port", port, "proto", "tcp") == nil {
			opts.say("allowed the local network on %s to reach port %s", iface, port)
		} else {
			opts.say("could not add the rule for %s; open port %s yourself", iface, port)
		}
	} else {
		opts.say("could not work out the local interface; open port %s yourself", port)
	}
	if _, err := user.LookupGroup("docker"); err == nil {
		if opts.Run("ufw", "allow", "from", "172.16.0.0/12",
			"to", "any", "port", port, "proto", "tcp") == nil {
			opts.say("allowed container networks to reach port %s (a proxy in Docker needs this)", port)
		}
	}
}

// defaultInterface reports the interface carrying the default route. Going
// through the route table avoids guessing names like eth0, which is wrong
// about half the time and was wrong on the first machine this ran on.
func defaultInterface(out func(string, ...string) (string, error)) string {
	routes, err := out("ip", "-4", "route", "show", "default")
	if err != nil {
		return ""
	}
	fields := strings.Fields(routes)
	for i, f := range fields {
		if f == "dev" && i+1 < len(fields) {
			return fields[i+1]
		}
	}
	return ""
}

// removeAccount removes the service account, and reports what happened
// rather than what was attempted: userdel refuses while a process still runs
// as that user, and announcing success anyway is how an uninstall ends up
// half done without saying so.
func removeAccount(opts *Options) {
	if _, err := user.Lookup(RunUser); err != nil {
		opts.say("user %s was already gone", RunUser)
	} else {
		if err := opts.Run("userdel", RunUser); err != nil {
			opts.say("could not remove user %s: something is still running as it", RunUser)
		} else {
			opts.say("removed user %s", RunUser)
		}
	}
	if _, err := user.LookupGroup(RunUser); err != nil {
		opts.say("group %s was already gone", RunUser)
		return
	}
	if err := opts.Run("groupdel", RunUser); err != nil {
		opts.say("could not remove group %s: it is still in use", RunUser)
		return
	}
	opts.say("removed group %s", RunUser)
}

func accountIDs() (int, int) {
	u, err := user.Lookup(RunUser)
	if err != nil {
		return -1, -1
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	return uid, gid
}

func sameFile(a, b string) bool {
	fa, err := os.Stat(a)
	if err != nil {
		return false
	}
	fb, err := os.Stat(b)
	if err != nil {
		return false
	}
	return os.SameFile(fa, fb)
}
