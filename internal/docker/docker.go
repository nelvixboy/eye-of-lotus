// Package docker is a very small Docker Engine API client that speaks
// HTTP over the daemon's Unix socket.
//
// Eye of Lotus needs exactly three read-only endpoints, so the official
// SDK (and its dependency tree) is not worth the weight here. Nothing in
// this package can start, stop or modify a container.
package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultSocket is where the daemon listens on a stock install.
const DefaultSocket = "/var/run/docker.sock"

// Client talks to the Docker daemon.
type Client struct {
	socket string
	http   *http.Client
	// version is the API version prefix used in request paths, for example
	// "v1.43". Empty means the daemon picks its own default.
	version string
}

// New returns a client for the given socket path. An empty path means the
// default socket.
func New(socket string) *Client {
	if socket == "" {
		socket = DefaultSocket
	}
	return &Client{
		socket: socket,
		http: &http.Client{
			Timeout: 10 * time.Second,
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", socket)
				},
				DisableCompression: true,
				MaxIdleConns:       4,
				IdleConnTimeout:    30 * time.Second,
			},
		},
	}
}

// Socket returns the socket path the client uses.
func (c *Client) Socket() string { return c.socket }

// Available reports whether the daemon answers.
func (c *Client) Available(ctx context.Context) bool {
	return c.get(ctx, "/_ping", nil) == nil
}

// Version returns the daemon version string, for example "24.0.7".
func (c *Client) Version(ctx context.Context) (string, error) {
	var v struct {
		Version    string `json:"Version"`
		APIVersion string `json:"ApiVersion"`
	}
	if err := c.get(ctx, "/version", &v); err != nil {
		return "", err
	}
	if v.APIVersion != "" {
		c.version = "v" + v.APIVersion
	}
	return v.Version, nil
}

// Info returns the counts the panel shows in the Docker card.
func (c *Client) Info(ctx context.Context) (Info, error) {
	var raw struct {
		Containers        int    `json:"Containers"`
		ContainersRunning int    `json:"ContainersRunning"`
		ContainersPaused  int    `json:"ContainersPaused"`
		ContainersStopped int    `json:"ContainersStopped"`
		Images            int    `json:"Images"`
		ServerVersion     string `json:"ServerVersion"`
	}
	if err := c.get(ctx, "/info", &raw); err != nil {
		return Info{}, err
	}
	return Info{
		Containers: raw.Containers,
		Running:    raw.ContainersRunning,
		Paused:     raw.ContainersPaused,
		Stopped:    raw.ContainersStopped,
		Images:     raw.Images,
		Version:    raw.ServerVersion,
	}, nil
}

// Info is a summary of the daemon's workload.
type Info struct {
	Containers int
	Running    int
	Paused     int
	Stopped    int
	Images     int
	Version    string
}

// Container is one container as reported by the list endpoint.
type Container struct {
	ID      string   `json:"Id"`
	Names   []string `json:"Names"`
	Image   string   `json:"Image"`
	Command string   `json:"Command"`
	Created int64    `json:"Created"`
	State   string   `json:"State"`
	Status  string   `json:"Status"`
	Health  string   `json:"-"`
	// RestartCount is read from the detailed endpoint.
	RestartCount int `json:"-"`
	// StartedAt is an RFC3339 timestamp.
	StartedAt string `json:"-"`
}

// Name returns the first container name without its leading slash.
func (c Container) Name() string {
	if len(c.Names) == 0 {
		return c.ID[:min(12, len(c.ID))]
	}
	return strings.TrimPrefix(c.Names[0], "/")
}

// Containers lists every container, running or not.
func (c *Client) Containers(ctx context.Context) ([]Container, error) {
	var raw []struct {
		ID      string   `json:"Id"`
		Names   []string `json:"Names"`
		Image   string   `json:"Image"`
		Command string   `json:"Command"`
		Created int64    `json:"Created"`
		State   string   `json:"State"`
		Status  string   `json:"Status"`
	}
	if err := c.get(ctx, "/containers/json?all=1", &raw); err != nil {
		return nil, err
	}
	out := make([]Container, 0, len(raw))
	for _, r := range raw {
		out = append(out, Container{
			ID: r.ID, Names: r.Names, Image: r.Image, Command: r.Command,
			Created: r.Created, State: r.State, Status: r.Status,
		})
	}
	return out, nil
}

// Inspect returns the health state, restart count and start time of one
// container. These live only on the detailed endpoint.
func (c *Client) Inspect(ctx context.Context, id string) (Container, error) {
	var raw struct {
		RestartCount int `json:"RestartCount"`
		State        struct {
			Status string `json:"Status"`
			Health *struct {
				Status string `json:"Status"`
			} `json:"Health"`
			StartedAt string `json:"StartedAt"`
		} `json:"State"`
	}
	if err := c.get(ctx, "/containers/"+url.PathEscape(id)+"/json", &raw); err != nil {
		return Container{}, err
	}
	out := Container{RestartCount: raw.RestartCount, StartedAt: raw.State.StartedAt}
	if raw.State.Health != nil {
		out.Health = raw.State.Health.Status
	}
	return out, nil
}

// Stats is one container's resource usage.
type Stats struct {
	CPUPct     float64
	MemBytes   uint64
	MemLimit   uint64
	MemPct     float64
	NetRxBytes uint64
	NetTxBytes uint64
	BlockRead  uint64
	BlockWrite uint64
	PIDs       int
}

// Stats reads one container's current usage with stream disabled, so the
// call returns a single sample and does not block.
func (c *Client) Stats(ctx context.Context, id string) (Stats, error) {
	var raw struct {
		CPUStats struct {
			CPUUsage struct {
				TotalUsage uint64 `json:"total_usage"`
			} `json:"cpu_usage"`
			SystemUsage uint64 `json:"system_cpu_usage"`
			OnlineCPUs  uint32 `json:"online_cpus"`
		} `json:"cpu_stats"`
		PreCPUStats struct {
			CPUUsage struct {
				TotalUsage uint64 `json:"total_usage"`
			} `json:"cpu_usage"`
			SystemUsage uint64 `json:"system_cpu_usage"`
		} `json:"precpu_stats"`
		MemoryStats struct {
			Usage uint64            `json:"usage"`
			Limit uint64            `json:"limit"`
			Stats map[string]uint64 `json:"stats"`
		} `json:"memory_stats"`
		Networks map[string]struct {
			RxBytes uint64 `json:"rx_bytes"`
			TxBytes uint64 `json:"tx_bytes"`
		} `json:"networks"`
		BlkioStats struct {
			IOServiceBytesRecursive []struct {
				Op    string `json:"op"`
				Value uint64 `json:"value"`
			} `json:"io_service_bytes_recursive"`
		} `json:"blkio_stats"`
		PidsStats struct {
			Current int `json:"current"`
		} `json:"pids_stats"`
	}
	if err := c.get(ctx, "/containers/"+url.PathEscape(id)+"/stats?stream=0&one-shot=1", &raw); err != nil {
		return Stats{}, err
	}

	var st Stats
	// Memory usage in Docker includes page cache, which makes every
	// container look hungry; the cached figure is subtracted, the same way
	// docker stats does it.
	cache := raw.MemoryStats.Stats["inactive_file"]
	if cache == 0 {
		cache = raw.MemoryStats.Stats["cache"]
	}
	if raw.MemoryStats.Usage > cache {
		st.MemBytes = raw.MemoryStats.Usage - cache
	}
	st.MemLimit = raw.MemoryStats.Limit
	st.MemPct = pct(float64(st.MemBytes), float64(st.MemLimit))

	cpuDelta := float64(raw.CPUStats.CPUUsage.TotalUsage) - float64(raw.PreCPUStats.CPUUsage.TotalUsage)
	sysDelta := float64(raw.CPUStats.SystemUsage) - float64(raw.PreCPUStats.SystemUsage)
	online := float64(raw.CPUStats.OnlineCPUs)
	if online == 0 {
		online = 1
	}
	if sysDelta > 0 && cpuDelta > 0 {
		st.CPUPct = round2(cpuDelta / sysDelta * online * 100)
	}

	for _, n := range raw.Networks {
		st.NetRxBytes += n.RxBytes
		st.NetTxBytes += n.TxBytes
	}
	for _, e := range raw.BlkioStats.IOServiceBytesRecursive {
		switch strings.ToLower(e.Op) {
		case "read":
			st.BlockRead += e.Value
		case "write":
			st.BlockWrite += e.Value
		}
	}
	st.PIDs = raw.PidsStats.Current
	return st, nil
}

// get performs a GET against the daemon. The API version prefix is added
// on the first successful call so that the client works with old daemons.
func (c *Client) get(ctx context.Context, path string, out any) error {
	prefix := c.version
	if prefix == "" {
		prefix = "/v1.41"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker"+prefix+path, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound && c.version == "" {
		// The daemon rejected our guessed API version; retry without one.
		c.version = "unversioned"
		return c.getUnversioned(ctx, path, out)
	}
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("docker: %s: %s: %s", path, resp.Status, strings.TrimSpace(string(body)))
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (c *Client) getUnversioned(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker"+path, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("docker: %s: %s", path, resp.Status)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// ErrUnavailable is returned when the daemon cannot be reached.
var ErrUnavailable = errors.New("docker: daemon is not reachable")

func pct(a, b float64) float64 {
	if b <= 0 {
		return 0
	}
	return round2(a / b * 100)
}

func round2(v float64) float64 {
	return float64(int64(v*100+0.5)) / 100
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
