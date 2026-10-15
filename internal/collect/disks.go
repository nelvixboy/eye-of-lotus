package collect

import (
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/nelvixboy/eye-of-lotus/internal/model"
	"github.com/nelvixboy/eye-of-lotus/internal/proc"
)

// diskState remembers the previous disk I/O counters so throughput can be
// reported as a rate.
type diskState struct {
	have bool
	at   time.Time
	devs map[string]proc.DiskStat
}

func newDiskState() *diskState { return &diskState{} }

// pseudoFS lists filesystems that carry no interesting space information.
// Reporting them would bury the real filesystems in noise.
var pseudoFS = map[string]bool{
	"proc": true, "sysfs": true, "devpts": true, "tmpfs": false, "devtmpfs": true,
	"cgroup": true, "cgroup2": true, "securityfs": true, "debugfs": true, "tracefs": true,
	"pstore": true, "bpf": true, "configfs": true, "fusectl": true, "mqueue": true,
	"hugetlbfs": true, "binfmt_misc": true, "rpc_pipefs": true, "nsfs": true,
	"autofs": true, "squashfs": true, "ramfs": true, "efivarfs": true,
}

// bookkeepingMounts are the mount points that hold kernel or session
// state rather than a filesystem somebody chose to mount. They are only
// reported when something real is mounted there, which is decided by
// whether the device is a block device path: /run/media/user/usb is a real
// disk, /run and /dev/shm are not.
var bookkeepingMounts = []string{"/proc", "/sys", "/dev", "/run"}

// isBookkeeping reports whether a mount is kernel bookkeeping rather than
// a filesystem worth warning about.
func isBookkeeping(device, point string) bool {
	if strings.HasPrefix(device, "/dev/") {
		return false
	}
	for _, prefix := range bookkeepingMounts {
		if point == prefix || strings.HasPrefix(point, prefix+"/") {
			return true
		}
	}
	return false
}

// collectDisks reports space and throughput for the mounted filesystems
// that a system administrator would actually watch.
func (c *Collector) collectDisks() []model.Disk {
	mounts, err := c.root.ReadMounts()
	if err != nil {
		return nil
	}
	stats, _ := c.root.ReadDiskStats()
	now := c.opts.Now()
	rates := c.disk.rates(stats, now)

	filter := make(map[string]bool, len(c.opts.Disks))
	for _, m := range c.opts.Disks {
		filter[m] = true
	}

	seen := make(map[string]bool, len(mounts))
	out := make([]model.Disk, 0, len(mounts))
	for _, m := range mounts {
		if pseudoFS[m.FSType] {
			continue
		}
		if len(filter) > 0 && !filter[m.Point] {
			continue
		}
		if isBookkeeping(m.Device, m.Point) {
			continue
		}
		if seen[m.Device] {
			// Bind mounts and overlay layers repeat the same device; the
			// first mount point wins, which is the shortest path.
			continue
		}
		seen[m.Device] = true

		var st syscall.Statfs_t
		if err := syscall.Statfs(c.root.DataPath(m.Point), &st); err != nil {
			continue
		}
		bsize := uint64(st.Bsize)
		total := st.Blocks * bsize
		free := st.Bavail * bsize
		used := total - st.Bfree*bsize
		if total == 0 {
			continue
		}
		d := model.Disk{
			Mount:   m.Point,
			Device:  m.Device,
			FSType:  m.FSType,
			Total:   total,
			Used:    used,
			Free:    free,
			UsedPct: proc.Percent(float64(used), float64(total)),
			// A read-only tmpfs or devtmpfs is ordinary; a read-only
			// ext4 on a real device means either deliberate hardening or
			// a kernel remount after an I/O error. Only the latter is
			// worth a warning, which is why this looks at the device.
			ReadOnly: strings.HasPrefix(m.Device, "/dev/") &&
				strings.Contains(m.Opts, "ro") && !strings.Contains(m.Opts, "rw"),
		}
		if st.Files > 0 {
			d.InodesPct = proc.Percent(float64(st.Files-st.Ffree), float64(st.Files))
		}
		if base, ok := deviceBase(m.Device); ok {
			d.DeviceBase = base
			if r, ok := rates[base]; ok {
				d.ReadBps, d.WriteBps = r.readBps, r.writeBps
				d.ReadIOPS, d.WriteIOPS = r.readIOPS, r.writeIOPS
			}
		}
		out = append(out, d)
	}
	// Show the fullest filesystem first: that is what needs attention.
	sort.SliceStable(out, func(i, j int) bool { return out[i].UsedPct > out[j].UsedPct })
	return out
}

// deviceBase maps "/dev/nvme0n1p2" to "nvme0n1p2" and resolves LVM and
// mapper devices, which is what /proc/diskstats keys on.
func deviceBase(dev string) (string, bool) {
	if !strings.HasPrefix(dev, "/dev/") {
		return "", false
	}
	base := filepath.Base(dev)
	if strings.HasPrefix(base, "dm-") {
		return base, true
	}
	// /dev/mapper/vg-lv is reported by diskstats under its dm-N name, which
	// cannot be derived from the path alone, so it is resolved by the
	// caller through the symlink target when possible.
	if target, err := filepath.EvalSymlinks(dev); err == nil {
		base = filepath.Base(target)
	}
	return base, true
}

type diskRate struct {
	readBps, writeBps   float64
	readIOPS, writeIOPS float64
}

// rates converts cumulative sector counters into per-second rates.
func (s *diskState) rates(cur map[string]proc.DiskStat, now time.Time) map[string]diskRate {
	out := make(map[string]diskRate, len(cur))
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
	const sectorSize = 512
	for name, c := range cur {
		p, ok := s.devs[name]
		if !ok {
			continue
		}
		r := diskRate{}
		if c.ReadSectors >= p.ReadSectors {
			r.readBps = float64(c.ReadSectors-p.ReadSectors) * sectorSize / elapsed
		}
		if c.WriteSectors >= p.WriteSectors {
			r.writeBps = float64(c.WriteSectors-p.WriteSectors) * sectorSize / elapsed
		}
		if c.ReadsDone >= p.ReadsDone {
			r.readIOPS = float64(c.ReadsDone-p.ReadsDone) / elapsed
		}
		if c.WritesDone >= p.WritesDone {
			r.writeIOPS = float64(c.WritesDone-p.WritesDone) / elapsed
		}
		out[name] = r
	}
	s.at = now
	s.devs = cur
	return out
}
