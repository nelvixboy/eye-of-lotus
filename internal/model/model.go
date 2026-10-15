// Package model holds the data structures that Eye of Lotus collects,
// stores and serves to the browser.
//
// The structures are deliberately plain: no behaviour, no hidden state.
// A Snapshot is a complete picture of the machine at one instant, and
// everything the UI renders is derived from it.
package model

// Severity classifies a finding reported by the issue engine.
type Severity string

// Severity levels, ordered from least to most urgent.
const (
	// SevOK means everything is fine. It is rarely used for an issue,
	// but exists so that the UI can render a healthy verdict.
	SevOK Severity = "ok"
	// SevInfo is informational: worth knowing, not worth acting on.
	SevInfo Severity = "info"
	// SevWarn means something drifted from its healthy state.
	SevWarn Severity = "warn"
	// SevCrit means the machine is degraded right now.
	SevCrit Severity = "crit"
)

// Rank returns the numeric weight of a severity, useful for sorting.
func (s Severity) Rank() int {
	switch s {
	case SevCrit:
		return 3
	case SevWarn:
		return 2
	case SevInfo:
		return 1
	default:
		return 0
	}
}

// Snapshot is one complete observation of the machine.
type Snapshot struct {
	// Time is the moment the snapshot was taken, as a Unix timestamp in
	// milliseconds. Milliseconds keep the JavaScript side honest.
	Time int64 `json:"time"`
	// Uptime is the system uptime in seconds.
	Uptime float64 `json:"uptime"`
	// Hostnames carries the machine name and kernel identity.
	Host SystemInfo `json:"host"`
	// TempC is the highest thermal zone reading in degrees Celsius. Zero
	// means no sensor was found, which is normal on virtual machines.
	TempC float64 `json:"temp_c"`
	// CPU is the processor picture.
	CPU CPU `json:"cpu"`
	// Memory covers RAM and swap.
	Memory Memory `json:"memory"`
	// Disks lists mounted filesystems that are worth watching.
	Disks []Disk `json:"disks"`
	// Net is the network picture.
	Net Network `json:"net"`
	// Procs lists the most expensive processes.
	Procs []Process `json:"procs"`
	// TotalProcs is the number of processes on the machine, which is
	// usually far larger than len(Procs).
	TotalProcs int `json:"total_procs"`
	// Services lists systemd units and Docker containers.
	Services Services `json:"services"`
	// Issues is the verdict of the issue engine.
	Issues []Issue `json:"issues"`
	// Verdict is the worst severity found in Issues.
	Verdict Severity `json:"verdict"`
	// CollectMS is how long collection took, in milliseconds. A monitor
	// that is slow is itself a problem worth surfacing.
	CollectMS float64 `json:"collect_ms"`
}

// SystemInfo identifies the machine.
type SystemInfo struct {
	Hostname string `json:"hostname"`
	OS       string `json:"os"`
	Kernel   string `json:"kernel"`
	Arch     string `json:"arch"`
	// Virtualization is set when the machine runs inside a container or
	// a virtual machine, for example "docker", "kvm" or "none".
	Virtualization string `json:"virtualization"`
	// CPUs is the number of logical processors.
	CPUs int `json:"cpus"`
}

// CPU describes processor utilisation and load.
type CPU struct {
	// Usage is the total busy percentage across all cores, 0..100.
	Usage float64 `json:"usage"`
	// Cores carries per-core utilisation in the same 0..100 range.
	Cores []float64 `json:"cores"`
	// Load1, Load5 and Load15 are the kernel load averages.
	Load1  float64 `json:"load1"`
	Load5  float64 `json:"load5"`
	Load15 float64 `json:"load15"`
	// Iowait is the share of time spent waiting on disk, 0..100.
	Iowait float64 `json:"iowait"`
	// Steal is the share of time stolen by the hypervisor, 0..100.
	Steal float64 `json:"steal"`
	// Model is the human readable CPU name, when it can be read.
	Model string `json:"model"`
	// MHz is the current average clock, when it can be read.
	MHz float64 `json:"mhz"`
}

// Memory describes RAM and swap usage in bytes.
type Memory struct {
	Total     uint64  `json:"total"`
	Used      uint64  `json:"used"`
	Available uint64  `json:"available"`
	Free      uint64  `json:"free"`
	Cached    uint64  `json:"cached"`
	Buffers   uint64  `json:"buffers"`
	SwapTotal uint64  `json:"swap_total"`
	SwapUsed  uint64  `json:"swap_used"`
	UsedPct   float64 `json:"used_pct"`
	SwapPct   float64 `json:"swap_pct"`
}

// Disk describes one mounted filesystem.
type Disk struct {
	Mount      string  `json:"mount"`
	Device     string  `json:"device"`
	FSType     string  `json:"fstype"`
	Total      uint64  `json:"total"`
	Used       uint64  `json:"used"`
	Free       uint64  `json:"free"`
	UsedPct    float64 `json:"used_pct"`
	InodesPct  float64 `json:"inodes_pct"`
	ReadBps    float64 `json:"read_bps"`
	WriteBps   float64 `json:"write_bps"`
	ReadIOPS   float64 `json:"read_iops"`
	WriteIOPS  float64 `json:"write_iops"`
	ReadOnly   bool    `json:"read_only"`
	DeviceBase string  `json:"device_base"`
}

// Network describes connectivity and traffic.
type Network struct {
	// Online is true when the machine can reach the outside world.
	Online bool `json:"online"`
	// CheckedVia describes how Online was determined, for example
	// "tcp:1.1.1.1:53" or "disabled".
	CheckedVia string `json:"checked_via"`
	// LatencyMS is the round trip time of the connectivity probe.
	LatencyMS float64 `json:"latency_ms"`
	// DNSOK reports whether name resolution worked.
	DNSOK bool `json:"dns_ok"`
	// DNSMS is how long resolution took.
	DNSMS float64 `json:"dns_ms"`
	// DNSName is the name that was resolved.
	DNSName string `json:"dns_name"`
	// RxBps and TxBps are the current throughput in bytes per second.
	RxBps float64 `json:"rx_bps"`
	TxBps float64 `json:"tx_bps"`
	// RxTotal and TxTotal are lifetime counters in bytes.
	RxTotal uint64 `json:"rx_total"`
	TxTotal uint64 `json:"tx_total"`
	// Ifaces lists individual interfaces.
	Ifaces []Iface `json:"ifaces"`
	// Listening lists TCP sockets in the LISTEN state.
	Listening []Listen `json:"listening"`
}

// Iface is a single network interface.
type Iface struct {
	Name    string  `json:"name"`
	Up      bool    `json:"up"`
	MAC     string  `json:"mac"`
	RxBps   float64 `json:"rx_bps"`
	TxBps   float64 `json:"tx_bps"`
	RxTotal uint64  `json:"rx_total"`
	TxTotal uint64  `json:"tx_total"`
	RxErrs  uint64  `json:"rx_errs"`
	TxErrs  uint64  `json:"tx_errs"`
	RxDrops uint64  `json:"rx_drops"`
	TxDrops uint64  `json:"tx_drops"`
	// Addrs lists the addresses assigned to the interface.
	Addrs []string `json:"addrs"`
}

// Listen is one listening socket.
type Listen struct {
	Proto   string `json:"proto"`
	Addr    string `json:"addr"`
	Port    int    `json:"port"`
	Process string `json:"process,omitempty"`
	PID     int    `json:"pid,omitempty"`
	// Exposed marks a socket bound to a wildcard address, which means it
	// is reachable from the network rather than from localhost only.
	Exposed bool `json:"exposed"`
}

// Process is one running process.
type Process struct {
	PID     int     `json:"pid"`
	PPID    int     `json:"ppid"`
	User    string  `json:"user"`
	Name    string  `json:"name"`
	Cmdline string  `json:"cmdline"`
	State   string  `json:"state"`
	CPU     float64 `json:"cpu"`
	MemPct  float64 `json:"mem_pct"`
	RSS     uint64  `json:"rss"`
	Threads int     `json:"threads"`
	Started int64   `json:"started"`
	// Uptime is how long the process has been alive, in seconds.
	Uptime float64 `json:"uptime"`
}

// Services groups everything that can be started and stopped.
type Services struct {
	Systemd SystemdInfo `json:"systemd"`
	Docker  DockerInfo  `json:"docker"`
}

// SystemdInfo reports on systemd units.
type SystemdInfo struct {
	Available bool      `json:"available"`
	Error     string    `json:"error,omitempty"`
	Version   string    `json:"version,omitempty"`
	Total     int       `json:"total"`
	Running   int       `json:"running"`
	Failed    int       `json:"failed"`
	Units     []Service `json:"units"`
}

// DockerInfo reports on containers.
type DockerInfo struct {
	Available bool      `json:"available"`
	Error     string    `json:"error,omitempty"`
	Version   string    `json:"version,omitempty"`
	Total     int       `json:"total"`
	Running   int       `json:"running"`
	Unhealthy int       `json:"unhealthy"`
	Images    int       `json:"images"`
	Items     []Service `json:"items"`
}

// Service is a single manageable unit: a systemd unit or a container.
type Service struct {
	// Kind is "systemd" or "docker".
	Kind string `json:"kind"`
	// Name is the unit name or container name.
	Name string `json:"name"`
	// Description is the human readable summary.
	Description string `json:"description"`
	// LoadState is systemd's load state, for example "loaded".
	LoadState string `json:"load_state"`
	// ActiveState is systemd's active state, for example "active".
	ActiveState string `json:"active_state"`
	// SubState refines ActiveState, for example "running" or "exited".
	SubState string `json:"sub_state"`
	// Status is a normalised state: running, exited, failed, dead, unknown.
	Status string `json:"status"`
	// Enabled reports whether the unit starts at boot.
	Enabled string `json:"enabled,omitempty"`
	// Health is the container health check result, when present.
	Health string `json:"health,omitempty"`
	// Image is the container image, for docker services.
	Image string `json:"image,omitempty"`
	// Started is the container start time in Unix milliseconds.
	Started int64 `json:"started,omitempty"`
	// Restarts is Docker's restart counter.
	Restarts int `json:"restarts,omitempty"`
	// Problem carries a short human readable explanation when the unit is
	// not in a good state.
	Problem string `json:"problem,omitempty"`
}

// Issue is one finding produced by the issue engine.
type Issue struct {
	// ID is a stable identifier, for example "disk.full".
	ID string `json:"id"`
	// Severity ranks the finding.
	Severity Severity `json:"severity"`
	// Title is a short headline.
	Title string `json:"title"`
	// Detail explains what exactly was observed.
	Detail string `json:"detail"`
	// Hint suggests what to do about it. Eye of Lotus never acts on it.
	Hint string `json:"hint,omitempty"`
	// Subject names the thing the issue is about, for example "/dev/sda1".
	Subject string `json:"subject,omitempty"`
	// Value and Threshold are the numbers behind the verdict, when the
	// issue is a threshold breach.
	Value     float64 `json:"value,omitempty"`
	Threshold float64 `json:"threshold,omitempty"`
}

// HistoryPoint is one row of stored history.
type HistoryPoint struct {
	Time     int64   `json:"time"`
	CPUPct   float64 `json:"cpu_pct"`
	MemPct   float64 `json:"mem_pct"`
	SwapPct  float64 `json:"swap_pct"`
	DiskPct  float64 `json:"disk_pct"`
	Load1    float64 `json:"load1"`
	RxBps    float64 `json:"rx_bps"`
	TxBps    float64 `json:"tx_bps"`
	Procs    int     `json:"procs"`
	Issues   int     `json:"issues"`
	Crit     int     `json:"crit"`
	Online   bool    `json:"online"`
	NetMS    float64 `json:"net_ms"`
	TempC    float64 `json:"temp_c"`
	DiskRead float64 `json:"disk_read_bps"`
	DiskWrit float64 `json:"disk_write_bps"`
}
