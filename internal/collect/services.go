package collect

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nelvixboy/eye-of-lotus/internal/model"
	"github.com/nelvixboy/eye-of-lotus/internal/systemd"
)

// serviceState caches the expensive per-unit lookups. Listing units is
// cheap, but fetching properties for each one is a round trip per unit,
// and a server can easily have three hundred of them.
type serviceState struct {
	mu        sync.Mutex
	details   map[string]systemd.UnitDetail
	detailsAt time.Time
}

func newServiceState() *serviceState {
	return &serviceState{details: make(map[string]systemd.UnitDetail, 256)}
}

// detailTTL is how long per-unit properties are reused before being
// fetched again. State itself is always fresh; only the rarely changing
// properties are cached.
const detailTTL = 60 * time.Second

// collectServices gathers systemd units and Docker containers.
func (c *Collector) collectServices(ctx context.Context) model.Services {
	var out model.Services
	out.Systemd = c.collectSystemd(ctx)
	out.Docker = c.collectDocker(ctx)
	return out
}

// collectSystemd lists units and normalises their state.
func (c *Collector) collectSystemd(ctx context.Context) model.SystemdInfo {
	// The unit list starts empty rather than nil so that the JSON always
	// carries an array. A null there would make every client null-check a
	// field it is entitled to iterate.
	info := model.SystemdInfo{Units: []model.Service{}}
	client := c.opts.Systemd
	if client == nil {
		info.Error = "systemd support is disabled in the configuration"
		return info
	}
	if err := client.Connect(); err != nil {
		info.Error = systemd.Explain(err)
		return info
	}
	info.Available = true
	info.Version = client.Version()

	units, err := client.ListUnits()
	if err != nil {
		// The bus accepted the connection but the manager did not answer,
		// which usually means systemd is not the one behind the socket.
		info.Available = false
		info.Error = systemd.Explain(err)
		return info
	}
	details := c.unitDetails(ctx, client, units)

	info.Units = make([]model.Service, 0, len(units))
	for _, u := range units {
		svc := model.Service{
			Kind:        "systemd",
			Name:        u.Name,
			Description: u.Description,
			LoadState:   u.LoadState,
			ActiveState: u.ActiveState,
			SubState:    u.SubState,
			Status:      systemd.NormalizeState(u.ActiveState, u.SubState),
		}
		if d, ok := details[u.Name]; ok {
			svc.Enabled = d.UnitFileState
			if d.NRestarts > 0 {
				svc.Restarts = int(d.NRestarts)
			}
		}
		if svc.LoadState == "not-found" {
			svc.Status = "missing"
			svc.Problem = "the unit file no longer exists"
		}
		if svc.ActiveState == "failed" {
			svc.Problem = "the unit entered the failed state"
		}
		info.Total++
		switch svc.Status {
		case "running":
			info.Running++
		case "failed":
			info.Failed++
		}
		info.Units = append(info.Units, svc)
	}
	// Failed first, then running, then everything else by name: the list
	// should lead with what needs attention.
	sort.SliceStable(info.Units, func(i, j int) bool {
		return unitRank(info.Units[i]) < unitRank(info.Units[j]) ||
			(unitRank(info.Units[i]) == unitRank(info.Units[j]) && info.Units[i].Name < info.Units[j].Name)
	})
	return info
}

func unitRank(s model.Service) int {
	switch s.Status {
	case "failed":
		return 0
	case "running":
		return 1
	case "starting", "reloading":
		return 2
	default:
		return 3
	}
}

// unitDetails returns cached per-unit properties, refreshing them when the
// cache is older than detailTTL.
func (c *Collector) unitDetails(ctx context.Context, client *systemd.Client, units []systemd.Unit) map[string]systemd.UnitDetail {
	if !c.opts.DiskDetail {
		return nil
	}
	st := c.services
	st.mu.Lock()
	if st.details != nil && c.opts.Now().Sub(st.detailsAt) < detailTTL {
		out := st.details
		st.mu.Unlock()
		return out
	}
	st.mu.Unlock()

	// Fetch in a small pool: sequential round trips would take seconds on
	// a machine with many units.
	const workers = 8
	type result struct {
		name string
		d    systemd.UnitDetail
	}
	jobs := make(chan systemd.Unit, workers)
	results := make(chan result, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for u := range jobs {
				if ctx.Err() != nil {
					return
				}
				d, err := client.Detail(u.Path)
				if err != nil {
					continue
				}
				results <- result{name: u.Name, d: d}
			}
		}()
	}
	go func() {
		defer close(jobs)
		for _, u := range units {
			select {
			case jobs <- u:
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() {
		wg.Wait()
		close(results)
	}()

	fresh := make(map[string]systemd.UnitDetail, len(units))
	for r := range results {
		fresh[r.name] = r.d
	}

	st.mu.Lock()
	st.details = fresh
	st.detailsAt = c.opts.Now()
	st.mu.Unlock()
	return fresh
}

// collectDocker lists containers and annotates the interesting ones.
func (c *Collector) collectDocker(ctx context.Context) model.DockerInfo {
	// As above: an empty array, never null.
	info := model.DockerInfo{Items: []model.Service{}}
	client := c.opts.Docker
	if client == nil {
		info.Error = "docker support is disabled in the configuration"
		return info
	}
	if !client.Available(ctx) {
		info.Error = "the Docker daemon is not reachable at " + client.Socket()
		return info
	}
	info.Available = true

	if v, err := client.Version(ctx); err == nil {
		info.Version = v
	}
	if di, err := client.Info(ctx); err == nil {
		info.Images = di.Images
	}
	containers, err := client.Containers(ctx)
	if err != nil {
		info.Available = false
		info.Error = err.Error()
		return info
	}

	info.Items = make([]model.Service, 0, len(containers))
	for _, ct := range containers {
		svc := model.Service{
			Kind:        "docker",
			Name:        ct.Name(),
			Description: strings.TrimSpace(ct.Image + " " + ct.Status),
			ActiveState: ct.State,
			SubState:    ct.Status,
			Status:      normalizeContainerState(ct.State),
			Image:       ct.Image,
			Started:     ct.Created * 1000,
		}
		info.Total++
		switch svc.Status {
		case "running":
			info.Running++
		}
		if svc.Status == "restarting" {
			svc.Problem = "the container is stuck in a restart loop"
		}
		if svc.Status == "dead" || svc.Status == "paused" {
			svc.Problem = "container state is " + svc.Status
		}
		// Only containers that are not plainly healthy are inspected in
		// detail: the call is one HTTP round trip per container.
		if svc.Status != "running" || info.Total <= 64 {
			if detail, err := client.Inspect(ctx, ct.ID); err == nil {
				svc.Health = detail.Health
				svc.Restarts = detail.RestartCount
				if detail.StartedAt != "" {
					if t, err := time.Parse(time.RFC3339Nano, detail.StartedAt); err == nil {
						svc.Started = t.UnixMilli()
					}
				}
				if detail.Health == "unhealthy" {
					svc.Problem = "the container health check is failing"
					info.Unhealthy++
				}
			}
		}
		info.Items = append(info.Items, svc)
	}
	sort.SliceStable(info.Items, func(i, j int) bool {
		return unitRank(info.Items[i]) < unitRank(info.Items[j]) ||
			(unitRank(info.Items[i]) == unitRank(info.Items[j]) && info.Items[i].Name < info.Items[j].Name)
	})
	return info
}

// normalizeContainerState maps Docker's lowercase states onto the panel's
// vocabulary.
func normalizeContainerState(state string) string {
	switch strings.ToLower(state) {
	case "running":
		return "running"
	case "exited", "created":
		return "exited"
	case "restarting":
		return "restarting"
	case "paused":
		return "paused"
	case "dead":
		return "dead"
	case "removing":
		return "removing"
	default:
		return "unknown"
	}
}
