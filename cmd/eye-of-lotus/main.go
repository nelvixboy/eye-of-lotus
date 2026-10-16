// Command eye-of-lotus runs the panel.
//
// It samples the machine it runs on, keeps a small history in SQLite and
// serves a read-only web interface. It never changes the state of the
// machine: no service is restarted, no process is killed, no file is
// written outside its own database.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/nelvixboy/eye-of-lotus/internal/collect"
	"github.com/nelvixboy/eye-of-lotus/internal/config"
	"github.com/nelvixboy/eye-of-lotus/internal/docker"
	"github.com/nelvixboy/eye-of-lotus/internal/install"
	"github.com/nelvixboy/eye-of-lotus/internal/model"
	"github.com/nelvixboy/eye-of-lotus/internal/store"
	"github.com/nelvixboy/eye-of-lotus/internal/systemd"
	"github.com/nelvixboy/eye-of-lotus/internal/web"
)

// version is replaced at build time through -ldflags.
var version = "dev"

func main() {
	// The two things the program does before it is a service rather than
	// after are handled first, and by their own flag set: installing writes
	// the configuration file that every other path reads, so it cannot wait
	// on one being there.
	if installMode, uninstallMode := installRequested(os.Args[1:]); installMode || uninstallMode {
		if err := runInstaller(installMode); err != nil {
			fmt.Fprintf(os.Stderr, "eye-of-lotus: %v\n", err)
			os.Exit(1)
		}
		return
	}
	if err := run(); err != nil {
		switch {
		case errors.Is(err, config.ErrShowVersion):
			fmt.Printf("eye-of-lotus %s\n", version)
			return
		case errors.Is(err, config.ErrHandled):
			return
		case errors.Is(err, flag.ErrHelp):
			// -h already printed the flags; asking for help is not a
			// failure, and a non-zero exit from it breaks scripts that use
			// it to check what a binary supports.
			return
		}
		fmt.Fprintf(os.Stderr, "eye-of-lotus: %v\n", err)
		os.Exit(1)
	}
}

// installRequested looks for the two flags before anything is parsed, so
// that their presence decides which flag set gets the arguments.
func installRequested(args []string) (install, uninstall bool) {
	for _, arg := range args {
		switch strings.TrimLeft(arg, "-") {
		case "install", "i":
			install = true
		case "uninstall", "u":
			uninstall = true
		}
	}
	return install, uninstall
}

// runInstaller parses the installer's own flags and does the work.
func runInstaller(asInstall bool) error {
	fs := flag.NewFlagSet("eye-of-lotus", flag.ContinueOnError)
	// The two mode flags are declared here as well, because this set is
	// handed the same arguments that chose it: parsing would otherwise
	// reject the very flag that got us here. Each has a one letter form,
	// since the common use is a single short command.
	fs.Bool("install", false, "install as a service and exit")
	fs.Bool("i", false, "short for -install")
	fs.Bool("uninstall", false, "remove the service and exit")
	fs.Bool("u", false, "short for -uninstall")
	listen := fs.String("listen", "", "address to bind, for example 0.0.0.0:9110")
	fs.StringVar(listen, "l", "", "short for -listen")
	noDockerGroup := fs.Bool("no-docker-group", false, "leave the service account out of the docker group")
	noFirewall := fs.Bool("no-firewall", false, "do not open the firewall")
	if err := fs.Parse(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			// -h already printed the flags; asking for help is not a
			// failure.
			return nil
		}
		return err
	}

	opts := install.Options{
		Version:       version,
		Listen:        *listen,
		ListenSet:     *listen != "",
		Password:      os.Getenv("EOL_PASSWORD"),
		NoDockerGroup: *noDockerGroup,
		NoFirewall:    *noFirewall,
	}
	if asInstall {
		return install.Run(opts)
	}
	return install.RunUninstall(opts)
}

func run() error {
	cfg, err := config.Load(os.Args[1:])
	if err != nil {
		return err
	}
	web.Version = version

	log := newLogger(cfg.LogLevel)
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// A demo run must work for somebody who downloaded a single file and
	// has no privileges at all. The configured history path belongs to the
	// service account, so a demo that was not told otherwise keeps its
	// history in a private temporary directory and takes it away on exit.
	// The charts still fill in: they read the recent past out of memory as
	// well, and that is all a demo has had time to collect anyway.
	if cfg.Demo && !cfg.DatabaseSet {
		if dir, err := os.MkdirTemp("", "eye-of-lotus-demo-"); err == nil {
			defer os.RemoveAll(dir)
			cfg.Database = filepath.Join(dir, "history.db")
		}
	}

	// History is optional: without a database the panel still works, it
	// simply forgets everything except the recent past held in memory.
	var history *store.Store
	if cfg.Database != "" {
		history, err = store.Open(store.Options{
			Path:         cfg.Database,
			RetainRaw:    cfg.RetainRaw.D(),
			RetainMinute: cfg.RetainMinute.D(),
			RetainHour:   cfg.RetainHour.D(),
		})
		if err != nil {
			return err
		}
		defer history.Close()
		log.Info("history enabled", "path", cfg.Database,
			"raw", cfg.RetainRaw.D().String(),
			"minute", cfg.RetainMinute.D().String(),
			"hour", cfg.RetainHour.D().String())
	} else {
		log.Warn("history disabled: no database configured, the graphs cover only this run")
	}

	live, cleanup, err := newLive(ctx, cfg, log, history)
	if err != nil {
		return err
	}
	defer cleanup()

	server := web.New(web.Options{
		Config:    cfg,
		Collector: live,
		Store:     historyInterface(history),
		Log:       log,
		Started:   time.Now(),
		Version:   version,
	})

	httpServer := &http.Server{
		Addr:              cfg.Listen,
		Handler:           server.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}

	errCh := make(chan error, 1)
	go func() {
		ln, err := net.Listen("tcp", cfg.Listen)
		if err != nil {
			errCh <- fmt.Errorf("cannot listen on %s: %w", cfg.Listen, err)
			return
		}
		log.Info("eye of lotus is watching",
			"address", "http://"+displayAddr(ln.Addr().String()),
			"demo", cfg.Demo, "version", version)
		if cfg.IsPublic() && !cfg.Demo {
			log.Warn("the listen address is reachable from the network; make sure the password is strong and consider a firewall")
		}
		if cfg.Password != "" && !config.IsHashed(cfg.Password) {
			log.Warn("the configured password is stored in plain text; run `eye-of-lotus -hash-password` and put the hash in the configuration file")
		}
		if err := httpServer.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		log.Info("shutting down")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Warn("http shutdown did not finish cleanly", "error", err)
	}
	return nil
}

// historyInterface returns a typed nil when history is disabled, so that
// the web layer sees "no store" rather than a non-nil interface holding a
// nil pointer.
func historyInterface(s *store.Store) web.History {
	if s == nil {
		return nil
	}
	return s
}

func displayAddr(addr string) string {
	if rest, ok := strings.CutPrefix(addr, "0.0.0.0:"); ok {
		return "127.0.0.1:" + rest
	}
	if rest, ok := strings.CutPrefix(addr, "[::]:"); ok {
		return "127.0.0.1:" + rest
	}
	return addr
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn", "warning":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
}

// collector is the sampling side of either a real or a demo machine.
type collector interface {
	Collect(ctx context.Context) model.Snapshot
	Close() error
}

// live is the sampling loop: it owns the collector, keeps the most recent
// snapshots in memory and hands them to the web layer.
type live struct {
	col   collector
	store *store.Store
	log   *slog.Logger

	mu      sync.RWMutex
	latest  model.Snapshot
	haveOne bool
	ring    []model.HistoryPoint

	// recorded tracks which findings have already been written to the
	// event log, so that a steady problem is recorded once rather than on
	// every sample.
	recorded map[string]bool
}

func newLive(ctx context.Context, cfg config.Config, log *slog.Logger, history *store.Store) (*live, func(), error) {
	l := &live{log: log, store: history, recorded: map[string]bool{}}

	var col collector
	if cfg.Demo {
		col = demoSampler{collect.NewDemo(collect.DemoOptions{
			Hostname: "lotus-demo",
			// The demo machine develops a filling filesystem after two
			// minutes, so the critical styling can be seen without
			// breaking anything real.
			Incident: 2 * time.Minute,
		})}
		log.Info("demo mode: the panel is showing a synthetic machine; nothing on this host is read")
	} else {
		opts := collect.Options{
			ProcRoot:      cfg.ProcRoot,
			Disks:         cfg.Disks,
			Ifaces:        cfg.Ifaces,
			TopProcs:      cfg.TopProcs,
			ProbeInterval: cfg.ProbeInterval.D(),
			ProbeTargets:  cfg.ProbeTargets,
			DNSName:       cfg.DNSName,
			DiskDetail:    cfg.SystemdDetail,
		}
		if cfg.Systemd {
			opts.Systemd = systemd.New(cfg.SystemdBus)
		}
		if cfg.Docker {
			opts.Docker = docker.New(cfg.DockerSocket)
		}
		real, err := collect.New(opts)
		if err != nil {
			return nil, nil, err
		}
		col = real
	}
	l.col = col

	// One sample before the loop starts, so that the first request has
	// something to show rather than an empty frame.
	l.publish(l.col.Collect(ctx))

	go l.loop(ctx, cfg.SampleInterval.D())

	return l, func() { _ = l.col.Close() }, nil
}

// demoSampler adapts the synthetic machine to the collector interface.
type demoSampler struct{ d *collect.Demo }

func (s demoSampler) Collect(ctx context.Context) model.Snapshot { return s.d.Snapshot(ctx) }
func (s demoSampler) Close() error                               { return nil }

// loop samples on a fixed cadence.
func (l *live) loop(ctx context.Context, sampleEvery time.Duration) {
	if sampleEvery <= 0 {
		sampleEvery = 2 * time.Second
	}
	ticker := time.NewTicker(sampleEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			snap := l.col.Collect(ctx)
			l.publish(snap)
			l.persist(ctx, snap)
		}
	}
}

// persist writes the sample to the history database. The store itself
// decides how often a row is actually written, so this is cheap to call
// on every tick.
func (l *live) persist(ctx context.Context, snap model.Snapshot) {
	if l.store == nil {
		return
	}
	writeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := l.store.Record(writeCtx, snap); err != nil {
		l.log.Warn("cannot write history", "error", err)
		return
	}
	if err := l.store.RecordEvents(writeCtx, snap, l.recorded); err != nil {
		l.log.Warn("cannot record events", "error", err)
		return
	}
	next := make(map[string]bool, len(snap.Issues))
	for _, i := range snap.Issues {
		if i.Severity.Rank() >= model.SevWarn.Rank() {
			next[i.ID+i.Subject] = true
		}
	}
	l.mu.Lock()
	l.recorded = next
	l.mu.Unlock()
}

// publish stores the newest snapshot and appends a point to the in-memory
// history ring, which is what the graphs use before the database has
// enough rows to be useful.
func (l *live) publish(snap model.Snapshot) {
	point := model.HistoryPoint{
		Time: snap.Time, CPUPct: snap.CPU.Usage, MemPct: snap.Memory.UsedPct,
		SwapPct: snap.Memory.SwapPct, Load1: snap.CPU.Load1, Procs: snap.TotalProcs,
		Online: snap.Net.Online, NetMS: snap.Net.LatencyMS, TempC: snap.TempC,
		RxBps: snap.Net.RxBps, TxBps: snap.Net.TxBps,
	}
	for _, d := range snap.Disks {
		if d.UsedPct > point.DiskPct {
			point.DiskPct = d.UsedPct
		}
		point.DiskRead += d.ReadBps
		point.DiskWrit += d.WriteBps
	}
	for _, i := range snap.Issues {
		switch i.Severity {
		case model.SevCrit:
			point.Crit++
			point.Issues++
		case model.SevWarn:
			point.Issues++
		}
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	l.latest = snap
	l.haveOne = true
	l.ring = append(l.ring, point)
	// Fifteen minutes of samples at two seconds each is enough for the
	// fallback graph; the database carries the longer story.
	if len(l.ring) > 512 {
		l.ring = l.ring[len(l.ring)-512:]
	}
}

// Snapshot implements web.Collector.
func (l *live) Snapshot(ctx context.Context) model.Snapshot {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.latest
}

// Recent exposes the in-memory ring, used when history is disabled.
func (l *live) Recent() []model.HistoryPoint {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]model.HistoryPoint, len(l.ring))
	copy(out, l.ring)
	return out
}
