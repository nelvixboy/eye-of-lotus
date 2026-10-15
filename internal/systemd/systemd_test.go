package systemd

import (
	"errors"
	"strings"
	"testing"
)

func TestExplainTurnsFailuresIntoAdvice(t *testing.T) {
	cases := []struct {
		err   error
		wants string
	}{
		{errors.New("dial unix /run/dbus/system_bus_socket: connect: no such file or directory"), "not running on this machine"},
		{errors.New("dial unix /run/dbus/system_bus_socket: connect: connection refused"), "refused the connection"},
		{errors.New("dbus: authentication rejected: REJECTED"), "dbus: authentication rejected"},
		{errors.New("EOF"), "closed the connection"},
		{errors.New("unexpected end of file"), "closed the connection"},
	}
	for _, tc := range cases {
		got := Explain(tc.err)
		if !strings.Contains(got, tc.wants) {
			t.Errorf("Explain(%v) = %q, want it to mention %q", tc.err, got, tc.wants)
		}
	}
	if got := Explain(nil); got != "" {
		t.Errorf("Explain(nil) = %q, want an empty string", got)
	}
}

func TestNormalizeState(t *testing.T) {
	cases := []struct {
		active, sub, want string
	}{
		{"active", "running", "running"},
		{"active", "exited", "exited"},
		{"active", "listening", "running"},
		{"active", "waiting", "active"},
		{"failed", "failed", "failed"},
		{"activating", "start-pre", "starting"},
		{"deactivating", "stop", "stopping"},
		{"inactive", "dead", "dead"},
		{"reloading", "reload", "reloading"},
		{"", "", "unknown"},
	}
	for _, tc := range cases {
		if got := NormalizeState(tc.active, tc.sub); got != tc.want {
			t.Errorf("NormalizeState(%q, %q) = %q, want %q", tc.active, tc.sub, got, tc.want)
		}
	}
}

func TestCleanName(t *testing.T) {
	cases := map[string]string{
		"nginx.service":   "nginx",
		"docker.socket":   "docker",
		"restic.timer":    "restic",
		"-.mount":         "-",
		"plain":           "plain",
		"weird.something": "weird.something",
	}
	for in, want := range cases {
		if got := CleanName(in); got != want {
			t.Errorf("CleanName(%q) = %q, want %q", in, got, want)
		}
	}
}
