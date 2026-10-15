package proc

import (
	"os"
	"path/filepath"
	"testing"
)

// writeFixture builds a small fake /proc tree under a temporary directory
// so that the parsers can be tested against known content.
func writeFixture(t *testing.T, files map[string]string) *Root {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		full := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", full, err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", full, err)
		}
	}
	root, err := Open(dir)
	if err != nil {
		t.Fatalf("open root: %v", err)
	}
	t.Cleanup(func() { root.Close() })
	return root
}

const statFixture = `cpu  100 20 30 400 10 5 5 0 0 0
cpu0 50 10 15 200 5 2 3 0 0 0
cpu1 50 10 15 200 5 3 2 0 0 0
intr 12345 0 0
ctxt 999
btime 1700000000
processes 4242
`

func TestReadCPUStat(t *testing.T) {
	root := writeFixture(t, map[string]string{"proc/stat": statFixture})
	got, err := root.ReadCPUStat()
	if err != nil {
		t.Fatalf("ReadCPUStat: %v", err)
	}
	if got.Total.User != 100 || got.Total.System != 30 || got.Total.Idle != 400 {
		t.Errorf("total counters wrong: %+v", got.Total)
	}
	if got.Total.Iowait != 10 || got.Total.IRQ != 5 || got.Total.SoftIRQ != 5 {
		t.Errorf("extended counters wrong: %+v", got.Total)
	}
	if len(got.Cores) != 2 {
		t.Fatalf("expected 2 cores, got %d", len(got.Cores))
	}
	if got.Cores[1].Idle != 200 || got.Cores[1].SoftIRQ != 2 {
		t.Errorf("per-core counters wrong: %+v", got.Cores[1])
	}
	// The parser must stop at the first non-cpu line.
	if got.Total.Total() != 570 {
		t.Errorf("total jiffies = %d, want 570", got.Total.Total())
	}
}

func TestReadCPUStatShortLines(t *testing.T) {
	// Old kernels report fewer columns; the missing ones must read as zero
	// rather than panicking.
	root := writeFixture(t, map[string]string{"proc/stat": "cpu  10 0 5 100\ncpu0 10 0 5 100\n"})
	got, err := root.ReadCPUStat()
	if err != nil {
		t.Fatalf("ReadCPUStat: %v", err)
	}
	if got.Total.Iowait != 0 || got.Total.Steal != 0 {
		t.Errorf("missing columns should be zero: %+v", got.Total)
	}
}

func TestLoadAvgAndUptime(t *testing.T) {
	root := writeFixture(t, map[string]string{
		"proc/loadavg": "0.82 0.56 0.42 3/1861 13\n",
		"proc/uptime":  "920523.45 1234567.89\n",
	})
	l1, l5, l15 := root.LoadAvg()
	if l1 != 0.82 || l5 != 0.56 || l15 != 0.42 {
		t.Errorf("load averages = %v %v %v", l1, l5, l15)
	}
	if up := root.Uptime(); up != 920523.45 {
		t.Errorf("uptime = %v, want 920523.45", up)
	}
	if boot := root.BootTime(1700001000); boot != 1700001000-920523 {
		t.Errorf("boot time = %d", boot)
	}
}

func TestReadMemInfo(t *testing.T) {
	root := writeFixture(t, map[string]string{"proc/meminfo": `MemTotal:       16291072 kB
MemFree:          553664 kB
MemAvailable:    9131032 kB
Buffers:          123456 kB
Cached:          4567890 kB
SwapTotal:       2097152 kB
SwapFree:        1048576 kB
`})
	m, err := root.ReadMemInfo()
	if err != nil {
		t.Fatalf("ReadMemInfo: %v", err)
	}
	if m.Total != 16291072*1024 {
		t.Errorf("total = %d", m.Total)
	}
	if m.Available != 9131032*1024 {
		t.Errorf("available = %d", m.Available)
	}
	if m.SwapTotal-m.SwapFree != 1048576*1024 {
		t.Errorf("swap used = %d", m.SwapTotal-m.SwapFree)
	}
}

func TestReadDiskStats(t *testing.T) {
	root := writeFixture(t, map[string]string{"proc/diskstats": ` 259       0 nvme0n1 1000 50 20480 300 2000 60 40960 500 0 800 900 10 0
   8       0 sda 10 0 80 5 20 0 160 7 0 12 8 0 0
bad line
`})
	stats, err := root.ReadDiskStats()
	if err != nil {
		t.Fatalf("ReadDiskStats: %v", err)
	}
	nvme, ok := stats["nvme0n1"]
	if !ok {
		t.Fatalf("nvme0n1 missing: %+v", stats)
	}
	if nvme.ReadsDone != 1000 || nvme.ReadSectors != 20480 || nvme.WriteSectors != 40960 {
		t.Errorf("nvme counters wrong: %+v", nvme)
	}
	if _, ok := stats["sda"]; !ok {
		t.Error("sda missing")
	}
}

func TestReadNetDev(t *testing.T) {
	root := writeFixture(t, map[string]string{"proc/net/dev": `Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
    lo: 1000 10 0 0 0 0 0 0 1000 10 0 0 0 0 0 0
  eth0: 55555555 44444 1 2 0 0 0 0 66666666 33333 3 4 0 5 0 0
`})
	devs, err := root.ReadNetDev()
	if err != nil {
		t.Fatalf("ReadNetDev: %v", err)
	}
	eth, ok := devs["eth0"]
	if !ok {
		t.Fatalf("eth0 missing: %+v", devs)
	}
	if eth.RxBytes != 55555555 || eth.TxBytes != 66666666 {
		t.Errorf("byte counters wrong: %+v", eth)
	}
	if eth.RxErrs != 1 || eth.RxDrops != 2 || eth.TxErrs != 3 || eth.TxDrops != 4 {
		t.Errorf("error counters wrong: %+v", eth)
	}
	if lo, ok := devs["lo"]; !ok || lo.RxBytes != 1000 {
		t.Errorf("loopback wrong: %+v", lo)
	}
}

func TestReadSocketsListen(t *testing.T) {
	root := writeFixture(t, map[string]string{
		// Port 22 (0x16) on 0.0.0.0, listening.
		"proc/net/tcp": `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000:0016 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 12345 1 0000000000000000 100 0 0 10 0
   1: 0100007F:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 54321 1 0000000000000000 100 0 0 10 0
`,
		// Port 53 (0x35) on 127.0.0.1, UDP.
		"proc/net/udp": `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
  10: 0100007F:0035 00000000:0000 07 00000000:00000000 00:00000000 00000000     0        0 77777 1 0000000000000000 0
`,
	})
	socks := root.ReadSockets()
	if len(socks) != 3 {
		t.Fatalf("expected 3 sockets, got %d: %+v", len(socks), socks)
	}
	first := socks[0]
	if first.Port != 22 || first.LocalIP != "0.0.0.0" || first.State != "LISTEN" {
		t.Errorf("first socket wrong: %+v", first)
	}
	if first.Inode != 12345 || first.UID != 0 {
		t.Errorf("inode or uid wrong: %+v", first)
	}
	second := socks[1]
	if second.LocalIP != "127.0.0.1" || second.Port != 8080 {
		t.Errorf("second socket wrong: %+v", second)
	}
	third := socks[2]
	if third.Proto != "udp" || third.Port != 53 {
		t.Errorf("udp socket wrong: %+v", third)
	}
}

func TestParseStatHandlesSpacesInComm(t *testing.T) {
	// A process whose comm contains spaces and parentheses must still parse.
	line := `1234 (weird (name) here) S 1 1234 1234 0 -1 4194560 100 0 0 0 55 66 0 0 20 0 7 0 987654 12345678 321 18446744073709551615`
	st, ok := parseStat(line)
	if !ok {
		t.Fatal("parseStat failed")
	}
	if st.PID != 1234 || st.Comm != "weird (name) here" {
		t.Errorf("pid/comm wrong: %+v", st)
	}
	if st.PPID != 1 || st.State != "S" {
		t.Errorf("ppid/state wrong: %+v", st)
	}
	if st.UTime != 55 || st.STime != 66 || st.CPUTicks != 121 {
		t.Errorf("cpu ticks wrong: %+v", st)
	}
	if st.Threads != 7 || st.StartTicks != 987654 {
		t.Errorf("threads/start wrong: %+v", st)
	}
	if st.RSSPages != 321 {
		t.Errorf("rss pages wrong: %+v", st)
	}
}

func TestProcStateWords(t *testing.T) {
	cases := map[string]string{"R": "running", "S": "sleeping", "D": "uninterruptible", "Z": "zombie", "I": "idle", "?": "unknown"}
	for letter, want := range cases {
		if got := (ProcStat{State: letter}).StateWord(); got != want {
			t.Errorf("state %q = %q, want %q", letter, got, want)
		}
	}
}

func TestReadMountsUnescapesSpaces(t *testing.T) {
	root := writeFixture(t, map[string]string{"proc/mounts": `/dev/sda1 / ext4 rw,relatime 0 0
/dev/sdb1 /mnt/my\040disk xfs rw 0 0
tmpfs /run tmpfs rw,nosuid 0 0
`})
	mounts, err := root.ReadMounts()
	if err != nil {
		t.Fatalf("ReadMounts: %v", err)
	}
	if len(mounts) != 3 {
		t.Fatalf("expected 3 mounts, got %d", len(mounts))
	}
	if mounts[1].Point != "/mnt/my disk" {
		t.Errorf("escaped space not decoded: %q", mounts[1].Point)
	}
}

func TestPIDsListsOnlyNumericEntries(t *testing.T) {
	root := writeFixture(t, map[string]string{
		"proc/1/stat":  "1 (init) S 0 1 1 0 -1 4194560 0 0 0 0 1 2 0 0 20 0 1 0 100 0 0",
		"proc/99/stat": "99 (bash) S 1 99 99 0 -1 4194560 0 0 0 0 3 4 0 0 20 0 1 0 200 0 0",
		"proc/cpuinfo": "processor: 0\n",
		"proc/self":    "",
		"proc/uptime":  "10.0 20.0\n",
		"proc/meminfo": "MemTotal: 100 kB\n",
	})
	pids, err := root.PIDs()
	if err != nil {
		t.Fatalf("PIDs: %v", err)
	}
	if len(pids) != 2 || pids[0] != 1 || pids[1] != 99 {
		t.Errorf("pids = %v, want [1 99]", pids)
	}
	st, err := root.ReadProcStat(99)
	if err != nil {
		t.Fatalf("ReadProcStat: %v", err)
	}
	if st.Comm != "bash" || st.CPUTicks != 7 {
		t.Errorf("stat wrong: %+v", st)
	}
}

func TestPercent(t *testing.T) {
	if got := Percent(50, 200); got != 25 {
		t.Errorf("Percent(50,200) = %v, want 25", got)
	}
	if got := Percent(1, 0); got != 0 {
		t.Errorf("Percent with zero denominator = %v, want 0", got)
	}
}
