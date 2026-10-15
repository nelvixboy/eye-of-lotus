package collect

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"sync"
	"time"

	"github.com/nelvixboy/eye-of-lotus/internal/model"
)

// Demo produces a believable machine out of nothing.
//
// It exists for two reasons: anyone can try the panel without a server,
// and the screenshots in the README are reproducible. The generated
// machine has a slow diurnal cycle, a few services, and an incident every
// so often, so that the interface is exercised rather than flattered.
type Demo struct {
	mu    sync.Mutex
	rng   *rand.Rand
	start time.Time
	last  time.Time
	// launch is when the demo began; the incident timer runs from launch,
	// not from the synthetic boot time in start.
	launch time.Time
	// seed is retained so the startup log can name it without drawing
	// from the RNG.
	seed   int64
	tick   int
	offset float64

	// machine state that drifts over time
	diskPct float64
	cpuBase float64
	memBase float64
	// cpuSpike records the tick when the CPU first crossed the incident
	// threshold, so the demo can report the start of the trouble.
	cpuSpike int
}

// DemoOptions tunes the synthetic machine.
type DemoOptions struct {
	// Hostname is the name reported by the synthetic machine.
	Hostname string
	// Seed makes the sequence reproducible.
	Seed int64
	// Incident makes the demo machine develop a problem after roughly
	// this much time, so the critical styling can be seen. Zero disables
	// incidents and the machine stays healthy.
	Incident time.Duration
	// Now is injectable for tests.
	Now func() time.Time
}

// gig is a byte count in gibibytes, written out because the demo machine
// deals in round numbers that are unreadable as raw integers.
func gig(n float64) uint64 { return uint64(n * 1024 * 1024 * 1024) }

// NewDemo builds a synthetic machine.
func NewDemo(opt DemoOptions) *Demo {
	if opt.Hostname == "" {
		opt.Hostname = "lotus-demo"
	}
	if opt.Seed == 0 {
		opt.Seed = 20261015
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	now := opt.Now()
	return &Demo{
		rng:     rand.New(rand.NewSource(opt.Seed)),
		seed:    opt.Seed,
		start:   now.Add(-37*time.Hour - 12*time.Minute),
		last:    now,
		launch:  now,
		diskPct: 41,
		cpuBase: 14,
		memBase: 46,
		offset:  float64(opt.Incident.Seconds()),
	}
}

// Snapshot returns the state of the synthetic machine at this moment.
func (d *Demo) Snapshot(ctx context.Context) model.Snapshot {
	d.mu.Lock()
	defer d.mu.Unlock()

	now := time.Now()
	elapsed := now.Sub(d.last).Seconds()
	if elapsed < 0 {
		elapsed = 0
	}
	d.last = now
	d.tick++

	// A slow sine gives the graphs a shape instead of pure noise, which
	// is what makes a screenshot look like a real machine.
	t := float64(now.Unix()%86400) / 86400.0
	diurnal := math.Sin(t*2*math.Pi-1.2)*0.5 + 0.5

	// Incident: after the configured delay the synthetic machine grows a
	// filesystem that slowly fills, so a critical finding appears without
	// anyone having to break a real server.
	if d.offset > 0 && now.Sub(d.launch).Seconds() > d.offset {
		d.diskPct = math.Min(99.4, d.diskPct+0.06)
		d.cpuBase = math.Min(88, d.cpuBase+0.03)
		if d.cpuBase > 80 && d.cpuSpike == 0 {
			d.cpuSpike = d.tick
		}
	}
	// Nothing grows forever: the demo machine is eventually cleaned up.
	if d.diskPct > 99 {
		d.diskPct = 41
		d.cpuBase = 14
	}

	jitter := func(scale float64) float64 { return (d.rng.Float64()*2 - 1) * scale }
	cpu := clamp(d.cpuBase+diurnal*22+jitter(6), 1, 100)
	mem := clamp(d.memBase+diurnal*9+jitter(2.5), 4, 99)
	iowait := clamp(3+math.Max(0, cpu-60)*0.35+jitter(1.5), 0, 100)

	snap := model.Snapshot{
		Time:     now.UnixMilli(),
		Uptime:   now.Sub(d.start).Seconds(),
		TempC:    round2(41 + diurnal*7 + jitter(1.5)),
		Host:     d.host(d.start),
		CPU:      d.cpu(cpu, iowait, diurnal),
		Memory:   d.memory(mem),
		Disks:    d.disks(cpu),
		Net:      d.network(elapsed, diurnal),
		Services: d.services(),
	}
	snap.Procs, snap.TotalProcs = d.processes(snap.Memory.Total, cpu)
	snap.Issues = buildIssues(&snap, Options{})
	snap.Verdict = worst(snap.Issues)
	snap.CollectMS = round2(2.4 + jitter(0.9))
	return snap
}

func (d *Demo) host(start time.Time) model.SystemInfo {
	return model.SystemInfo{
		Hostname:       "lotus-demo",
		OS:             "Debian GNU/Linux 13 (trixie)",
		Kernel:         "6.12.9-amd64",
		Arch:           "amd64",
		Virtualization: "kvm",
		CPUs:           4,
	}
}

func (d *Demo) cpu(usage, iowait, diurnal float64) model.CPU {
	cores := make([]float64, 4)
	for i := range cores {
		cores[i] = clamp(usage+(d.rng.Float64()*2-1)*18, 0, 100)
	}
	return model.CPU{
		Usage:  round2(usage),
		Cores:  cores,
		Load1:  round2(usage / 100 * 4 * 1.05),
		Load5:  round2(usage / 100 * 4 * 0.92),
		Load15: round2(usage/100*4*0.8 + diurnal*0.2),
		Iowait: round2(iowait),
		Steal:  round2(clamp((d.rng.Float64()*2-1)*1.4+0.4, 0, 5)),
		Model:  "AMD Ryzen 5 5600X 6-Core Processor",
		MHz:    round2(3.4 + diurnal*0.5),
	}
}

func (d *Demo) memory(usedPct float64) model.Memory {
	const total = 16 * 1024 * 1024 * 1024
	used := uint64(usedPct / 100 * total)
	const swapTotal = 4 * 1024 * 1024 * 1024
	swapPct := clamp(8+d.rng.Float64()*12, 0, 100)
	return model.Memory{
		Total:     total,
		Used:      used,
		Available: total - used,
		Free:      uint64(float64(total-used) * 0.35),
		Cached:    gig(2.4),
		Buffers:   uint64(310 * 1024 * 1024),
		SwapTotal: swapTotal,
		SwapUsed:  uint64(swapPct / 100 * swapTotal),
		UsedPct:   round2(usedPct),
		SwapPct:   round2(swapPct),
	}
}

func (d *Demo) disks(cpu float64) []model.Disk {
	const rootTotal = 468 * 1024 * 1024 * 1024
	rootUsed := uint64(d.diskPct / 100 * rootTotal)
	return []model.Disk{
		{
			Mount: "/", Device: "/dev/nvme0n1p2", FSType: "ext4",
			Total: rootTotal, Used: rootUsed, Free: rootTotal - rootUsed,
			UsedPct: round2(d.diskPct), InodesPct: round2(11 + d.diskPct/12),
			ReadBps: round2(cpu * 96 * 1024), WriteBps: round2(cpu * 41 * 1024),
			ReadIOPS: round2(cpu * 3.1), WriteIOPS: round2(cpu * 1.4),
		},
		{
			Mount: "/var/backups", Device: "/dev/sdb1", FSType: "xfs",
			Total: gig(2048), Used: gig(1290),
			Free:    gig(758),
			UsedPct: 63.2, InodesPct: 0.4,
			ReadBps: 0, WriteBps: round2(cpu * 210 * 1024), ReadIOPS: 0, WriteIOPS: round2(cpu * 2.2),
		},
		{
			Mount: "/boot", Device: "/dev/nvme0n1p1", FSType: "vfat",
			Total: 1022 * 1024 * 1024, Used: 318 * 1024 * 1024, Free: 704 * 1024 * 1024,
			UsedPct: 31.1, InodesPct: 0.1, ReadOnly: true,
		},
	}
}

func (d *Demo) network(elapsed, diurnal float64) model.Network {
	rx := round2((6 + diurnal*9 + d.rng.Float64()*4) * 1024 * 1024)
	tx := round2((1.6 + diurnal*3 + d.rng.Float64()*1.5) * 1024 * 1024)
	latency := round2(9 + d.rng.Float64()*6)
	if d.rng.Float64() < 0.004 {
		latency = 480 // an occasional slow probe, so the warning path is visible
	}
	listeners := []model.Listen{
		{Proto: "tcp", Addr: "127.0.0.1", Port: 22, Process: "sshd", PID: 812, Exposed: false},
		{Proto: "tcp", Addr: "0.0.0.0", Port: 22, Process: "sshd", PID: 812, Exposed: true},
		{Proto: "tcp", Addr: "0.0.0.0", Port: 80, Process: "nginx", PID: 1204, Exposed: true},
		{Proto: "tcp", Addr: "0.0.0.0", Port: 443, Process: "nginx", PID: 1204, Exposed: true},
		{Proto: "tcp", Addr: "127.0.0.1", Port: 5432, Process: "postgres", PID: 2210, Exposed: false},
		{Proto: "tcp", Addr: "127.0.0.1", Port: 9110, Process: "eye-of-lotus", PID: 3391, Exposed: false},
		{Proto: "udp", Addr: "127.0.0.1", Port: 53, Process: "unbound", PID: 640, Exposed: false},
	}
	return model.Network{
		Online:     true,
		CheckedVia: "tcp:1.1.1.1:53",
		LatencyMS:  latency,
		DNSOK:      true,
		DNSMS:      round2(11 + d.rng.Float64()*8),
		DNSName:    "one.one.one.one",
		RxBps:      rx,
		TxBps:      tx,
		RxTotal:    gig(3174),
		TxTotal:    gig(942),
		Ifaces: []model.Iface{
			{
				Name: "eth0", Up: true, MAC: "52:54:00:8f:1c:2a",
				RxBps: rx, TxBps: tx,
				RxTotal: gig(3174),
				TxTotal: gig(942),
				RxErrs:  0, TxErrs: 0, RxDrops: 3, TxDrops: 0,
				Addrs: []string{"192.168.1.42/24", "2a01:4f8:c17:1::42/64"},
			},
			{
				Name: "wg0", Up: true, MAC: "",
				RxBps: round2(rx * 0.08), TxBps: round2(tx * 0.22),
				RxTotal: 41 * 1024 * 1024 * 1024, TxTotal: 12 * 1024 * 1024 * 1024,
				Addrs: []string{"10.8.0.1/24"},
			},
		},
		Listening: listeners,
	}
}

func (d *Demo) processes(memTotal uint64, cpu float64) ([]model.Process, int) {
	now := time.Now()
	type seed struct {
		pid     int
		user    string
		name    string
		cmd     string
		cpu     float64
		memPct  float64
		threads int
		age     time.Duration
	}
	seeds := []seed{
		{1204, "www-data", "nginx", "nginx: worker process", cpu * 0.31, 2.1, 4, 96 * time.Hour},
		{2210, "postgres", "postgres", "/usr/lib/postgresql/16/bin/postgres -D /var/lib/postgresql/16/main", cpu * 0.44, 22.6, 31, 96 * time.Hour},
		{3391, "lotus", "eye-of-lotus", "/usr/local/bin/eye-of-lotus -config /etc/eye-of-lotus/config.json", cpu * 0.03, 0.4, 9, 37 * time.Hour},
		{812, "root", "sshd", "sshd: /usr/sbin/sshd -D [listener]", cpu * 0.01, 0.2, 1, 96 * time.Hour},
		{640, "unbound", "unbound", "/usr/sbin/unbound -d -p", cpu * 0.06, 1.1, 3, 96 * time.Hour},
		{4102, "dev", "node", "node /srv/app/server.js", cpu * 0.72, 7.4, 11, 4 * time.Hour},
		{5320, "dev", "docker-proxy", "docker-proxy -proto tcp -host-ip 0.0.0.0 -host-port 8080", cpu * 0.01, 0.1, 1, 4 * time.Hour},
		{5331, "root", "containerd-shim", "/usr/bin/containerd-shim-runc-v2 -namespace moby -id 9f2c", cpu * 0.02, 0.3, 9, 4 * time.Hour},
		{6110, "backup", "restic", "restic backup /srv --repo /var/backups/restic", cpu * 0.18, 3.2, 6, 40 * time.Minute},
		{1, "root", "systemd", "/sbin/init", cpu * 0.01, 0.1, 1, 96 * time.Hour},
	}
	out := make([]model.Process, 0, len(seeds))
	for _, s := range seeds {
		rss := uint64(s.memPct / 100 * float64(memTotal))
		out = append(out, model.Process{
			PID: s.pid, PPID: 1, User: s.user, Name: s.name, Cmdline: s.cmd,
			State: "sleeping", CPU: round2(s.cpu), MemPct: round2(s.memPct),
			RSS: rss, Threads: s.threads,
			Started: now.Add(-s.age).UnixMilli(), Uptime: s.age.Seconds(),
		})
	}
	// One process shows what a busy process looks like.
	out[5].State = "running"
	out[5].CPU = round2(clamp(cpu*0.62, 0.5, 99))
	return out, 214
}

func (d *Demo) services() model.Services {
	unit := func(name, desc, active, sub, enabled, status string, restarts int) model.Service {
		return model.Service{
			Kind: "systemd", Name: name, Description: desc,
			LoadState: "loaded", ActiveState: active, SubState: sub,
			Status: status, Enabled: enabled, Restarts: restarts,
		}
	}
	units := []model.Service{
		unit("nginx.service", "A high performance web server and a reverse proxy server", "active", "running", "enabled", "running", 0),
		unit("postgresql.service", "PostgreSQL RDBMS", "active", "running", "enabled", "running", 0),
		unit("docker.service", "Docker Application Container Engine", "active", "running", "enabled", "running", 0),
		unit("unbound.service", "Validating, recursive, caching DNS resolver", "active", "running", "enabled", "running", 0),
		unit("eye-of-lotus.service", "Eye of Lotus server monitor", "active", "running", "enabled", "running", 0),
		unit("cron.service", "Regular background program processing daemon", "active", "running", "enabled", "running", 0),
		unit("ssh.service", "OpenBSD Secure Shell server", "active", "running", "enabled", "running", 0),
		unit("restic-backup.timer", "Nightly restic backup", "active", "waiting", "enabled", "active", 0),
		unit("certbot-renew.timer", "Twice daily certificate renewal", "active", "waiting", "enabled", "active", 0),
		unit("systemd-timesyncd.service", "Network Time Synchronization", "active", "running", "enabled", "running", 0),
		unit("fail2ban.service", "Fail2Ban Service", "inactive", "dead", "enabled", "dead", 0),
		unit("apt-daily-upgrade.service", "Daily apt upgrade and clean activities", "inactive", "dead", "static", "dead", 0),
		unit("foo-worker.service", "Example background worker", "failed", "failed", "enabled", "failed", 3),
	}
	items := []model.Service{
		{Kind: "docker", Name: "web", Description: "nginx:1.27-alpine Up 4 days (healthy)", Status: "running", ActiveState: "running", Image: "nginx:1.27-alpine", Health: "healthy", Restarts: 0},
		{Kind: "docker", Name: "app", Description: "ghcr.io/example/app:2.4.1 Up 4 days (healthy)", Status: "running", ActiveState: "running", Image: "ghcr.io/example/app:2.4.1", Health: "healthy", Restarts: 0},
		{Kind: "docker", Name: "redis", Description: "redis:7-alpine Up 4 days", Status: "running", ActiveState: "running", Image: "redis:7-alpine", Health: "", Restarts: 2},
		{Kind: "docker", Name: "worker", Description: "ghcr.io/example/worker:2.4.1 Restarting (1) 12 seconds ago", Status: "restarting", ActiveState: "restarting", Image: "ghcr.io/example/worker:2.4.1", Restarts: 47, Problem: "the container is stuck in a restart loop"},
		{Kind: "docker", Name: "metrics", Description: "prom/prometheus:v2.55.0 Exited (0) 2 days ago", Status: "exited", ActiveState: "exited", Image: "prom/prometheus:v2.55.0", Health: "", Restarts: 0},
	}
	return model.Services{
		Systemd: model.SystemdInfo{
			Available: true, Version: "257.1", Total: len(units), Running: 10, Failed: 1, Units: units,
		},
		Docker: model.DockerInfo{
			Available: true, Version: "27.4.1", Total: len(items), Running: 3, Unhealthy: 0, Images: 14, Items: items,
		},
	}
}

// String describes the demo machine for the startup log.
func (d *Demo) String() string {
	return fmt.Sprintf("demo machine (seed %d)", d.seed)
}
