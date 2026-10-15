package collect

import (
	"os/user"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/nelvixboy/eye-of-lotus/internal/model"
	"github.com/nelvixboy/eye-of-lotus/internal/proc"
)

// procState remembers the previous CPU tick total so that a process's CPU
// usage can be expressed as a share of the machine, like top does.
type procState struct {
	have      bool
	at        time.Time
	totalTick uint64
	ticks     map[int]uint64
}

func newProcState() *procState {
	return &procState{ticks: make(map[int]uint64, 256)}
}

// collectProcesses returns every process on the machine, sorted by CPU
// usage and then by memory. The caller decides how many to publish.
func (c *Collector) collectProcesses(now int64, memTotal uint64) ([]model.Process, int) {
	stat, err := c.root.ReadCPUStat()
	if err != nil {
		return nil, 0
	}
	totalTicks := stat.Total.Total()

	pids, err := c.root.PIDs()
	if err != nil {
		return nil, 0
	}

	c.procsMu.Lock()
	ps := c.procs
	if ps == nil {
		ps = newProcState()
		c.procs = ps
	}
	elapsed := 0.0
	if ps.have {
		elapsed = c.opts.Now().Sub(ps.at).Seconds()
	}
	totalDelta := totalTicks - ps.totalTick
	c.procsMu.Unlock()

	boot := c.root.BootTime(now)
	users := newUserCache()

	out := make([]model.Process, 0, len(pids))
	ticks := make(map[int]uint64, len(pids))
	for _, pid := range pids {
		st, err := c.root.ReadProcStat(pid)
		if err != nil {
			// The process vanished between listing and reading, which is
			// completely normal on a busy machine.
			continue
		}
		ticks[pid] = st.CPUTicks
		p := model.Process{
			PID:     st.PID,
			PPID:    st.PPID,
			Name:    st.Comm,
			State:   st.StateWord(),
			Threads: st.Threads,
		}
		if st.StartTicks > 0 {
			p.Started = (boot + int64(st.StartTicks/proc.ClockTicks)) * 1000
			p.Uptime = float64(now) - float64(boot) - float64(st.StartTicks)/proc.ClockTicks
			if p.Uptime < 0 {
				p.Uptime = 0
			}
		}
		if rss, ok := c.root.ReadStatmRSS(pid); ok {
			p.RSS = rss
		} else {
			p.RSS = st.RSSPages * 4096
		}
		p.MemPct = proc.Percent(float64(p.RSS), float64(memTotal))
		if uid, ok := c.root.ReadStatusUID(pid); ok {
			p.User = users.name(uid)
		}

		// CPU is only meaningful once there is a previous sample and at
		// least a little wall time between the two.
		if ps.have && elapsed > 0.05 && totalDelta > 0 {
			if prev, ok := ps.ticks[pid]; ok && st.CPUTicks >= prev {
				cpuTicks := float64(st.CPUTicks - prev)
				// A single busy thread on a four core machine reads as
				// 25%, the same convention top uses.
				p.CPU = round2(cpuTicks / float64(totalDelta) * 100)
			}
		}
		if len(p.Cmdline) == 0 {
			p.Cmdline = c.root.ReadCmdline(pid)
		}
		if p.Cmdline == "" {
			p.Cmdline = "[" + st.Comm + "]"
		}
		if len(p.Cmdline) > 300 {
			p.Cmdline = p.Cmdline[:300] + "..."
		}
		out = append(out, p)
	}

	c.procsMu.Lock()
	ps.have = true
	ps.at = c.opts.Now()
	ps.totalTick = totalTicks
	ps.ticks = ticks
	c.procsMu.Unlock()

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].CPU != out[j].CPU {
			return out[i].CPU > out[j].CPU
		}
		if out[i].RSS != out[j].RSS {
			return out[i].RSS > out[j].RSS
		}
		return out[i].PID < out[j].PID
	})
	return out, len(pids)
}

// userCache resolves UIDs to names once per sample instead of once per
// process, because the lookup crosses into NSS and can be slow.
type userCache struct {
	mu    sync.Mutex
	names map[int]string
}

func newUserCache() *userCache {
	return &userCache{names: make(map[int]string, 16)}
}

func (u *userCache) name(uid int) string {
	u.mu.Lock()
	defer u.mu.Unlock()
	if n, ok := u.names[uid]; ok {
		return n
	}
	name := strconv.Itoa(uid)
	if usr, err := user.LookupId(name); err == nil && usr.Username != "" {
		name = usr.Username
	}
	u.names[uid] = name
	return name
}
