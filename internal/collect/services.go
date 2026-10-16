package collect

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nelvixboy/eye-of-lotus/internal/docker"
	"github.com/nelvixboy/eye-of-lotus/internal/model"
	"github.com/nelvixboy/eye-of-lotus/internal/systemd"
)

// serviceState caches the expensive per-service lookups. Asking for the
// list is cheap; asking for properties is a round trip per item, and a
// server can easily have three hundred units and a few dozen containers.
type serviceState struct {
	mu        sync.Mutex
	details   map[string]systemd.UnitDetail
	detailsAt time.Time

	// containers holds one inspect result per container id.
	containers   map[string]docker.Container
	containersAt time.Time

	// The daemon version and the image count, which change about never.
	dockerVersion   string
	dockerImages    int
	dockerSummaryAt time.Time

	// containerList is the result of listing containers, which is far more
	// expensive than it looks because the daemon gathers each container's
	// state before answering.
	containerList   []docker.Container
	containerListAt time.Time
}

func newServiceState() *serviceState {
	return &serviceState{details: make(map[string]systemd.UnitDetail, 256)}
}

// detailTTL is how long per-unit properties are reused before being
// fetched again. State itself is always fresh; only the rarely changing
// properties are cached.
const detailTTL = 60 * time.Second

// containerTTL is the same idea for containers, kept shorter because a
// container can start failing at any moment and the health check is what
// the panel reports about it.
const containerTTL = 30 * time.Second

// containerListTTL is how long the container list is reused. Listing
// containers makes the daemon gather each container's state before it
// answers, measured at about fifty milliseconds on a host with thirteen of
// them, against half a millisecond for a single inspect of the same
// container. Asking once every few seconds rather than on every sample is
// most of the difference between a cheap sample and an expensive one, and
// container state that is a few seconds old is still container state.
const containerListTTL = 5 * time.Second

// dockerSummaryTTL is how long the daemon version and the image count are
// reused. Both change about never, and asking for them is expensive: the
// info endpoint makes the daemon gather statistics about images, containers
// and drivers before it answers. It was the single most expensive call a
// sample made, and it was being made for one number.
const dockerSummaryTTL = 60 * time.Second

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
	if err := client.Check(ctx); err != nil {
		info.Error = explainDocker(err, client.Socket())
		return info
	}
	info.Available = true

	info.Version, info.Images = c.dockerSummary(ctx, client)
	containers, err := c.containerList(ctx, client)
	if err != nil {
		info.Available = false
		info.Error = err.Error()
		return info
	}
	details := c.containerDetails(ctx, client, containers)

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
		if detail, ok := details[ct.ID]; ok {
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

// explainDocker turns a connection failure into the thing that is actually
// wrong. "Not reachable" is technically true of a permission problem and
// sends the reader looking in the wrong place: the socket is there, the
// daemon is running, and this process simply may not open it. That is the
// default state of a fresh install, because the service account is not in
// the docker group until somebody puts it there.
func explainDocker(err error, socket string) string {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "no such file or directory"):
		return "the Docker daemon is not running: nothing is listening on " + socket
	case strings.Contains(msg, "permission denied"):
		return "permission denied on " + socket + ": the panel runs as a user that may not open it. Add that user to the docker group with `usermod -aG docker <user>` and restart the service"
	default:
		return "cannot reach the Docker daemon at " + socket + ": " + msg
	}
}

// containerDetails returns one inspect result per container, refreshing the
// whole set when it has gone stale.
//
// The list endpoint is cheap and is asked on every sample, so the container
// states on screen are always current. The detail endpoint is a round trip
// each, and on a host with a dozen containers it was the single most
// expensive thing a sample did: about ten milliseconds per container, every
// two seconds, for numbers that change far more slowly than that.
func (c *Collector) containerDetails(ctx context.Context, client *docker.Client, containers []docker.Container) map[string]docker.Container {
	st := c.services
	st.mu.Lock()
	cached := st.containers
	fresh := cached != nil &&
		len(cached) == len(containers) &&
		c.opts.Now().Sub(st.containersAt) < containerTTL
	st.mu.Unlock()
	if fresh {
		return cached
	}

	out := make(map[string]docker.Container, len(containers))
	for _, ct := range containers {
		if ctx.Err() != nil {
			break
		}
		d, err := client.Inspect(ctx, ct.ID)
		if err != nil {
			continue
		}
		out[ct.ID] = d
	}

	st.mu.Lock()
	st.containers = out
	st.containersAt = c.opts.Now()
	st.mu.Unlock()
	return out
}

// dockerSummary returns the daemon version and the image count, asking the
// daemon only when the cached answer has gone stale.
func (c *Collector) dockerSummary(ctx context.Context, client *docker.Client) (version string, images int) {
	st := c.services
	st.mu.Lock()
	if !st.dockerSummaryAt.IsZero() && c.opts.Now().Sub(st.dockerSummaryAt) < dockerSummaryTTL {
		version, images = st.dockerVersion, st.dockerImages
		st.mu.Unlock()
		return version, images
	}
	st.mu.Unlock()

	if v, err := client.Version(ctx); err == nil {
		version = v
	}
	if di, err := client.Info(ctx); err == nil {
		images = di.Images
	}

	st.mu.Lock()
	st.dockerVersion, st.dockerImages = version, images
	st.dockerSummaryAt = c.opts.Now()
	st.mu.Unlock()
	return version, images
}

// containerList returns the containers, asking the daemon only when the
// cached answer has gone stale.
func (c *Collector) containerList(ctx context.Context, client *docker.Client) ([]docker.Container, error) {
	st := c.services
	st.mu.Lock()
	if !st.containerListAt.IsZero() && c.opts.Now().Sub(st.containerListAt) < containerListTTL {
		cached := st.containerList
		st.mu.Unlock()
		return cached, nil
	}
	st.mu.Unlock()

	list, err := client.Containers(ctx)
	if err != nil {
		return nil, err
	}

	st.mu.Lock()
	st.containerList = list
	st.containerListAt = c.opts.Now()
	st.mu.Unlock()
	return list, nil
}
