package collect

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/nelvixboy/eye-of-lotus/internal/model"
	"github.com/nelvixboy/eye-of-lotus/internal/proc"
)

// Thresholds for the issue engine. They are collected here so that a user
// can see, in one place, exactly what counts as a problem.
var thresholds = struct {
	DiskWarn      float64
	DiskCrit      float64
	InodeWarn     float64
	InodeCrit     float64
	MemWarn       float64
	MemCrit       float64
	SwapWarn      float64
	SwapCrit      float64
	LoadWarn      float64
	LoadCrit      float64
	IOWaitWarn    float64
	IOWaitCrit    float64
	LatencyWarnMS float64
	DNSWarnMS     float64
	TempWarnC     float64
	TempCritC     float64
}{
	DiskWarn: 88, DiskCrit: 95,
	InodeWarn: 88, InodeCrit: 95,
	MemWarn: 90, MemCrit: 96,
	SwapWarn: 40, SwapCrit: 80,
	// Load is compared against the number of logical processors.
	LoadWarn: 1.5, LoadCrit: 3.0,
	IOWaitWarn: 25, IOWaitCrit: 50,
	LatencyWarnMS: 400, DNSWarnMS: 500,
	TempWarnC: 80, TempCritC: 95,
}

// interestingPorts are services that should never face the open internet
// without a very good reason.
var interestingPorts = map[int]string{
	2375:  "the unauthenticated Docker API",
	2376:  "the Docker API",
	6379:  "Redis",
	27017: "MongoDB",
	9200:  "Elasticsearch",
	11211: "memcached",
	5432:  "PostgreSQL",
	3306:  "MySQL",
	23:    "Telnet",
	21:    "FTP",
}

// buildIssues runs every check and returns the findings, worst first.
func buildIssues(s *model.Snapshot, opts Options) []model.Issue {
	var out []model.Issue
	out = append(out, memoryIssues(s)...)
	out = append(out, cpuIssues(s)...)
	out = append(out, diskIssues(s)...)
	out = append(out, networkIssues(s)...)
	out = append(out, serviceIssues(s)...)
	out = append(out, portIssues(s)...)
	out = append(out, thermalIssues(s, opts)...)

	sortIssues(out)
	return out
}

func memoryIssues(s *model.Snapshot) []model.Issue {
	var out []model.Issue
	m := s.Memory
	if m.Total == 0 {
		return nil
	}
	switch {
	case m.UsedPct >= thresholds.MemCrit:
		out = append(out, model.Issue{
			ID: "memory.critical", Severity: model.SevCrit,
			Title: "Memory is nearly exhausted", Subject: "RAM",
			Detail: fmt.Sprintf("%s of %s in use (%.1f%%)", humanBytes(m.Used), humanBytes(m.Total), m.UsedPct),
			Hint:   "Find the process at the top of the process table and decide whether it belongs there. The kernel will start killing processes before it swaps itself to death.",
			Value:  m.UsedPct, Threshold: thresholds.MemCrit,
		})
	case m.UsedPct >= thresholds.MemWarn:
		out = append(out, model.Issue{
			ID: "memory.high", Severity: model.SevWarn,
			Title: "Memory usage is high", Subject: "RAM",
			Detail: fmt.Sprintf("%s of %s in use (%.1f%%)", humanBytes(m.Used), humanBytes(m.Total), m.UsedPct),
			Hint:   "Not urgent yet, but worth knowing which service grew.",
			Value:  m.UsedPct, Threshold: thresholds.MemWarn,
		})
	}
	if m.SwapTotal > 0 {
		switch {
		case m.SwapPct >= thresholds.SwapCrit:
			out = append(out, model.Issue{
				ID: "memory.swap_critical", Severity: model.SevCrit,
				Title: "Swap is almost full", Subject: "swap",
				Detail: fmt.Sprintf("%s of %s swapped (%.1f%%)", humanBytes(m.SwapUsed), humanBytes(m.SwapTotal), m.SwapPct),
				Hint:   "A machine that has run out of both RAM and swap will start killing processes.",
				Value:  m.SwapPct, Threshold: thresholds.SwapCrit,
			})
		case m.SwapPct >= thresholds.SwapWarn:
			out = append(out, model.Issue{
				ID: "memory.swap_high", Severity: model.SevWarn,
				Title: "Swap is in use", Subject: "swap",
				Detail: fmt.Sprintf("%s of %s swapped (%.1f%%)", humanBytes(m.SwapUsed), humanBytes(m.SwapTotal), m.SwapPct),
				Hint:   "Heavy swap use means the working set no longer fits in RAM; everything on this machine is slower than it should be.",
				Value:  m.SwapPct, Threshold: thresholds.SwapWarn,
			})
		}
	}
	return out
}

func cpuIssues(s *model.Snapshot) []model.Issue {
	var out []model.Issue
	cores := float64(len(s.CPU.Cores))
	if cores == 0 {
		cores = float64(s.Host.CPUs)
	}
	if cores > 0 {
		ratio := s.CPU.Load1 / cores
		switch {
		case ratio >= thresholds.LoadCrit:
			out = append(out, model.Issue{
				ID: "cpu.load_critical", Severity: model.SevCrit,
				Title: "Load average far exceeds the core count", Subject: "load",
				Detail: fmt.Sprintf("load1 %.2f across %d logical processors (%.2fx)", s.CPU.Load1, int(cores), ratio),
				Hint:   "More runnable work than there are cores. Something is queueing: look at the process table and at disk wait.",
				Value:  round2(ratio), Threshold: thresholds.LoadCrit,
			})
		case ratio >= thresholds.LoadWarn:
			out = append(out, model.Issue{
				ID: "cpu.load_high", Severity: model.SevWarn,
				Title: "Load average is elevated", Subject: "load",
				Detail: fmt.Sprintf("load1 %.2f across %d logical processors (%.2fx)", s.CPU.Load1, int(cores), ratio),
				Hint:   "Sustained load above the core count means requests are waiting.",
				Value:  round2(ratio), Threshold: thresholds.LoadWarn,
			})
		}
	}
	switch {
	case s.CPU.Iowait >= thresholds.IOWaitCrit:
		out = append(out, model.Issue{
			ID: "cpu.iowait_critical", Severity: model.SevCrit,
			Title: "Processors are stalled on disk I/O", Subject: "iowait",
			Detail: fmt.Sprintf("%.1f%% of CPU time is spent waiting for devices", s.CPU.Iowait),
			Hint:   "The bottleneck is storage, not compute. Check the disk throughput in the hardware card and look for a runaway writer.",
			Value:  s.CPU.Iowait, Threshold: thresholds.IOWaitCrit,
		})
	case s.CPU.Iowait >= thresholds.IOWaitWarn:
		out = append(out, model.Issue{
			ID: "cpu.iowait_high", Severity: model.SevWarn,
			Title: "Significant disk wait", Subject: "iowait",
			Detail: fmt.Sprintf("%.1f%% of CPU time is spent waiting for devices", s.CPU.Iowait),
			Hint:   "Worth watching: on a slow disk this is what makes a healthy-looking machine feel dead.",
			Value:  s.CPU.Iowait, Threshold: thresholds.IOWaitWarn,
		})
	}
	if s.CPU.Steal >= 10 {
		out = append(out, model.Issue{
			ID: "cpu.steal", Severity: model.SevWarn,
			Title: "CPU time is being stolen by the hypervisor", Subject: "steal",
			Detail: fmt.Sprintf("%.1f%% of CPU time is stolen", s.CPU.Steal),
			Hint:   "This machine is a virtual guest and its host is oversubscribed. Nothing you do inside the guest will fix it.",
			Value:  s.CPU.Steal, Threshold: 10,
		})
	}
	return out
}

func diskIssues(s *model.Snapshot) []model.Issue {
	var out []model.Issue
	for _, d := range s.Disks {
		switch {
		case d.UsedPct >= thresholds.DiskCrit:
			out = append(out, model.Issue{
				ID: "disk.critical", Severity: model.SevCrit,
				Title: "Filesystem nearly full", Subject: d.Mount,
				Detail: fmt.Sprintf("%s is %.1f%% full: %s free of %s", d.Mount, d.UsedPct, humanBytes(d.Free), humanBytes(d.Total)),
				Hint:   "A full root filesystem breaks logging, databases and the panel itself. Clear logs or old artifacts now.",
				Value:  d.UsedPct, Threshold: thresholds.DiskCrit,
			})
		case d.UsedPct >= thresholds.DiskWarn:
			out = append(out, model.Issue{
				ID: "disk.high", Severity: model.SevWarn,
				Title: "Filesystem filling up", Subject: d.Mount,
				Detail: fmt.Sprintf("%s is %.1f%% full: %s free of %s", d.Mount, d.UsedPct, humanBytes(d.Free), humanBytes(d.Total)),
				Hint:   "Plan the cleanup now rather than at 100%.",
				Value:  d.UsedPct, Threshold: thresholds.DiskWarn,
			})
		}
		if d.InodesPct >= thresholds.InodeCrit {
			out = append(out, model.Issue{
				ID: "disk.inodes_critical", Severity: model.SevCrit,
				Title: "Inodes exhausted", Subject: d.Mount,
				Detail: fmt.Sprintf("%s has used %.1f%% of its inodes while holding %s of free space", d.Mount, d.InodesPct, humanBytes(d.Free)),
				Hint:   "Space is left but no new file can be created. Look for a directory holding millions of tiny files.",
				Value:  d.InodesPct, Threshold: thresholds.InodeCrit,
			})
		} else if d.InodesPct >= thresholds.InodeWarn {
			out = append(out, model.Issue{
				ID: "disk.inodes_high", Severity: model.SevWarn,
				Title: "Many inodes in use", Subject: d.Mount,
				Detail: fmt.Sprintf("%s has used %.1f%% of its inodes", d.Mount, d.InodesPct),
				Hint:   "Usually a cache, a mail queue or a session directory growing one small file at a time.",
				Value:  d.InodesPct, Threshold: thresholds.InodeWarn,
			})
		}
		if d.ReadOnly && d.Mount != "/" {
			out = append(out, model.Issue{
				ID: "disk.readonly", Severity: model.SevWarn,
				Title: "Filesystem is mounted read-only", Subject: d.Mount,
				Detail: fmt.Sprintf("%s (%s) is mounted read-only", d.Mount, d.Device),
				Hint:   "Either intentional hardening or the kernel remounted it after an I/O error. Check the journal for the reason.",
			})
		}
	}
	return out
}

func networkIssues(s *model.Snapshot) []model.Issue {
	var out []model.Issue
	n := s.Net
	if !n.Online {
		out = append(out, model.Issue{
			ID: "network.offline", Severity: model.SevCrit,
			Title: "No internet connectivity", Subject: "network",
			Detail: "No TCP connection could be established to any probe target (" + n.CheckedVia + ")",
			Hint:   "Check the default route, the interface state below, and whether an upstream firewall is dropping outbound traffic.",
		})
	} else if n.LatencyMS >= thresholds.LatencyWarnMS {
		out = append(out, model.Issue{
			ID: "network.latency", Severity: model.SevWarn,
			Title: "High network latency", Subject: "network",
			Detail: fmt.Sprintf("the connectivity probe took %.0f ms via %s", n.LatencyMS, n.CheckedVia),
			Hint:   "The link works but is slow. Compare with the traffic graph before blaming the provider.",
			Value:  n.LatencyMS, Threshold: thresholds.LatencyWarnMS,
		})
	}
	if n.Online && !n.DNSOK {
		out = append(out, model.Issue{
			ID: "network.dns", Severity: model.SevCrit,
			Title: "DNS resolution is failing", Subject: n.DNSName,
			Detail: "the machine can reach the internet by address but cannot resolve " + n.DNSName,
			Hint:   "Check /etc/resolv.conf and the resolver service. Almost everything that talks to the network will fail while this lasts.",
		})
	} else if n.DNSOK && n.DNSMS >= thresholds.DNSWarnMS {
		out = append(out, model.Issue{
			ID: "network.dns_slow", Severity: model.SevWarn,
			Title: "DNS resolution is slow", Subject: n.DNSName,
			Detail: fmt.Sprintf("resolving %s took %.0f ms", n.DNSName, n.DNSMS),
			Hint:   "Every outbound request pays this cost first.",
			Value:  n.DNSMS, Threshold: thresholds.DNSWarnMS,
		})
	}
	return out
}

func serviceIssues(s *model.Snapshot) []model.Issue {
	var out []model.Issue
	if !s.Services.Systemd.Available {
		out = append(out, model.Issue{
			ID: "systemd.unavailable", Severity: model.SevInfo,
			Title: "systemd is not reachable", Subject: "systemd",
			Detail: strings.TrimSpace(s.Services.Systemd.Error),
			Hint:   "The panel keeps working; unit state simply cannot be shown. On a machine without systemd, turn systemd support off in the configuration.",
		})
	}
	for _, u := range s.Services.Systemd.Units {
		if u.Status == "failed" {
			out = append(out, model.Issue{
				ID: "systemd.failed", Severity: model.SevCrit,
				Title: "Unit failed: " + u.Name, Subject: u.Name,
				Detail: u.Description + " is in state " + u.ActiveState + "/" + u.SubState,
				Hint:   "Read the unit's log with journalctl -u " + u.Name + " -n 50.",
			})
		}
	}
	if s.Services.Docker.Available {
		for _, ct := range s.Services.Docker.Items {
			switch {
			case ct.Health == "unhealthy":
				out = append(out, model.Issue{
					ID: "docker.unhealthy", Severity: model.SevCrit,
					Title: "Container unhealthy: " + ct.Name, Subject: ct.Name,
					Detail: ct.Image + ": the health check keeps failing",
					Hint:   "The container runs but its own health check disagrees. Inspect it with docker inspect --format '{{json .State.Health}}' " + ct.Name,
				})
			case ct.Status == "restarting":
				out = append(out, model.Issue{
					ID: "docker.restarting", Severity: model.SevCrit,
					Title: "Container restart loop: " + ct.Name, Subject: ct.Name,
					Detail: ct.Image + ": the container keeps restarting",
					Hint:   "Read its logs with docker logs --tail 50 " + ct.Name,
				})
			case ct.Status == "dead":
				out = append(out, model.Issue{
					ID: "docker.dead", Severity: model.SevWarn,
					Title: "Container is dead: " + ct.Name, Subject: ct.Name,
					Detail: ct.Image + ": the container is in the dead state",
					Hint:   "A dead container must be removed before it can be recreated.",
				})
			}
		}
	}
	return out
}

func portIssues(s *model.Snapshot) []model.Issue {
	var out []model.Issue
	for _, l := range s.Net.Listening {
		if !l.Exposed {
			continue
		}
		if what, ok := interestingPorts[l.Port]; ok {
			owner := l.Process
			if owner == "" {
				owner = "an unidentified process"
			}
			out = append(out, model.Issue{
				ID: "port.sensitive", Severity: model.SevCrit,
				Title: fmt.Sprintf("Port %d is open to the network", l.Port), Subject: fmt.Sprintf("%s/%d", l.Proto, l.Port),
				Detail: fmt.Sprintf("%s is listening on every interface and looks like %s", owner, what),
				Hint:   "Bind it to 127.0.0.1, put it behind a firewall, or enable authentication before someone else finds it.",
			})
		}
	}
	return out
}

func thermalIssues(s *model.Snapshot, opts Options) []model.Issue {
	temp := s.TempC
	if temp <= 0 {
		return nil
	}
	switch {
	case temp >= thresholds.TempCritC:
		return []model.Issue{{
			ID: "thermal.critical", Severity: model.SevCrit,
			Title: "Temperature is critical", Subject: "thermal",
			Detail: fmt.Sprintf("a thermal zone reports %.1f C", temp),
			Hint:   "The kernel will throttle or shut the machine down. Check airflow and the fan.",
			Value:  temp, Threshold: thresholds.TempCritC,
		}}
	case temp >= thresholds.TempWarnC:
		return []model.Issue{{
			ID: "thermal.high", Severity: model.SevWarn,
			Title: "Temperature is high", Subject: "thermal",
			Detail: fmt.Sprintf("a thermal zone reports %.1f C", temp),
			Hint:   "Sustained heat shortens the life of every component in the case.",
			Value:  temp, Threshold: thresholds.TempWarnC,
		}}
	}
	return nil
}

// inspectProcesses looks for process level problems among the top
// processes and reports how many zombies exist in total.
func inspectProcesses(procs []model.Process, total int) []model.Issue {
	var out []model.Issue
	zombies := 0
	for _, p := range procs {
		if p.State == "zombie" {
			zombies++
		}
	}
	if zombies > 0 {
		out = append(out, model.Issue{
			ID: "proc.zombie", Severity: model.SevWarn,
			Title: "Zombie processes are present", Subject: "processes",
			Detail: fmt.Sprintf("%d zombie process(es) among the busiest %d of %d total", zombies, len(procs), total),
			Hint:   "A zombie holds no memory, but its parent has not reaped it. If the count keeps growing, the parent has a bug.",
			Value:  float64(zombies),
		})
	}
	return out
}

// readTemperature returns the highest reading found in the thermal zones.
func readTemperature(opts Options) (float64, bool) {
	root, err := proc.Open(opts.ProcRoot)
	if err != nil {
		return 0, false
	}
	defer root.Close()
	zones, err := root.ReadDirNames("sys/class/thermal")
	if err != nil {
		return 0, false
	}
	best := 0.0
	found := false
	for _, z := range zones {
		if !strings.HasPrefix(z, "thermal_zone") {
			continue
		}
		b, err := root.ReadFile("sys/class/thermal/" + z + "/temp")
		if err != nil {
			continue
		}
		milli, err := strconv.ParseFloat(strings.TrimSpace(string(b)), 64)
		if err != nil {
			continue
		}
		c := milli / 1000
		// Reject implausible readings: some zones report nonsense when the
		// driver does not implement the sensor properly.
		if c < 1 || c > 200 {
			continue
		}
		if !found || c > best {
			best, found = c, true
		}
	}
	return round2(best), found
}

// sortIssues orders findings so that the most urgent come first and ties
// are broken alphabetically, which keeps the list stable between samples.
func sortIssues(issues []model.Issue) {
	for i := 1; i < len(issues); i++ {
		for j := i; j > 0; j-- {
			a, b := issues[j-1], issues[j]
			if a.Severity.Rank() > b.Severity.Rank() ||
				(a.Severity.Rank() == b.Severity.Rank() && a.ID <= b.ID) {
				break
			}
			issues[j-1], issues[j] = issues[j], issues[j-1]
		}
	}
}

// humanBytes formats a byte count for messages shown to a human.
func humanBytes(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	units := []string{"KiB", "MiB", "GiB", "TiB", "PiB"}
	v := float64(b)
	i := -1
	for v >= unit && i < len(units)-1 {
		v /= unit
		i++
	}
	return fmt.Sprintf("%.1f %s", v, units[i])
}
