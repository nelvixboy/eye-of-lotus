package collect

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nelvixboy/eye-of-lotus/internal/model"
)

// fixture writes a fake /proc and /sys tree and returns a Collector that
// reads from it. The numbers are chosen to trip the thresholds that the
// issue engine is supposed to detect.
func fixture(t *testing.T, opts Options, extra map[string]string) *Collector {
	t.Helper()
	dir := t.TempDir()

	files := map[string]string{
		"proc/stat": `cpu  1000 20 300 4000 500 5 5 0 0 0
cpu0 500 10 150 2000 250 2 3 0 0 0
cpu1 500 10 150 2000 250 3 2 0 0 0
ctxt 999
btime 1700000000
`,
		"proc/loadavg": "8.00 3.10 2.00 3/1861 13\n",
		"proc/uptime":  "920523.45 1234567.89\n",
		"proc/meminfo": `MemTotal:       16000000 kB
MemFree:          200000 kB
MemAvailable:     640000 kB
Buffers:          100000 kB
Cached:           400000 kB
SReclaimable:      50000 kB
SwapTotal:       5000000 kB
SwapFree:        1800000 kB
`,
		"proc/cpuinfo": "processor: 0\nmodel name: Fixture CPU @ 2.50GHz\ncpu MHz: 2500.000\n",
		"proc/version": "Linux version 6.1.0-fixture\n",
		"proc/diskstats": ` 259 0 nvme0n1 1000 50 20480 300 2000 60 40960 500 0 800 900 10 0
   8 0 sda 10 0 80 5 20 0 160 7 0 12 8 0 0
`,
		"proc/net/dev": `Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
    lo: 1000 10 0 0 0 0 0 0 1000 10 0 0 0 0 0 0
  eth0: 55555555 44444 1 2 0 0 0 0 66666666 33333 3 4 0 5 0 0
`,
		// 0.0.0.0:2375 is the unauthenticated Docker API: a critical finding.
		"proc/net/tcp": `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000:0947 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 12345 1 0000000000000000 100 0 0 10 0
   1: 0100007F:0016 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 54321 1 0000000000000000 100 0 0 10 0
`,
		"proc/net/tcp6": "  sl  local_address                         remote_address                        st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n",
		"proc/net/udp":  "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n",
		"proc/net/udp6": "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n",
		"proc/mounts": `/dev/nvme0n1p2 / ext4 rw,relatime 0 0
/dev/sdb1 /data xfs rw 0 0
proc /proc proc rw 0 0
tmpfs /run tmpfs rw 0 0
`,
		"proc/1/stat":                          "1 (systemd) S 0 1 1 0 -1 4194560 100 0 0 0 40 60 0 0 20 0 1 0 100 1000000 500",
		"proc/4242/stat":                       "4242 (postgres) R 1 4242 4242 0 -1 4194560 200 0 0 0 900 100 0 0 20 0 12 0 5000 5000000 9000",
		"proc/4242/cmdline":                    "/usr/lib/postgresql/16/bin/postgres\x00-D\x00/var/lib/postgresql\x00",
		"proc/4242/status":                     "Name:\tpostgres\nUid:\t70\t70\t70\t70\n",
		"proc/4242/statm":                      "1000 900 200 1 0 500 0\n",
		"sys/class/thermal/thermal_zone0/temp": "85000\n",
		"etc/os-release":                       "NAME=\"Fixture\"\nPRETTY_NAME=\"Fixture Linux 1.0\"\n",
		"proc/sys/kernel/osrelease":            "6.1.0-fixture\n",
	}
	for k, v := range extra {
		files[k] = v
	}
	for name, content := range files {
		full := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	opts.ProcRoot = dir
	if opts.TopProcs == 0 {
		opts.TopProcs = 5
	}
	if opts.ProbeInterval == 0 {
		opts.ProbeInterval = time.Minute
	}
	if len(opts.ProbeTargets) == 0 {
		// The probe target is the loopback port from the fixture, which is
		// closed, so the collector will report the machine as offline.
		// That is exactly the behaviour under test.
		opts.ProbeTargets = []string{"127.0.0.1:2375"}
	}
	if opts.DNSName == "" {
		opts.DNSName = "invalid.invalid"
	}
	c, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// TestCollectEndToEnd drives one full pass over the fake machine.
func TestCollectEndToEnd(t *testing.T) {
	c := fixture(t, Options{}, nil)
	snap := c.Collect(context.Background())

	if snap.Time == 0 {
		t.Error("snapshot has no timestamp")
	}
	if snap.Uptime < 900000 {
		t.Errorf("uptime = %v, want about 920523", snap.Uptime)
	}
	if snap.Host.Hostname == "" || snap.Host.OS != "Fixture Linux 1.0" {
		t.Errorf("host info wrong: %+v", snap.Host)
	}
	if snap.Host.Kernel != "6.1.0-fixture" {
		t.Errorf("kernel = %q", snap.Host.Kernel)
	}
	if snap.CPU.Model != "Fixture CPU @ 2.50GHz" {
		t.Errorf("cpu model = %q", snap.CPU.Model)
	}
	if snap.CPU.Load1 != 8.00 {
		t.Errorf("load1 = %v", snap.CPU.Load1)
	}
	if len(snap.CPU.Cores) != 2 {
		t.Errorf("expected 2 cores, got %d", len(snap.CPU.Cores))
	}
	if snap.Memory.Total != 16000000*1024 {
		t.Errorf("mem total = %d", snap.Memory.Total)
	}
	if snap.Memory.UsedPct < 90 {
		t.Errorf("mem used = %.1f%%, expected the fixture to read as nearly full", snap.Memory.UsedPct)
	}
	if len(snap.Disks) != 1 {
		t.Fatalf("expected 1 real filesystem, got %d: %+v", len(snap.Disks), snap.Disks)
	}
	if snap.Disks[0].Mount != "/" || snap.Disks[0].Device != "/dev/nvme0n1p2" {
		t.Errorf("first disk wrong: %+v", snap.Disks[0])
	}
	// Loopback is excluded from the totals on purpose: it would double
	// count traffic that never leaves the machine.
	if snap.Net.RxTotal != 55555555 || snap.Net.TxTotal != 66666666 {
		t.Errorf("network totals wrong: rx=%d tx=%d", snap.Net.RxTotal, snap.Net.TxTotal)
	}
	if len(snap.Net.Listening) != 2 {
		t.Fatalf("expected 2 listening sockets, got %d: %+v", len(snap.Net.Listening), snap.Net.Listening)
	}
	// Listeners are sorted by port, so the SSH socket on 22 comes first.
	if snap.Net.Listening[0].Port != 22 || snap.Net.Listening[0].Exposed {
		t.Errorf("first listener should be 22 bound to localhost: %+v", snap.Net.Listening[0])
	}
	if !snap.Net.Listening[1].Exposed || snap.Net.Listening[1].Port != 2375 {
		t.Errorf("second listener should be the exposed docker port: %+v", snap.Net.Listening[1])
	}
	if snap.TotalProcs != 2 {
		t.Errorf("total processes = %d, want 2", snap.TotalProcs)
	}
	if len(snap.Procs) != 2 {
		t.Fatalf("expected 2 processes, got %d", len(snap.Procs))
	}
	var pg *model.Process
	for i := range snap.Procs {
		if snap.Procs[i].PID == 4242 {
			pg = &snap.Procs[i]
		}
	}
	if pg == nil {
		t.Fatal("postgres process missing from the snapshot")
	}
	if pg.User != "70" && pg.User == "" {
		t.Errorf("process user not resolved: %+v", pg)
	}
	if pg.RSS != 900*4096 {
		t.Errorf("rss = %d, want %d", pg.RSS, 900*4096)
	}
	if pg.Cmdline != "/usr/lib/postgresql/16/bin/postgres -D /var/lib/postgresql" {
		t.Errorf("cmdline = %q", pg.Cmdline)
	}
	if snap.CollectMS < 0 {
		t.Error("negative collection time")
	}
	if snap.Verdict != model.SevCrit {
		t.Errorf("verdict = %q, expected crit", snap.Verdict)
	}
}

// TestIssuesAreDetected is the test that matters most: it asserts that the
// panel actually notices the problems the fixture was built to contain.
func TestIssuesAreDetected(t *testing.T) {
	c := fixture(t, Options{}, nil)
	snap := c.Collect(context.Background())

	want := map[string]model.Severity{
		"memory.critical":     model.SevCrit,
		"memory.swap_high":    model.SevWarn,
		"cpu.load_critical":   model.SevCrit,
		"thermal.high":        model.SevWarn,
		"network.offline":     model.SevCrit,
		"port.sensitive":      model.SevCrit,
		"systemd.unavailable": model.SevInfo,
	}
	got := make(map[string]model.Severity, len(snap.Issues))
	for _, i := range snap.Issues {
		got[i.ID] = i.Severity
	}
	for id, sev := range want {
		if got[id] != sev {
			t.Errorf("issue %q: got severity %q, want %q (all issues: %v)", id, got[id], sev, keys(got))
		}
	}
	// Findings must be ordered worst first.
	for i := 1; i < len(snap.Issues); i++ {
		if snap.Issues[i-1].Severity.Rank() < snap.Issues[i].Severity.Rank() {
			t.Fatalf("issues are not sorted by severity: %v", keys(got))
		}
	}
}

func TestDiskIssues(t *testing.T) {
	// A mount point that does not exist cannot be measured, so it must be
	// skipped rather than reported with invented numbers; one that does
	// exist must carry real capacity figures.
	//
	// DataPath maps the mount point into the fixture root, so the fixture
	// directory itself stands in for the mounted filesystem.
	c := fixture(t, Options{}, map[string]string{
		"proc/mounts": "/dev/nvme0n1p2 / ext4 rw 0 0\n/dev/sdz1 /gone xfs rw 0 0\n",
	})
	snap := c.Collect(context.Background())

	var found bool
	for _, d := range snap.Disks {
		if d.Mount == "/gone" {
			t.Errorf("nonexistent mount /gone should have been skipped: %+v", d)
		}
		if d.Mount == "/" {
			found = true
			if d.Total == 0 {
				t.Errorf("capacity not measured for /: %+v", d)
			}
		}
	}
	if !found {
		t.Fatalf("root filesystem missing from %+v", snap.Disks)
	}
}

func TestDiskFullProducesIssue(t *testing.T) {
	// The thresholds are asserted directly here, because a test cannot
	// make a real filesystem 95% full.
	cases := []struct {
		usedPct float64
		wantID  string
	}{
		{70, ""},
		{90, "disk.high"},
		{97, "disk.critical"},
	}
	for _, tc := range cases {
		s := &model.Snapshot{Disks: []model.Disk{{
			Mount: "/data", Device: "/dev/sdb1", Total: 1000, Used: uint64(tc.usedPct * 10), UsedPct: tc.usedPct,
		}}}
		issues := diskIssues(s)
		var got string
		if len(issues) > 0 {
			got = issues[0].ID
		}
		if got != tc.wantID {
			t.Errorf("at %v%% full: got issue %q, want %q", tc.usedPct, got, tc.wantID)
		}
	}
}

func TestInodeExhaustionProducesIssue(t *testing.T) {
	s := &model.Snapshot{Disks: []model.Disk{{
		Mount: "/var", Device: "/dev/sdb1", Total: 1000, UsedPct: 10, InodesPct: 96,
	}}}
	issues := diskIssues(s)
	if len(issues) != 1 || issues[0].ID != "disk.inodes_critical" {
		t.Fatalf("inode exhaustion not detected: %+v", issues)
	}
}

func TestSecondSampleProducesRates(t *testing.T) {
	c := fixture(t, Options{}, nil)
	first := c.Collect(context.Background())
	if first.CPU.Usage != 0 {
		t.Errorf("first sample should not report CPU usage, got %v", first.CPU.Usage)
	}

	// Advance the clock and the kernel counters: 100 jiffies of extra
	// work and 900 of extra idle means about 10% busy.
	now := time.Now()
	c.opts.Now = func() time.Time { return now.Add(2 * time.Second) }
	writeFile(t, c, "proc/stat", `cpu  1100 20 300 4900 500 5 5 0 0 0
cpu0 550 10 150 2450 250 2 3 0 0 0
cpu1 550 10 150 2450 250 3 2 0 0 0
`)
	writeFile(t, c, "proc/net/dev", `Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
    lo: 1000 10 0 0 0 0 0 0 1000 10 0 0 0 0 0 0
  eth0: 55557555 44444 1 2 0 0 0 0 66669666 33333 3 4 0 5 0 0
`)
	second := c.Collect(context.Background())
	if second.CPU.Usage <= 0 || second.CPU.Usage > 30 {
		t.Errorf("second sample CPU usage = %v, want roughly 10%%", second.CPU.Usage)
	}
	if len(second.CPU.Cores) != 2 || second.CPU.Cores[0] <= 0 {
		t.Errorf("per-core usage not reported: %+v", second.CPU.Cores)
	}
	// 2000 extra bytes over 2 seconds is 1000 bytes/s.
	if second.Net.RxBps < 500 || second.Net.RxBps > 2000 {
		t.Errorf("rx rate = %v, want about 1000 bytes/s", second.Net.RxBps)
	}
}

func TestCollectorSurvivesGarbageCounters(t *testing.T) {
	// A collector that panics on malformed input takes the whole panel
	// down. It must degrade into an issue instead.
	c := fixture(t, Options{}, map[string]string{
		"proc/stat":    "cpu nonsense here\n",
		"proc/net/dev": "garbage\n",
		"proc/mounts":  "garbage\n",
	})
	snap := c.Collect(context.Background())
	if snap.Time == 0 {
		t.Fatal("snapshot was not produced for malformed input")
	}
}

func TestDisabledSystemdAndDockerReportWhy(t *testing.T) {
	c := fixture(t, Options{}, nil)
	snap := c.Collect(context.Background())
	if snap.Services.Systemd.Available {
		t.Error("systemd should be unavailable when no client is configured")
	}
	if snap.Services.Systemd.Error == "" {
		t.Error("an unavailable systemd must explain itself")
	}
	if snap.Services.Docker.Available || snap.Services.Docker.Error == "" {
		t.Errorf("docker should be unavailable and explained: %+v", snap.Services.Docker)
	}
}

func writeFile(t *testing.T, c *Collector, name, content string) {
	t.Helper()
	full := filepath.Join(c.root.Dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func keys(m map[string]model.Severity) []string {
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+"="+string(v))
	}
	return out
}

func TestBookkeepingMountsAreSkipped(t *testing.T) {
	// Kernel and session bookkeeping has no block device behind it and is
	// not worth a line in the interface; a real disk mounted under /run
	// still is.
	cases := []struct {
		device, point string
		want          bool
	}{
		{"/dev/nvme0n1p2", "/", false},
		{"/dev/sdb1", "/data", false},
		{"/dev/sdb1", "/run/media/dev/usb", false},
		{"overlay", "/", false},
		{"run", "/run", true},
		{"tmpfs", "/run/user/1000", true},
		{"shm", "/dev/shm", true},
		{"devpts", "/dev/pts", true},
		{"sysfs", "/sys/fs/cgroup", true},
		{"proc", "/proc/sys/fs/binfmt_misc", true},
	}
	for _, tc := range cases {
		if got := isBookkeeping(tc.device, tc.point); got != tc.want {
			t.Errorf("isBookkeeping(%q, %q) = %v, want %v", tc.device, tc.point, got, tc.want)
		}
	}
}

func TestReadOnlyWarningOnlyForRealDevices(t *testing.T) {
	// A read-only tmpfs is ordinary; a read-only ext4 on a device is a
	// signal. The collector must not cry wolf about the first.
	c := fixture(t, Options{}, map[string]string{
		"proc/mounts": "/dev/nvme0n1p2 / ext4 rw 0 0\n" +
			"tmpfs /tmp tmpfs ro 0 0\n",
	})
	snap := c.Collect(context.Background())
	for _, d := range snap.Disks {
		if d.FSType == "tmpfs" && d.ReadOnly {
			t.Errorf("a read-only tmpfs should not be flagged: %+v", d)
		}
	}
}

// TestExplainDockerNamesTheRealProblem covers the distinction that cost an
// afternoon: "the daemon is not reachable" is technically true of a
// permission problem and sends the reader looking in the wrong place. The
// socket is there, the daemon is running, and the panel simply may not open
// it, which is the default state of a fresh install.
func TestExplainDockerNamesTheRealProblem(t *testing.T) {
	const sock = "/var/run/docker.sock"
	cases := []struct {
		name string
		err  error
		want []string
	}{
		{
			name: "nothing listening",
			err:  errors.New("dial unix " + sock + ": connect: no such file or directory"),
			want: []string{"not running", sock},
		},
		{
			name: "the panel may not open it",
			err:  errors.New("dial unix " + sock + ": connect: permission denied"),
			want: []string{"permission denied", "docker group"},
		},
		{
			name: "anything else",
			err:  errors.New("dial unix " + sock + ": connect: connection refused"),
			want: []string{"cannot reach", "connection refused"},
		},
	}
	for _, c := range cases {
		got := explainDocker(c.err, sock)
		for _, want := range c.want {
			if !strings.Contains(got, want) {
				t.Errorf("%s: %q does not mention %q", c.name, got, want)
			}
		}
	}
}
