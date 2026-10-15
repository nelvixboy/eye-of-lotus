package proc

import (
	"errors"
	"io/fs"
	"strconv"
	"strings"
)

// ProcStat is the parsed contents of /proc/<pid>/stat. Only the fields Eye
// of Lotus actually uses are kept; the rest of the line is ignored so that
// kernel additions never break the parser.
type ProcStat struct {
	PID        int
	Comm       string
	State      string
	PPID       int
	UTime      uint64
	STime      uint64
	Threads    int
	StartTicks uint64
	VSZ        uint64
	RSSPages   uint64
	Nice       int
	Priority   int
	// CPUTicks is user plus system time.
	CPUTicks uint64
}

// StateWord expands the single letter state code into a readable word.
func (p ProcStat) StateWord() string {
	switch p.State {
	case "R":
		return "running"
	case "S":
		return "sleeping"
	case "D":
		return "uninterruptible"
	case "Z":
		return "zombie"
	case "T":
		return "stopped"
	case "t":
		return "tracing"
	case "X", "x":
		return "dead"
	case "I":
		return "idle"
	case "W":
		return "paging"
	case "P":
		return "parked"
	default:
		return "unknown"
	}
}

// parseStat parses one /proc/<pid>/stat line. The comm field is wrapped in
// parentheses and may itself contain spaces or parentheses, so the line is
// split on the last ')' rather than on whitespace.
func parseStat(line string) (ProcStat, bool) {
	open := strings.IndexByte(line, '(')
	closeIdx := strings.LastIndexByte(line, ')')
	if open < 0 || closeIdx < open {
		return ProcStat{}, false
	}
	var st ProcStat
	st.PID = atoi(line[:open])
	st.Comm = line[open+1 : closeIdx]

	// After ") " the fields continue from stat field 3 (state).
	rest := fields(line[closeIdx+1:])
	if len(rest) < 20 {
		return ProcStat{}, false
	}
	st.State = rest[0]
	st.PPID = atoi(rest[1])
	st.UTime = atou(rest[11])
	st.STime = atou(rest[12])
	st.Priority = atoi(rest[15])
	st.Nice = atoi(rest[16])
	st.Threads = atoi(rest[17])
	st.StartTicks = atou(rest[19])
	if len(rest) > 20 {
		st.VSZ = atou(rest[20])
	}
	if len(rest) > 21 {
		st.RSSPages = atou(rest[21])
	}
	st.CPUTicks = st.UTime + st.STime
	return st, true
}

// ReadProcStat reads and parses one process's stat file.
func (r *Root) ReadProcStat(pid int) (ProcStat, error) {
	b, err := r.read("proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return ProcStat{}, err
	}
	st, ok := parseStat(string(b))
	if !ok {
		return ProcStat{}, errors.New("proc: malformed stat line for pid " + strconv.Itoa(pid))
	}
	return st, nil
}

// PIDs lists the numeric entries of /proc, sorted ascending.
func (r *Root) PIDs() ([]int, error) {
	dir, err := r.FS.Open("proc")
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	entries, err := dir.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	out := make([]int, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if name == "" || name[0] < '0' || name[0] > '9' {
			continue
		}
		pid, err := strconv.Atoi(name)
		if err != nil {
			continue
		}
		out = append(out, pid)
	}
	// /proc is not sorted; sort so the output is stable between samples.
	sortInts(out)
	return out, nil
}

// ReadCmdline reads /proc/<pid>/cmdline and joins the argument vector with
// spaces. Kernel threads have an empty cmdline.
func (r *Root) ReadCmdline(pid int) string {
	b, err := r.read("proc/" + strconv.Itoa(pid) + "/cmdline")
	if err != nil || len(b) == 0 {
		return ""
	}
	parts := strings.Split(strings.TrimRight(string(b), "\x00"), "\x00")
	return strings.Join(parts, " ")
}

// ReadStatusUID returns the real UID of a process from /proc/<pid>/status.
func (r *Root) ReadStatusUID(pid int) (int, bool) {
	var uid int
	found := false
	_ = r.scanLines("proc/"+strconv.Itoa(pid)+"/status", func(line string) error {
		if !strings.HasPrefix(line, "Uid:") {
			return nil
		}
		f := fields(line)
		if len(f) >= 2 {
			uid = atoi(f[1])
			found = true
		}
		return nil
	})
	return uid, found
}

// ReadDirNames lists the entry names of a directory under the root.
func (r *Root) ReadDirNames(name string) ([]string, error) {
	dir, err := r.FS.Open(name)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	entries, err := dir.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out, nil
}

// Readlink resolves a symbolic link under the root, returning the raw
// target string without following it.
func (r *Root) Readlink(name string) (string, error) {
	if r == nil || r.FS == nil {
		return "", errors.New("proc: root is not open")
	}
	return r.FS.Readlink(name)
}

// ReadStatmRSS returns the resident set size of a process in bytes, read
// from /proc/<pid>/statm (field 2, in pages).
func (r *Root) ReadStatmRSS(pid int) (uint64, bool) {
	b, err := r.read("proc/" + strconv.Itoa(pid) + "/statm")
	if err != nil {
		return 0, false
	}
	f := fields(string(b))
	if len(f) < 2 {
		return 0, false
	}
	return atou(f[1]) * uint64(pageSize()), true
}

// pageSize reports the kernel page size in bytes.
func pageSize() int { return 4096 }

// BootTime derives the system boot time in seconds since the Unix epoch by
// combining /proc/uptime with the current wall clock. Callers pass now so
// that a single sample uses a single clock reading.
func (r *Root) BootTime(now int64) int64 {
	return now - int64(r.Uptime())
}

// ClockTicks is the kernel's USER_HZ, the unit of the CPU time fields in
// /proc/<pid>/stat. It is 100 on every architecture Linux supports.
const ClockTicks = 100

// ErrNoProc is returned when the process disappeared between listing and
// reading, which happens constantly on a busy machine and is not an error
// worth reporting.
var ErrNoProc = errors.New("proc: process vanished")

// IsNotExist reports whether an error means the file or process is gone.
func IsNotExist(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrPermission)
}

func sortInts(a []int) {
	// Insertion sort is fine: /proc listings arrive nearly sorted, and this
	// avoids pulling in a sort import for one call.
	for i := 1; i < len(a); i++ {
		for j := i; j > 0 && a[j] < a[j-1]; j-- {
			a[j], a[j-1] = a[j-1], a[j]
		}
	}
}
