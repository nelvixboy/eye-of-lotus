//go:build linux

// Package systemd talks to systemd over D-Bus without shelling out to
// systemctl, so the panel keeps working even where the CLI is missing.
package systemd

import (
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/nelvixboy/eye-of-lotus/internal/dbus"
)

const (
	managerDest  = "org.freedesktop.systemd1"
	managerPath  = "/org/freedesktop/systemd1"
	managerIface = "org.freedesktop.systemd1.Manager"
	unitIface    = "org.freedesktop.systemd1.Unit"
	serviceIface = "org.freedesktop.systemd1.Service"
	propsIface   = "org.freedesktop.DBus.Properties"
)

// Unit is one systemd unit as reported by ListUnits.
type Unit struct {
	Name        string
	Description string
	LoadState   string
	ActiveState string
	SubState    string
	Path        dbus.ObjectPath
	JobID       uint32
	JobType     string
	JobPath     dbus.ObjectPath
}

// UnitDetail carries the extra properties fetched per unit.
type UnitDetail struct {
	UnitFileState string
	FragmentPath  string
	NRestarts     uint64
	MainPID       uint32
	MemoryCurrent uint64
	HasMemory     bool
}

// Client is a lazily connected systemd manager client. It is safe for
// concurrent use and reconnects once after a broken connection.
type Client struct {
	addr string

	mu     sync.Mutex
	conn   *dbus.Conn
	failed error
	// version is cached because it never changes for a running bus.
	version string
}

// New returns a client for the given bus address. Use "system" for the
// usual system bus.
func New(address string) *Client {
	if address == "" || address == "system" {
		address = ""
	}
	return &Client{addr: address}
}

// Connect opens the bus if it is not open yet.
func (c *Client) Connect() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.connectLocked()
}

func (c *Client) connectLocked() error {
	if c.conn != nil {
		return nil
	}
	conn, err := dbus.Dial(c.addr)
	if err != nil {
		c.failed = err
		return err
	}
	c.conn = conn
	c.failed = nil
	return nil
}

// Close releases the bus connection.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		return nil
	}
	err := c.conn.Close()
	c.conn = nil
	return err
}

// Available reports whether systemd is reachable.
func (c *Client) Available() bool { return c.Connect() == nil }

// call runs a manager method, reconnecting once if the bus went away.
func (c *Client) call(member, sig string, args []any, out any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.connectLocked(); err != nil {
		return err
	}
	err := c.conn.Call(managerDest, managerPath, managerIface, member, sig, args, out)
	if err == nil {
		return nil
	}
	// A dropped connection is worth exactly one retry.
	_ = c.conn.Close()
	c.conn = nil
	if err := c.connectLocked(); err != nil {
		return err
	}
	return c.conn.Call(managerDest, managerPath, managerIface, member, sig, args, out)
}

// Version returns the systemd version string.
func (c *Client) Version() string {
	c.mu.Lock()
	if c.version != "" {
		v := c.version
		c.mu.Unlock()
		return v
	}
	c.mu.Unlock()

	var v string
	if err := c.call("GetVersionString", "", nil, &v); err != nil {
		return ""
	}
	c.mu.Lock()
	c.version = v
	c.mu.Unlock()
	return v
}

// ListUnits returns every unit systemd knows about.
func (c *Client) ListUnits() ([]Unit, error) {
	var raw []struct {
		Name        string
		Description string
		LoadState   string
		ActiveState string
		SubState    string
		Path        dbus.ObjectPath
		JobID       uint32
		JobType     string
		JobPath     dbus.ObjectPath
	}
	if err := c.call("ListUnits", "", nil, &raw); err != nil {
		return nil, err
	}
	out := make([]Unit, 0, len(raw))
	for _, u := range raw {
		out = append(out, Unit{
			Name:        u.Name,
			Description: u.Description,
			LoadState:   u.LoadState,
			ActiveState: u.ActiveState,
			SubState:    u.SubState,
			Path:        u.Path,
			JobID:       u.JobID,
			JobType:     u.JobType,
			JobPath:     u.JobPath,
		})
	}
	return out, nil
}

// Detail fetches the extra properties the panel shows for one unit. A unit
// that vanished between calls returns a zero Detail rather than an error.
func (c *Client) Detail(path dbus.ObjectPath) (UnitDetail, error) {
	if path == "" {
		return UnitDetail{}, errors.New("systemd: empty unit path")
	}
	var d UnitDetail
	if err := c.getProperty(path, unitIface, "UnitFileState", &d.UnitFileState); err != nil {
		return d, err
	}
	if err := c.getProperty(path, unitIface, "FragmentPath", &d.FragmentPath); err != nil {
		return d, err
	}
	// These properties only exist on service units; failures are expected
	// for timers, sockets and targets, so they are deliberately ignored.
	var restarts uint32
	if err := c.getProperty(path, serviceIface, "NRestarts", &restarts); err == nil {
		d.NRestarts = uint64(restarts)
	}
	var mainPID uint32
	if err := c.getProperty(path, serviceIface, "MainPID", &mainPID); err == nil {
		d.MainPID = mainPID
	}
	var mem uint64
	if err := c.getProperty(path, serviceIface, "MemoryCurrent", &mem); err == nil {
		d.MemoryCurrent = mem
		d.HasMemory = true
	}
	return d, nil
}

// getProperty reads one property through org.freedesktop.DBus.Properties.
func (c *Client) getProperty(path dbus.ObjectPath, iface, name string, out any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.connectLocked(); err != nil {
		return err
	}
	// The reply to Get is a variant; the decoder unwraps it into out.
	return c.conn.Call(managerDest, string(path), propsIface, "Get",
		"ss", []any{iface, name}, out)
}

// Explain turns a connection failure into something an operator can act
// on. A missing bus socket means systemd is not running here at all, which
// is normal on Artix, Alpine, Void and inside most containers; anything
// else is a real problem worth reading twice.
func Explain(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "no such file or directory"):
		return "systemd is not running on this machine (the D-Bus socket /run/dbus/system_bus_socket is absent)"
	case strings.Contains(msg, "connection refused"):
		return "the system bus refused the connection; the D-Bus daemon may be starting up or restarting"
	case strings.Contains(msg, "permission denied"):
		return "the panel is not allowed to talk to the system bus; check the D-Bus policy for the user it runs as"
	case strings.Contains(msg, "EOF") || strings.Contains(msg, "closed") ||
		strings.Contains(msg, "end of file"):
		return "the system bus closed the connection; the D-Bus daemon or systemd appears to have restarted"
	default:
		return "cannot reach systemd over D-Bus: " + msg
	}
}

// CleanName turns "nginx.service" into "nginx" for display.
func CleanName(name string) string {
	for _, suffix := range []string{".service", ".socket", ".timer", ".target", ".mount", ".path", ".slice", ".scope", ".device", ".swap", ".automount"} {
		if strings.HasSuffix(name, suffix) {
			return strings.TrimSuffix(name, suffix)
		}
	}
	return name
}

// NormalizeState maps systemd's two-part state onto the panel's small
// vocabulary: running, exited, failed, dead or unknown.
func NormalizeState(active, sub string) string {
	switch active {
	case "active":
		switch sub {
		case "running", "listening", "plugged", "mounted", "start":
			return "running"
		case "exited":
			return "exited"
		default:
			return "active"
		}
	case "failed":
		return "failed"
	case "activating":
		return "starting"
	case "deactivating":
		return "stopping"
	case "reloading":
		return "reloading"
	case "inactive":
		return "dead"
	default:
		return "unknown"
	}
}

// String describes the client for logs.
func (c *Client) String() string {
	if !c.Available() {
		return fmt.Sprintf("systemd(unavailable: %v)", c.failed)
	}
	return "systemd(" + c.Version() + ")"
}
