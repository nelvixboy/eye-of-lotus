package install

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeLookPath answers as if the named binaries were installed.
func fakeLookPath(names ...string) func(string) (string, error) {
	return func(name string) (string, error) {
		for _, n := range names {
			if n == name {
				return "/usr/bin/" + name, nil
			}
		}
		return "", os.ErrNotExist
	}
}

func TestDetectServiceManager(t *testing.T) {
	// The detection cannot be exercised for both answers on one machine,
	// because it looks at this machine. What is checked here is the part
	// that can be: an unknown host is reported as such rather than guessed
	// at.
	if got := DetectServiceManager(fakeLookPath()); got != KindNone {
		t.Errorf("with neither systemctl nor rc-service, got %v, want KindNone", got)
	}
}

func TestServiceDefinitionsCarryTheRealPaths(t *testing.T) {
	unit := systemdUnit()
	for _, want := range []string{
		"ExecStart=" + BinaryPath,
		"-config " + ConfigPath,
		"User=" + RunUser,
		"Group=" + RunUser,
		"ReadWritePaths=" + DataDir,
		"ProtectSystem=strict",
		"Restart=on-failure",
		"WantedBy=multi-user.target",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("the systemd unit does not mention %q", want)
		}
	}

	script := openRCScript()
	for _, want := range []string{
		"#!/sbin/openrc-run",
		"command=\"" + BinaryPath,
		"command_user=\"" + RunUser + ":" + RunUser,
		"pidfile=\"/run/" + ServiceName + ".pid",
		"depend()",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("the OpenRC script does not mention %q", want)
		}
	}
}

func TestPortOf(t *testing.T) {
	cases := map[string]string{
		"127.0.0.1:9110": "9110",
		"0.0.0.0:9110":   "9110",
		"[::1]:9110":     "9110",
		":9110":          "9110",
	}
	for in, want := range cases {
		if got := portOf(in); got != want {
			t.Errorf("portOf(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIsLoopback(t *testing.T) {
	for _, in := range []string{"127.0.0.1:9110", "localhost:9110", "[::1]:9110", "::1:9110"} {
		if !isLoopback(in) {
			t.Errorf("isLoopback(%q) = false, want true", in)
		}
	}
	for _, in := range []string{"0.0.0.0:9110", "192.168.1.10:9110", "[::]:9110"} {
		if isLoopback(in) {
			t.Errorf("isLoopback(%q) = true, want false", in)
		}
	}
}

// TestListeningChecksMatchWholePorts is the regression test for a check that
// once answered "the port is free" while something was on it. A shell
// version piped ss into grep -q, grep left at the first match, ss died of
// SIGPIPE, and pipefail turned the match into a failure.
func TestListeningChecksMatchWholePorts(t *testing.T) {
	listing := `State  Recv-Q Send-Q Local Address:Port Peer Address:Port Process
LISTEN 0      4096   127.0.0.1:9110      0.0.0.0:*
LISTEN 0      4096       *:91100           *:*
`
	out := func(_ string, _ ...string) (string, error) { return listing, nil }

	if !anyoneListening(out, "9110") {
		t.Error("9110 is in the listing but was reported free")
	}
	if anyoneListening(out, "911") {
		t.Error("911 is only a prefix of a listed port but was reported busy")
	}
	if anyoneListening(out, "8080") {
		t.Error("8080 is not in the listing but was reported busy")
	}

	withPID := `LISTEN 0 4096 127.0.0.1:9110 0.0.0.0:* users:(("eye-of-lotus",pid=90461,fd=10))`
	pidOut := func(_ string, _ ...string) (string, error) { return withPID, nil }
	if got := listenerPID(pidOut, "9110"); got != 90461 {
		t.Errorf("listenerPID = %d, want 90461", got)
	}
	if got := listenerPID(pidOut, "80"); got != 0 {
		t.Errorf("listenerPID for a port nobody holds = %d, want 0", got)
	}
}

func TestMergeListenKeepsEverythingElse(t *testing.T) {
	original := []byte(`{
  "listen": "127.0.0.1:9110",
  "password": "pbkdf2-sha256$210000$salt$hash",
  "session_secret": "secret",
  "database": "/var/lib/eye-of-lotus/history.db",
  "something_from_the_future": {"nested": true}
}
`)
	body, changed, err := mergeListen(original, "0.0.0.0:9110")
	if err != nil {
		t.Fatalf("mergeListen: %v", err)
	}
	if !changed {
		t.Fatal("mergeListen reported no change for a different address")
	}
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("the result is not valid JSON: %v", err)
	}
	if doc["listen"] != "0.0.0.0:9110" {
		t.Errorf("listen = %v", doc["listen"])
	}
	// The password is the thing that must not be lost, and an unknown
	// setting is the thing that must not be silently dropped.
	if doc["password"] != "pbkdf2-sha256$210000$salt$hash" {
		t.Errorf("the password changed: %v", doc["password"])
	}
	if doc["session_secret"] != "secret" {
		t.Errorf("the session secret changed: %v", doc["session_secret"])
	}
	if _, ok := doc["something_from_the_future"]; !ok {
		t.Error("a setting this version does not know about was dropped")
	}

	if _, changed, err := mergeListen(original, "127.0.0.1:9110"); err != nil || changed {
		t.Errorf("merging the same address reported changed=%v err=%v", changed, err)
	}
	if _, _, err := mergeListen([]byte("not json"), "0.0.0.0:9110"); err == nil {
		t.Error("invalid JSON should be refused rather than replaced")
	}
}

func TestInstallRefusesToRunAsANonRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root, so the refusal cannot be observed")
	}
	// The check must happen before anything is touched, which is why it is
	// first in Run rather than buried in a step.
	if err := Run(Options{Out: os.Stdout, Err: os.Stderr}); err == nil {
		t.Fatal("Run as an unprivileged user should have refused")
	}
}

func TestInstallConfigRoundTrips(t *testing.T) {
	body, err := json.MarshalIndent(installConfig{
		Listen:        "0.0.0.0:9110",
		Password:      "hash",
		SessionSecret: "secret",
		Database:      DatabasePath,
		Systemd:       true,
		SystemdDetail: true,
		Docker:        true,
		LogLevel:      "info",
	}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	// The keys have to be the ones config.go reads, so they are checked by
	// name rather than by round tripping through this same struct.
	for _, key := range []string{
		`"listen"`, `"password"`, `"session_secret"`, `"database"`,
		`"systemd"`, `"systemd_detail"`, `"docker"`, `"log_level"`,
	} {
		if !strings.Contains(string(body), key) {
			t.Errorf("the configuration does not contain the key %s", key)
		}
	}
}

func TestLogPathsAreAbsolute(t *testing.T) {
	// The service definitions name paths directly, with no shell to expand
	// anything, so a relative path would be resolved against whatever
	// directory the service manager happened to be in.
	for _, path := range []string{
		BinaryPath, ConfigPath, DataDir, SystemdUnit, OpenRCScript, LogPath, RegistryDir,
	} {
		if !filepath.IsAbs(path) {
			t.Errorf("%q is not an absolute path", path)
		}
	}
}
