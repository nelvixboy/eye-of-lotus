// Package proc reads the Linux kernel's synthetic filesystem: /proc and
// /sys. Nothing in this package writes, and nothing in it requires root.
//
// Every function takes the root as an argument rather than hardcoding "/"
// so that the parsers can be tested against fixture files.
package proc

import (
	"bufio"
	"errors"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Root is a filesystem root under which /proc and /sys live. On a live
// machine it is "/", in tests it is a fixture directory.
//
// Paths handed to the file methods are relative to the root, so "proc/stat"
// means "/proc/stat" on a real machine and "<fixture>/proc/stat" in a test.
type Root struct {
	// Dir is the host path of the root, used by the few calls that need a
	// real path rather than an open file descriptor.
	Dir string
	FS  *os.Root
}

// Open opens a root at the given path. Pass "/" for the real machine.
func Open(dir string) (*Root, error) {
	if dir == "" {
		dir = "/"
	}
	r, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	return &Root{Dir: dir, FS: r}, nil
}

// Resolve turns a root-relative path into a host path.
func (r *Root) Resolve(name string) string {
	if r == nil {
		return name
	}
	return filepath.Join(r.Dir, name)
}

// DataPath maps an absolute kernel path such as a mount point onto a host
// path that can be handed to a syscall. On a real machine the path is
// returned unchanged; under a fixture root it is looked up inside the
// fixture, which is what makes the disk tests deterministic.
func (r *Root) DataPath(abs string) string {
	if r == nil || r.Dir == "/" || r.Dir == "" {
		return abs
	}
	return filepath.Join(r.Dir, strings.TrimPrefix(abs, "/"))
}

// Close releases the root.
func (r *Root) Close() error {
	if r == nil || r.FS == nil {
		return nil
	}
	return r.FS.Close()
}

func (r *Root) read(name string) ([]byte, error) {
	if r == nil || r.FS == nil {
		return nil, errors.New("proc: root is not open")
	}
	return fs.ReadFile(r.FS.FS(), name)
}

func (r *Root) open(name string) (*os.File, error) {
	if r == nil || r.FS == nil {
		return nil, errors.New("proc: root is not open")
	}
	return r.FS.Open(name)
}

// ReadFile reads a file under the root. Callers use it for /proc/cpuinfo,
// /etc/os-release and similar world readable files.
func (r *Root) ReadFile(name string) ([]byte, error) { return r.read(name) }

// StatFile reports whether a path exists under the root.
func (r *Root) StatFile(name string) (fs.FileInfo, error) {
	if r == nil || r.FS == nil {
		return nil, errors.New("proc: root is not open")
	}
	return fs.Stat(r.FS.FS(), name)
}

// scanLines calls fn for every line of a file, skipping a trailing newline.
func (r *Root) scanLines(name string, fn func(line string) error) error {
	f, err := r.open(name)
	if err != nil {
		return err
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	s.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for s.Scan() {
		if err := fn(s.Text()); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
	return s.Err()
}

func fields(line string) []string { return strings.Fields(line) }

func atof(s string) float64 {
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0
	}
	return v
}

func atou(s string) uint64 {
	v, err := strconv.ParseUint(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0
	}
	return v
}

func atoi(s string) int {
	v, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0
	}
	return v
}

// CPUTimes holds the cumulative jiffy counters from /proc/stat.
type CPUTimes struct {
	User, Nice, System, Idle, Iowait, IRQ, SoftIRQ, Steal uint64
}

// Total is the sum of every accounted jiffy.
func (c CPUTimes) Total() uint64 {
	return c.User + c.Nice + c.System + c.Idle + c.Iowait + c.IRQ + c.SoftIRQ + c.Steal
}

// Busy is every jiffy that was not spent idling.
func (c CPUTimes) Busy() uint64 {
	return c.Total() - c.Idle
}

func parseCPUTimes(f []string) (CPUTimes, bool) {
	if len(f) < 5 {
		return CPUTimes{}, false
	}
	return CPUTimes{
		User:    atou(f[1]),
		Nice:    atou(f[2]),
		System:  atou(f[3]),
		Idle:    atou(f[4]),
		Iowait:  atou(f[5%len(f)]),
		IRQ:     atou(f[min(6, len(f)-1)]),
		SoftIRQ: atou(f[min(7, len(f)-1)]),
		Steal:   atou(f[min(8, len(f)-1)]),
	}, true
}

// CPUStat is the parsed contents of /proc/stat: the aggregate line plus
// one entry per logical core.
type CPUStat struct {
	Total CPUTimes
	Cores []CPUTimes
}

// ReadCPUStat parses /proc/stat. Individual counter columns may be absent
// on old kernels; missing values read as zero.
func (r *Root) ReadCPUStat() (CPUStat, error) {
	var out CPUStat
	err := r.scanLines("proc/stat", func(line string) error {
		f := fields(line)
		if len(f) == 0 || !strings.HasPrefix(f[0], "cpu") {
			// The cpu lines are first in the file; the rest is not ours.
			return io.EOF
		}
		// Pad so that indexing for iowait/irq/softirq/steal is safe.
		for len(f) < 9 {
			f = append(f, "0")
		}
		t, ok := parseCPUTimes(f)
		if !ok {
			return nil
		}
		if f[0] == "cpu" {
			out.Total = t
		} else {
			out.Cores = append(out.Cores, t)
		}
		return nil
	})
	return out, err
}

// LoadAvg returns the one, five and fifteen minute load averages.
func (r *Root) LoadAvg() (l1, l5, l15 float64) {
	b, err := r.read("proc/loadavg")
	if err != nil {
		return 0, 0, 0
	}
	f := fields(string(b))
	if len(f) < 3 {
		return 0, 0, 0
	}
	return atof(f[0]), atof(f[1]), atof(f[2])
}

// Uptime returns the system uptime in seconds.
func (r *Root) Uptime() float64 {
	b, err := r.read("proc/uptime")
	if err != nil {
		return 0
	}
	f := fields(string(b))
	if len(f) == 0 {
		return 0
	}
	return atof(f[0])
}

// MemInfo is the parsed contents of /proc/meminfo, in bytes.
type MemInfo struct {
	Total, Free, Available, Buffers, Cached, SReclaimable, Shmem uint64
	SwapTotal, SwapFree                                          uint64
	Dirty, Writeback                                             uint64
}

// ReadMemInfo parses /proc/meminfo. Kernel values are in kibibytes and are
// converted to bytes here so that no caller has to remember the unit.
func (r *Root) ReadMemInfo() (MemInfo, error) {
	var m MemInfo
	err := r.scanLines("proc/meminfo", func(line string) error {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			return nil
		}
		kb := atou(fields(v)[0])
		b := kb * 1024
		switch k {
		case "MemTotal":
			m.Total = b
		case "MemFree":
			m.Free = b
		case "MemAvailable":
			m.Available = b
		case "Buffers":
			m.Buffers = b
		case "Cached":
			m.Cached = b
		case "SReclaimable":
			m.SReclaimable = b
		case "Shmem":
			m.Shmem = b
		case "SwapTotal":
			m.SwapTotal = b
		case "SwapFree":
			m.SwapFree = b
		case "Dirty":
			m.Dirty = b
		case "Writeback":
			m.Writeback = b
		}
		return nil
	})
	return m, err
}

// DiskStat is one row of /proc/diskstats.
type DiskStat struct {
	Name          string
	ReadsDone     uint64
	ReadSectors   uint64
	ReadMs        uint64
	WritesDone    uint64
	WriteSectors  uint64
	WriteMs       uint64
	InFlight      uint64
	IOMs          uint64
	WeightedIOMs  uint64
	DiscardsDone  uint64
	DiscardSector uint64
}

// ReadDiskStats parses /proc/diskstats, keyed by device name.
func (r *Root) ReadDiskStats() (map[string]DiskStat, error) {
	out := make(map[string]DiskStat, 32)
	err := r.scanLines("proc/diskstats", func(line string) error {
		f := fields(line)
		if len(f) < 14 {
			return nil
		}
		// Layout: major minor name reads reads_merged sectors read_ms
		// writes writes_merged sectors write_ms in_flight io_ms weighted.
		st := DiskStat{
			Name:         f[2],
			ReadsDone:    atou(f[3]),
			ReadSectors:  atou(f[5]),
			ReadMs:       atou(f[6]),
			WritesDone:   atou(f[7]),
			WriteSectors: atou(f[9]),
			WriteMs:      atou(f[10]),
			InFlight:     atou(f[11]),
			IOMs:         atou(f[12]),
			WeightedIOMs: atou(f[13]),
		}
		if len(f) >= 18 {
			st.DiscardsDone = atou(f[14])
			st.DiscardSector = atou(f[16])
		}
		out[st.Name] = st
		return nil
	})
	return out, err
}

// NetDevStat is one row of /proc/net/dev.
type NetDevStat struct {
	Name                             string
	RxBytes, RxPackets               uint64
	RxErrs, RxDrops, RxFifo, RxFrame uint64
	TxBytes, TxPackets               uint64
	TxErrs, TxDrops, TxFifo, TxColl  uint64
}

// ReadNetDev parses /proc/net/dev.
func (r *Root) ReadNetDev() (map[string]NetDevStat, error) {
	out := make(map[string]NetDevStat, 8)
	err := r.scanLines("proc/net/dev", func(line string) error {
		name, rest, ok := strings.Cut(line, ":")
		if !ok {
			return nil
		}
		f := fields(rest)
		if len(f) < 16 {
			return nil
		}
		out[strings.TrimSpace(name)] = NetDevStat{
			Name:      strings.TrimSpace(name),
			RxBytes:   atou(f[0]),
			RxPackets: atou(f[1]),
			RxErrs:    atou(f[2]),
			RxDrops:   atou(f[3]),
			RxFifo:    atou(f[4]),
			RxFrame:   atou(f[5]),
			TxBytes:   atou(f[8]),
			TxPackets: atou(f[9]),
			TxErrs:    atou(f[10]),
			TxDrops:   atou(f[11]),
			TxFifo:    atou(f[12]),
			TxColl:    atou(f[13]),
		}
		return nil
	})
	return out, err
}

// Sock is one socket or connection from /proc/net/{tcp,udp}.
type Sock struct {
	Proto   string
	LocalIP string
	Port    int
	State   string
	Inode   uint64
	UID     int
}

// ReadSockets parses the IPv4 and IPv6 socket tables.
func (r *Root) ReadSockets() []Sock {
	var out []Sock
	for _, t := range []struct{ file, proto string }{
		{"proc/net/tcp", "tcp"},
		{"proc/net/tcp6", "tcp6"},
		{"proc/net/udp", "udp"},
		{"proc/net/udp6", "udp6"},
	} {
		_ = r.scanLines(t.file, func(line string) error {
			f := fields(line)
			if len(f) < 10 || f[0] == "sl" {
				return nil
			}
			ip, port := parseHexAddr(f[1])
			if port == 0 {
				return nil
			}
			out = append(out, Sock{
				Proto:   t.proto,
				LocalIP: ip,
				Port:    port,
				State:   tcpState(f[3]),
				Inode:   atou(f[9]),
				UID:     atoi(f[7]),
			})
			return nil
		})
	}
	return out
}

// parseHexAddr decodes the "0100007F:1F90" form used by /proc/net.
// IPv4 addresses are little endian byte order; IPv6 is four 32-bit words.
func parseHexAddr(s string) (string, int) {
	hexIP, hexPort, ok := strings.Cut(s, ":")
	if !ok {
		return "", 0
	}
	port := 0
	if p, err := strconv.ParseUint(hexPort, 16, 32); err == nil {
		port = int(p)
	}
	switch len(hexIP) {
	case 8:
		b := make([]byte, 4)
		for i := 0; i < 4; i++ {
			v, err := strconv.ParseUint(hexIP[i*2:i*2+2], 16, 8)
			if err != nil {
				return hexIP, port
			}
			b[3-i] = byte(v)
		}
		return ipv4String(b), port
	case 32:
		// Sixteen bytes, written as four little endian 32-bit words.
		raw := make([]byte, 16)
		for w := 0; w < 4; w++ {
			for i := 0; i < 4; i++ {
				v, err := strconv.ParseUint(hexIP[w*8+i*2:w*8+i*2+2], 16, 8)
				if err != nil {
					return hexIP, port
				}
				raw[w*4+(3-i)] = byte(v)
			}
		}
		return ipv6String(raw), port
	}
	return hexIP, port
}

func ipv4String(b []byte) string {
	return strconv.Itoa(int(b[0])) + "." + strconv.Itoa(int(b[1])) + "." +
		strconv.Itoa(int(b[2])) + "." + strconv.Itoa(int(b[3]))
}

func ipv6String(b []byte) string {
	// Group into eight 16-bit words and compress the longest zero run,
	// following the usual :: shorthand rules.
	words := make([]uint16, 8)
	for i := 0; i < 8; i++ {
		words[i] = uint16(b[i*2])<<8 | uint16(b[i*2+1])
	}
	bestStart, bestLen, curStart, curLen := -1, 0, -1, 0
	for i, w := range words {
		if w == 0 {
			if curStart < 0 {
				curStart = i
			}
			curLen++
			if curLen > bestLen {
				bestStart, bestLen = curStart, curLen
			}
		} else {
			curStart, curLen = -1, 0
		}
	}
	var sb strings.Builder
	for i := 0; i < 8; i++ {
		if bestLen > 1 && i == bestStart {
			sb.WriteString("::")
			i += bestLen - 1
			continue
		}
		if i > 0 && !strings.HasSuffix(sb.String(), "::") {
			sb.WriteString(":")
		}
		sb.WriteString(strconv.FormatUint(uint64(words[i]), 16))
	}
	return sb.String()
}

// tcpState translates the kernel's hex state code.
func tcpState(h string) string {
	switch strings.ToUpper(h) {
	case "01":
		return "ESTABLISHED"
	case "02":
		return "SYN_SENT"
	case "03":
		return "SYN_RECV"
	case "04":
		return "FIN_WAIT1"
	case "05":
		return "FIN_WAIT2"
	case "06":
		return "TIME_WAIT"
	case "07":
		return "CLOSE"
	case "08":
		return "CLOSE_WAIT"
	case "09":
		return "LAST_ACK"
	case "0A":
		return "LISTEN"
	case "0B":
		return "CLOSING"
	case "0C":
		return "NEW_SYN_RECV"
	default:
		return "UNKNOWN"
	}
}

// Mount is one line of /proc/mounts.
type Mount struct {
	Device string
	Point  string
	FSType string
	Opts   string
}

// ReadMounts parses /proc/mounts. Space escapes are decoded.
func (r *Root) ReadMounts() ([]Mount, error) {
	var out []Mount
	err := r.scanLines("proc/mounts", func(line string) error {
		f := fields(line)
		if len(f) < 4 {
			return nil
		}
		out = append(out, Mount{
			Device: unescape(f[0]),
			Point:  unescape(f[1]),
			FSType: f[2],
			Opts:   f[3],
		})
		return nil
	})
	return out, err
}

// unescape decodes the \040 style escapes used in /proc/mounts.
func unescape(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var sb strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				sb.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		sb.WriteByte(s[i])
	}
	return sb.String()
}

// Percent returns a/b as a percentage, guarding against a zero denominator.
func Percent(a, b float64) float64 {
	if b <= 0 {
		return 0
	}
	return round2(a / b * 100)
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
