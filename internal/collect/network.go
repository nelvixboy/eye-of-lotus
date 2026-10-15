package collect

import (
	"context"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nelvixboy/eye-of-lotus/internal/model"
	"github.com/nelvixboy/eye-of-lotus/internal/proc"
)

// netState remembers the previous interface counters so throughput can be
// reported per second.
type netState struct {
	have bool
	at   time.Time
	devs map[string]proc.NetDevStat
}

func newNetState() *netState { return &netState{} }

// probeState throttles the internet and DNS checks, which open real
// sockets and should not run on every sample.
type probeState struct {
	last   time.Time
	online bool
	via    string
	ms     float64
	dnsOK  bool
	dnsMS  float64
}

func newProbeState() *probeState { return &probeState{} }

// collectNetwork reports connectivity, throughput, interfaces and listening
// sockets.
func (c *Collector) collectNetwork(ctx context.Context, now time.Time) model.Network {
	var out model.Network

	out.Ifaces = c.ifaces()
	for _, i := range out.Ifaces {
		out.RxBps += i.RxBps
		out.TxBps += i.TxBps
		out.RxTotal += i.RxTotal
		out.TxTotal += i.TxTotal
	}

	out.Online, out.CheckedVia, out.LatencyMS, out.DNSOK, out.DNSMS, out.DNSName = c.connectivity(ctx, now)
	out.Listening = c.listening()
	return out
}

// ifaces builds the per-interface list, including rates since the previous
// sample.
func (c *Collector) ifaces() []model.Iface {
	devs, err := c.root.ReadNetDev()
	if err != nil {
		return nil
	}
	now := c.opts.Now()
	rates := c.net.rates(devs, now)

	filter := make(map[string]bool, len(c.opts.Ifaces))
	for _, name := range c.opts.Ifaces {
		filter[name] = true
	}
	names := make([]string, 0, len(devs))
	for name := range devs {
		if name == "lo" && len(filter) == 0 {
			// The loopback interface is noise on a busy host; it is still
			// reported when explicitly requested.
			continue
		}
		if len(filter) > 0 && !filter[name] {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)

	out := make([]model.Iface, 0, len(names))
	for _, name := range names {
		d := devs[name]
		iface := model.Iface{
			Name:    name,
			RxTotal: d.RxBytes,
			TxTotal: d.TxBytes,
			RxErrs:  d.RxErrs,
			TxErrs:  d.TxErrs,
			RxDrops: d.RxDrops,
			TxDrops: d.TxDrops,
			Up:      interfaceIsUp(name),
			MAC:     interfaceMAC(name),
			Addrs:   interfaceAddrs(name),
		}
		if r, ok := rates[name]; ok {
			iface.RxBps, iface.TxBps = r.rx, r.tx
		}
		out = append(out, iface)
	}
	return out
}

// rates turns cumulative byte counters into per-second throughput.
func (s *netState) rates(cur map[string]proc.NetDevStat, now time.Time) map[string]struct{ rx, tx float64 } {
	out := make(map[string]struct{ rx, tx float64 }, len(cur))
	if !s.have {
		s.have = true
		s.at = now
		s.devs = cur
		return out
	}
	elapsed := now.Sub(s.at).Seconds()
	if elapsed <= 0 {
		elapsed = 1
	}
	for name, cv := range cur {
		pv, ok := s.devs[name]
		if !ok {
			continue
		}
		var rx, tx float64
		if cv.RxBytes >= pv.RxBytes {
			rx = float64(cv.RxBytes-pv.RxBytes) / elapsed
		}
		if cv.TxBytes >= pv.TxBytes {
			tx = float64(cv.TxBytes-pv.TxBytes) / elapsed
		}
		out[name] = struct{ rx, tx float64 }{round2(rx), round2(tx)}
	}
	s.at = now
	s.devs = cur
	return out
}

// connectivity probes the internet and DNS, at most once per ProbeInterval.
func (c *Collector) connectivity(ctx context.Context, now time.Time) (online bool, via string, ms float64, dnsOK bool, dnsMS float64, dnsName string) {
	p := c.probe
	if !p.last.IsZero() && now.Sub(p.last) < c.opts.ProbeInterval {
		return p.online, p.via, p.ms, p.dnsOK, p.dnsMS, c.opts.DNSName
	}
	p.last = now
	p.online, p.via, p.ms = probeTCP(ctx, c.opts.ProbeTargets)
	p.dnsOK, p.dnsMS = probeDNS(ctx, c.opts.DNSName)
	return p.online, p.via, p.ms, p.dnsOK, p.dnsMS, c.opts.DNSName
}

// probeTCP dials the first reachable target and reports how long it took.
// A TCP connect is used instead of ICMP because it needs no privileges and
// works behind the NAT that most servers sit behind.
func probeTCP(ctx context.Context, targets []string) (bool, string, float64) {
	for _, target := range targets {
		dctx, cancel := context.WithTimeout(ctx, 2500*time.Millisecond)
		start := time.Now()
		d := net.Dialer{}
		conn, err := d.DialContext(dctx, "tcp", target)
		elapsed := time.Since(start)
		cancel()
		if err == nil {
			conn.Close()
			return true, "tcp:" + target, round2(float64(elapsed.Microseconds()) / 1000)
		}
	}
	return false, "tcp:" + strings.Join(targets, ","), 0
}

// probeDNS resolves a well known name and reports how long it took.
func probeDNS(ctx context.Context, name string) (bool, float64) {
	dctx, cancel := context.WithTimeout(ctx, 2500*time.Millisecond)
	defer cancel()
	start := time.Now()
	addrs, err := net.DefaultResolver.LookupHost(dctx, name)
	elapsed := time.Since(start)
	if err != nil || len(addrs) == 0 {
		return false, 0
	}
	return true, round2(float64(elapsed.Microseconds()) / 1000)
}

// listening reports TCP sockets in the LISTEN state, annotated with the
// process that owns them when that can be determined.
func (c *Collector) listening() []model.Listen {
	socks := c.root.ReadSockets()
	owners := c.socketOwners()

	out := make([]model.Listen, 0, 16)
	seen := make(map[string]bool, 16)
	for _, s := range socks {
		if s.State != "LISTEN" {
			continue
		}
		key := s.Proto + "|" + s.LocalIP + "|" + strconv.Itoa(s.Port)
		if seen[key] {
			continue
		}
		seen[key] = true

		l := model.Listen{
			Proto: s.Proto,
			Addr:  s.LocalIP,
			Port:  s.Port,
			// "0.0.0.0" and "::" mean every interface, so the socket is
			// reachable from outside the machine.
			Exposed: s.LocalIP == "0.0.0.0" || s.LocalIP == "::",
		}
		if owner, ok := owners[s.Inode]; ok {
			l.Process = owner.name
			l.PID = owner.pid
		}
		out = append(out, l)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Port != out[j].Port {
			return out[i].Port < out[j].Port
		}
		return out[i].Proto < out[j].Proto
	})
	return out
}

type owner struct {
	pid  int
	name string
}

// socketOwners maps socket inodes to processes by walking /proc/<pid>/fd.
// The walk is expensive, so the result is cached for a few seconds.
func (c *Collector) socketOwners() map[uint64]owner {
	c.ownerMu.Lock()
	defer c.ownerMu.Unlock()
	if c.owners != nil && c.opts.Now().Sub(c.ownersAt) < 5*time.Second {
		return c.owners
	}
	out := make(map[uint64]owner, 64)
	pids, err := c.root.PIDs()
	if err != nil {
		return c.owners
	}
	for _, pid := range pids {
		links, err := c.root.ReadDirNames("/proc/" + strconv.Itoa(pid) + "/fd")
		if err != nil {
			continue
		}
		name := ""
		for _, link := range links {
			target, err := c.root.Readlink("/proc/" + strconv.Itoa(pid) + "/fd/" + link)
			if err != nil || !strings.HasPrefix(target, "socket:[") {
				continue
			}
			inode := strings.TrimSuffix(strings.TrimPrefix(target, "socket:["), "]")
			v, err := strconv.ParseUint(inode, 10, 64)
			if err != nil {
				continue
			}
			if name == "" {
				name = c.processName(pid)
			}
			out[v] = owner{pid: pid, name: name}
		}
	}
	c.owners = out
	c.ownersAt = c.opts.Now()
	return out
}

func (c *Collector) processName(pid int) string {
	if st, err := c.root.ReadProcStat(pid); err == nil {
		return st.Comm
	}
	return ""
}

// interfaceIsUp reports the operational state of a real interface.
func interfaceIsUp(name string) bool {
	iface, err := net.InterfaceByName(name)
	if err != nil {
		return false
	}
	return iface.Flags&net.FlagUp != 0
}

func interfaceMAC(name string) string {
	iface, err := net.InterfaceByName(name)
	if err != nil {
		return ""
	}
	return iface.HardwareAddr.String()
}

// interfaceAddrs lists the addresses assigned to an interface, with the
// prefix length, skipping IPv6 link-local noise.
func interfaceAddrs(name string) []string {
	iface, err := net.InterfaceByName(name)
	if err != nil {
		return nil
	}
	addrs, err := iface.Addrs()
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(addrs))
	for _, a := range addrs {
		s := a.String()
		if strings.Contains(s, "fe80::") {
			continue
		}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
