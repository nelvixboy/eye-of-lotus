package collect

import (
	"os"
	"runtime"
	"strconv"
	"strings"

	"github.com/nelvixboy/eye-of-lotus/internal/model"
	"github.com/nelvixboy/eye-of-lotus/internal/proc"
)

// cpuState remembers the previous /proc/stat sample so utilisation can be
// reported as a rate rather than a lifetime average.
type cpuState struct {
	have  bool
	prev  proc.CPUTimes
	cores []proc.CPUTimes
}

func newCPUState() *cpuState { return &cpuState{} }

// collectHost identifies the machine.
func (c *Collector) collectHost() model.SystemInfo {
	info := model.SystemInfo{
		// The hostname is taken from the running system rather than from
		// the proc root, because /proc/sys/kernel/hostname is the same
		// value and this keeps the fixture tests honest.
		Hostname: hostname(),
		CPUs:     runtime.NumCPU(),
		Kernel:   kernelRelease(c.root),
		Arch:     runtime.GOARCH,
	}
	if b, err := c.root.ReadFile("etc/os-release"); err == nil {
		info.OS = osReleaseField(string(b), "PRETTY_NAME")
	}
	if info.OS == "" {
		if b, err := c.root.ReadFile("proc/version"); err == nil {
			info.OS = strings.TrimSpace(string(b))
		}
	}
	info.Virtualization = virtualization(c.root)
	return info
}

func hostname() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "unknown"
	}
	return h
}

func osReleaseField(content, key string) string {
	for _, line := range strings.Split(content, "\n") {
		k, v, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(k) != key {
			continue
		}
		return strings.Trim(strings.TrimSpace(v), `"'`)
	}
	return ""
}

// kernelRelease reads the kernel version, preferring /proc/sys/kernel/osrelease
// because it has no trailing build metadata.
func kernelRelease(r *proc.Root) string {
	if b, err := r.ReadFile("proc/sys/kernel/osrelease"); err == nil {
		return strings.TrimSpace(string(b))
	}
	return ""
}

// virtualization sniffs the usual tell-tale files. It is best effort: the
// panel reports "unknown" rather than guessing.
func virtualization(r *proc.Root) string {
	if b, err := r.ReadFile("proc/1/environ"); err == nil && strings.Contains(string(b), "container=") {
		for _, kv := range strings.Split(string(b), "\x00") {
			if v, ok := strings.CutPrefix(kv, "container="); ok && v != "" {
				return v
			}
		}
	}
	if _, err := r.StatFile("sys/class/dmi/id/product_name"); err == nil {
		if b, err := r.ReadFile("sys/class/dmi/id/product_name"); err == nil {
			name := strings.ToLower(strings.TrimSpace(string(b)))
			switch {
			case strings.Contains(name, "kvm"), strings.Contains(name, "qemu"):
				return "kvm"
			case strings.Contains(name, "vmware"):
				return "vmware"
			case strings.Contains(name, "virtualbox"):
				return "virtualbox"
			case strings.Contains(name, "xen"):
				return "xen"
			}
		}
	}
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return "docker"
	}
	return "none"
}

// collectHardware fills the CPU and memory parts of the snapshot.
func (c *Collector) collectHardware() (model.CPU, model.Memory) {
	var cpu model.CPU
	var mem model.Memory

	stat, err := c.root.ReadCPUStat()
	if err == nil {
		cpu.Cores, cpu.Usage, cpu.Iowait, cpu.Steal = c.cpu.rates(stat)
	}
	cpu.Load1, cpu.Load5, cpu.Load15 = c.root.LoadAvg()
	cpu.Model = cpuModel(c.root)
	cpu.MHz = cpuMHz(c.root)

	if mi, err := c.root.ReadMemInfo(); err == nil {
		used := uint64(0)
		if mi.Total > mi.Available {
			used = mi.Total - mi.Available
		}
		mem = model.Memory{
			Total:     mi.Total,
			Free:      mi.Free,
			Available: mi.Available,
			Buffers:   mi.Buffers,
			Cached:    mi.Cached + mi.SReclaimable,
			Used:      used,
			SwapTotal: mi.SwapTotal,
			SwapUsed:  mi.SwapTotal - mi.SwapFree,
		}
		mem.UsedPct = proc.Percent(float64(mem.Used), float64(mem.Total))
		mem.SwapPct = proc.Percent(float64(mem.SwapUsed), float64(mem.SwapTotal))
	}
	return cpu, mem
}

// rates converts cumulative jiffies into usage percentages.
func (s *cpuState) rates(stat proc.CPUStat) (cores []float64, total, iowait, steal float64) {
	if !s.have {
		// The first sample has no previous reading to subtract, so only
		// the per-core slice is sized; the UI shows a dash until the
		// second sample arrives.
		s.have = true
		s.prev = stat.Total
		s.cores = stat.Cores
		return make([]float64, len(stat.Cores)), 0, 0, 0
	}
	totalDelta := stat.Total.Total() - s.prev.Total()
	total, iowait, steal = usage(s.prev, stat.Total, totalDelta)

	cores = make([]float64, len(stat.Cores))
	for i := range stat.Cores {
		if i >= len(s.cores) {
			break
		}
		d := stat.Cores[i].Total() - s.cores[i].Total()
		if d == 0 {
			// A counter that did not move (hot-unplugged core) reports
			// as idle rather than dividing by zero.
			continue
		}
		cores[i], _, _ = usage(s.cores[i], stat.Cores[i], d)
	}
	s.prev = stat.Total
	s.cores = stat.Cores
	return cores, total, iowait, steal
}

// usage computes busy, iowait and steal percentages for one delta window.
func usage(prev, cur proc.CPUTimes, delta uint64) (busy, iowait, steal float64) {
	if delta == 0 {
		return 0, 0, 0
	}
	busy = float64((cur.Busy() - prev.Busy())) / float64(delta) * 100
	iowait = float64(cur.Iowait-prev.Iowait) / float64(delta) * 100
	steal = float64(cur.Steal-prev.Steal) / float64(delta) * 100
	return round2(clamp(busy, 0, 100)), round2(clamp(iowait, 0, 100)), round2(clamp(steal, 0, 100))
}

func pad(v []float64) []float64 { return v }

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func round2(v float64) float64 {
	return float64(int64(v*100+0.5)) / 100
}

// cpuModel reads the first "model name" line from /proc/cpuinfo.
func cpuModel(r *proc.Root) string {
	b, err := r.ReadFile("proc/cpuinfo")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		k, v, ok := strings.Cut(line, ":")
		if ok && strings.TrimSpace(k) == "model name" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// cpuMHz averages the per-core clock readings, in gigahertz.
func cpuMHz(r *proc.Root) float64 {
	b, err := r.ReadFile("proc/cpuinfo")
	if err != nil {
		return 0
	}
	sum, n := 0.0, 0
	for _, line := range strings.Split(string(b), "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(k) != "cpu MHz" {
			continue
		}
		f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		if err != nil {
			continue
		}
		sum += f
		n++
	}
	if n == 0 {
		return 0
	}
	return round2(sum / float64(n) / 1000)
}
