// Package collect turns kernel counters into the panel's snapshot model.
//
// Every collector is defensive by design: a monitoring tool that crashes or
// hangs because one counter changed shape is worse than useless. Failures
// are recorded as issues and rendered in the UI instead of being fatal.
package collect

import (
	"context"
	"fmt"
	"runtime/debug"
	"sync"
	"time"

	"github.com/nelvixboy/eye-of-lotus/internal/docker"
	"github.com/nelvixboy/eye-of-lotus/internal/model"
	"github.com/nelvixboy/eye-of-lotus/internal/proc"
	"github.com/nelvixboy/eye-of-lotus/internal/systemd"
)

// Options configures a Collector.
type Options struct {
	// ProcRoot is the filesystem root holding /proc and /sys. Empty means
	// the real root; tests point it at fixture directories.
	ProcRoot string
	// Disks limits which mount points are reported. Empty means every
	// real filesystem.
	Disks []string
	// Ifaces limits which network interfaces are reported. Empty means
	// every interface that has seen traffic.
	Ifaces []string
	// TopProcs is how many processes to keep, sorted by CPU then memory.
	TopProcs int
	// ProbeInterval is how often the internet and DNS probes run. They
	// open real sockets and must not run on every sample.
	ProbeInterval time.Duration
	// ProbeTargets are the TCP endpoints tried when checking for internet.
	ProbeTargets []string
	// DNSName is the name resolved when checking DNS.
	DNSName string

	// Systemd is the systemd client, or nil when systemd is not in use.
	Systemd *systemd.Client
	// Docker is the container client, or nil when Docker is not in use.
	Docker *docker.Client

	// DiskDetail controls whether per-unit systemd properties are fetched.
	// It costs one D-Bus round trip per unit.
	DiskDetail bool
	// Now is injectable for tests.
	Now func() time.Time
}

// Collector produces snapshots. It is safe for concurrent use, though the
// sampling loop should be single threaded so rates stay meaningful.
type Collector struct {
	opts Options

	mu       sync.Mutex
	root     *proc.Root
	cpu      *cpuState
	disk     *diskState
	net      *netState
	procs    *procState
	services *serviceState
	probe    *probeState
	closed   bool

	// procsMu guards the process CPU baseline, which is updated once per
	// sample by collectProcesses.
	procsMu sync.Mutex

	// ownerMu guards the socket owner cache, which is filled by walking
	// /proc/<pid>/fd and is therefore expensive enough to cache.
	ownerMu  sync.Mutex
	owners   map[uint64]owner
	ownersAt time.Time
}

// New opens the proc root and prepares the stateful trackers.
func New(opts Options) (*Collector, error) {
	if opts.TopProcs <= 0 {
		opts.TopProcs = 12
	}
	if opts.ProbeInterval <= 0 {
		opts.ProbeInterval = 15 * time.Second
	}
	if len(opts.ProbeTargets) == 0 {
		opts.ProbeTargets = []string{"1.1.1.1:53", "8.8.8.8:53", "9.9.9.9:53"}
	}
	if opts.DNSName == "" {
		opts.DNSName = "one.one.one.one"
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	root, err := proc.Open(opts.ProcRoot)
	if err != nil {
		return nil, fmt.Errorf("collect: cannot open proc root %q: %w", opts.ProcRoot, err)
	}
	return &Collector{
		opts:     opts,
		root:     root,
		cpu:      newCPUState(),
		disk:     newDiskState(),
		net:      newNetState(),
		procs:    newProcState(),
		services: newServiceState(),
		probe:    newProbeState(),
	}, nil
}

// Close releases the proc root and any bus connections.
func (c *Collector) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	if c.opts.Systemd != nil {
		_ = c.opts.Systemd.Close()
	}
	return c.root.Close()
}

// Collect takes one snapshot. It never returns an error: anything that
// fails is reported inside the snapshot as an issue, because the panel's
// job is to show what is wrong, not to refuse to render.
func (c *Collector) Collect(ctx context.Context) model.Snapshot {
	started := c.opts.Now()
	var snap model.Snapshot
	snap.Time = started.UnixMilli()

	c.mu.Lock()
	defer c.mu.Unlock()

	now := started.Unix()
	snap.Uptime = c.root.Uptime()
	snap.Host = c.collectHost()

	var problems []model.Issue

	guard("hardware", &problems, func() {
		snap.CPU, snap.Memory = c.collectHardware()
		if temp, ok := readTemperature(c.opts); ok {
			snap.TempC = temp
		}
	})
	guard("disks", &problems, func() {
		snap.Disks = c.collectDisks()
	})
	guard("network", &problems, func() {
		snap.Net = c.collectNetwork(ctx, started)
	})
	guard("processes", &problems, func() {
		procs, total := c.collectProcesses(now, snap.Memory.Total)
		snap.TotalProcs = total
		if len(procs) > c.opts.TopProcs {
			snap.Procs = procs[:c.opts.TopProcs]
		} else {
			snap.Procs = procs
		}
		problems = append(problems, inspectProcesses(procs, total)...)
	})
	guard("services", &problems, func() {
		snap.Services = c.collectServices(ctx)
	})

	snap.Issues = append(problems, buildIssues(&snap, c.opts)...)
	snap.Verdict = worst(snap.Issues)
	snap.CollectMS = float64(c.opts.Now().Sub(started).Microseconds()) / 1000
	return snap
}

// guard runs one collector stage, converting a panic into an issue. A
// single malformed counter must never take the whole panel down.
func guard(stage string, issues *[]model.Issue, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			*issues = append(*issues, model.Issue{
				ID:       "collect." + stage,
				Severity: model.SevWarn,
				Title:    "Collector crashed: " + stage,
				Detail:   fmt.Sprintf("%v\n%s", r, firstLines(string(debug.Stack()), 6)),
				Hint:     "Please report this with the stack trace: it is a bug in Eye of Lotus.",
				Subject:  stage,
			})
		}
	}()
	fn()
}

func firstLines(s string, n int) string {
	out := make([]byte, 0, 512)
	count := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			count++
			if count >= n {
				break
			}
		}
		out = append(out, s[i])
	}
	return string(out)
}

func worst(issues []model.Issue) model.Severity {
	w := model.SevOK
	for _, i := range issues {
		if i.Severity.Rank() > w.Rank() {
			w = i.Severity
		}
	}
	return w
}
