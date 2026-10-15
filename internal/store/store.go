// Package store keeps metric history in SQLite.
//
// Three resolutions are kept in one table: raw samples for the recent
// past, one minute averages for the last week, and one hour averages for
// three months. Aggregation happens in place on a timer, and each level
// is pruned once its retention window has passed.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/nelvixboy/eye-of-lotus/internal/model"

	_ "modernc.org/sqlite"
)

// Resolution names the aggregation level of a stored row.
type Resolution string

// The three levels of detail the database holds.
const (
	// Raw is one row per StoreInterval.
	Raw Resolution = "raw"
	// Minute is one averaged row per minute.
	Minute Resolution = "minute"
	// Hour is one averaged row per hour.
	Hour Resolution = "hour"
)

// DefaultWriteInterval is how often a sample becomes a stored row when
// the caller does not say otherwise. At this cadence the raw table grows
// by about 5,700 rows a day.
const DefaultWriteInterval = 5 * time.Second

// Options configures a Store.
type Options struct {
	// Path is the SQLite file. Its directory is created if needed.
	Path string
	// RetainRaw, RetainMinute and RetainHour are the retention windows.
	RetainRaw    time.Duration
	RetainMinute time.Duration
	RetainHour   time.Duration
	// WriteInterval is the minimum time between raw rows. Samples arrive
	// far more often than the database needs to remember them. A negative
	// value disables the throttle, which the tests rely on.
	WriteInterval time.Duration
	// Clock is injectable for tests; it defaults to time.Now.
	Clock func() time.Time
}

// Store is a handle on the history database. It is safe for concurrent
// use; SQLite is opened in WAL mode with a single writer.
type Store struct {
	db  *sql.DB
	opt Options

	mu        sync.Mutex
	last      *model.HistoryPoint
	lastAt    time.Time
	lastWrite time.Time
	// lastSampleMS is the timestamp of the most recent sample seen, which
	// is what the write cadence is measured against. Using the wall clock
	// instead would drop rows whenever the two disagree, as they do in
	// tests and whenever a snapshot arrives late.
	lastSampleMS int64
}

// Open creates or opens the database and applies the schema.
func Open(opt Options) (*Store, error) {
	if opt.Path == "" {
		return nil, errors.New("store: empty database path")
	}
	if opt.RetainRaw <= 0 {
		opt.RetainRaw = 24 * time.Hour
	}
	if opt.RetainMinute <= 0 {
		opt.RetainMinute = 7 * 24 * time.Hour
	}
	if opt.RetainHour <= 0 {
		opt.RetainHour = 90 * 24 * time.Hour
	}
	if opt.WriteInterval == 0 {
		opt.WriteInterval = DefaultWriteInterval
	}
	// A negative interval disables the throttle entirely, which is what
	// the tests use to write every sample.
	if opt.WriteInterval < 0 {
		opt.WriteInterval = 0
	}
	if opt.Clock == nil {
		opt.Clock = time.Now
	}
	if dir := filepath.Dir(opt.Path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("store: cannot create %s: %w", dir, err)
		}
	}

	// _pragma parameters are applied to every new connection in the pool.
	dsn := "file:" + opt.Path + "?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: cannot open %s: %w", opt.Path, err)
	}
	// One writer avoids SQLITE_BUSY entirely; the write volume here is a
	// few rows per minute.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	s := &Store{db: db, opt: opt}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close flushes and closes the database.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	// Fold the last raw samples into their buckets before shutting down so
	// that a restart does not lose the recent past.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = s.aggregate(ctx)
	// Checkpoint the write-ahead log so that a long-running panel does not
	// leave a large -wal file behind on shutdown.
	_, _ = s.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	return s.db.Close()
}

const schema = `
CREATE TABLE IF NOT EXISTS samples (
    ts          INTEGER NOT NULL,
    resolution  TEXT    NOT NULL,
    cpu_pct     REAL    NOT NULL DEFAULT 0,
    mem_pct     REAL    NOT NULL DEFAULT 0,
    swap_pct    REAL    NOT NULL DEFAULT 0,
    disk_pct    REAL    NOT NULL DEFAULT 0,
    load1       REAL    NOT NULL DEFAULT 0,
    rx_bps      REAL    NOT NULL DEFAULT 0,
    tx_bps      REAL    NOT NULL DEFAULT 0,
    procs       INTEGER NOT NULL DEFAULT 0,
    issues      INTEGER NOT NULL DEFAULT 0,
    crit        INTEGER NOT NULL DEFAULT 0,
    online      INTEGER NOT NULL DEFAULT 1,
    net_ms      REAL    NOT NULL DEFAULT 0,
    temp_c      REAL    NOT NULL DEFAULT 0,
    disk_read   REAL    NOT NULL DEFAULT 0,
    disk_write  REAL    NOT NULL DEFAULT 0,
    PRIMARY KEY (ts, resolution)
) WITHOUT ROWID;

CREATE INDEX IF NOT EXISTS samples_resolution_ts ON samples (resolution, ts);

CREATE TABLE IF NOT EXISTS meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS events (
    ts       INTEGER NOT NULL,
    severity TEXT    NOT NULL,
    id       TEXT    NOT NULL,
    title    TEXT    NOT NULL,
    detail   TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS events_ts ON events (ts DESC);
`

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("store: cannot apply schema: %w", err)
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO meta (key, value) VALUES ('schema', '1') ON CONFLICT (key) DO NOTHING`)
	if err != nil {
		return fmt.Errorf("store: cannot stamp schema version: %w", err)
	}
	return nil
}

// Record writes one sample if enough time has passed since the last write,
// and folds finished buckets into their aggregates.
func (s *Store) Record(ctx context.Context, snap model.Snapshot) error {
	point := pointOf(snap)

	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.opt.Clock()
	s.last = &point
	s.lastAt = now

	// The raw table is written on a fixed cadence, not on every sample.
	// The comparison uses the sample's own timestamp.
	if s.opt.WriteInterval > 0 && s.lastSampleMS != 0 {
		gap := time.Duration(point.Time-s.lastSampleMS) * time.Millisecond
		if gap >= 0 && gap < s.opt.WriteInterval {
			return s.aggregateLocked(ctx)
		}
	}
	s.lastWrite = now
	s.lastSampleMS = point.Time

	_, err := s.db.ExecContext(ctx, `
INSERT INTO samples (ts, resolution, cpu_pct, mem_pct, swap_pct, disk_pct, load1,
                     rx_bps, tx_bps, procs, issues, crit, online, net_ms, temp_c,
                     disk_read, disk_write)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (ts, resolution) DO UPDATE SET
    cpu_pct = excluded.cpu_pct,
    mem_pct = excluded.mem_pct,
    swap_pct = excluded.swap_pct,
    disk_pct = excluded.disk_pct,
    load1 = excluded.load1,
    rx_bps = excluded.rx_bps,
    tx_bps = excluded.tx_bps,
    procs = excluded.procs,
    issues = excluded.issues,
    crit = excluded.crit,
    online = excluded.online,
    net_ms = excluded.net_ms,
    temp_c = excluded.temp_c,
    disk_read = excluded.disk_read,
    disk_write = excluded.disk_write`,
		point.Time/1000, string(Raw), point.CPUPct, point.MemPct, point.SwapPct,
		point.DiskPct, point.Load1, point.RxBps, point.TxBps, point.Procs,
		point.Issues, point.Crit, boolToInt(point.Online), point.NetMS,
		point.TempC, point.DiskRead, point.DiskWrit)
	if err != nil {
		return fmt.Errorf("store: cannot write sample: %w", err)
	}
	return s.aggregateLocked(ctx)
}

// pointOf reduces a full snapshot to the handful of numbers worth keeping
// forever.
func pointOf(snap model.Snapshot) model.HistoryPoint {
	p := model.HistoryPoint{
		Time:   snap.Time,
		CPUPct: snap.CPU.Usage,
		MemPct: snap.Memory.UsedPct,
		Load1:  snap.CPU.Load1,
		Procs:  snap.TotalProcs,
		Online: snap.Net.Online,
		NetMS:  snap.Net.LatencyMS,
		RxBps:  snap.Net.RxBps,
		TxBps:  snap.Net.TxBps,
	}
	p.SwapPct = snap.Memory.SwapPct
	p.TempC = snap.TempC
	for _, d := range snap.Disks {
		if d.UsedPct > p.DiskPct {
			p.DiskPct = d.UsedPct
		}
		p.DiskRead += d.ReadBps
		p.DiskWrit += d.WriteBps
	}
	for _, i := range snap.Issues {
		switch i.Severity {
		case model.SevCrit:
			p.Crit++
			p.Issues++
		case model.SevWarn:
			p.Issues++
		}
	}
	return p
}

// aggregateLocked folds completed buckets into coarser rows and prunes
// expired data. The caller must hold the mutex.
func (s *Store) aggregateLocked(ctx context.Context) error {
	// Aggregation touches many rows for little benefit if run constantly;
	// once a minute is plenty for a panel.
	if s.opt.Clock().Sub(s.lastAggregate()) < time.Minute {
		return nil
	}
	s.markAggregate()
	return s.aggregate(ctx)
}

func (s *Store) lastAggregate() time.Time {
	var raw string
	err := s.db.QueryRow(`SELECT value FROM meta WHERE key = 'aggregated_at'`).Scan(&raw)
	if err != nil {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}
	}
	return t
}

func (s *Store) markAggregate() {
	_, _ = s.db.Exec(`INSERT INTO meta (key, value) VALUES ('aggregated_at', ?)
	                  ON CONFLICT (key) DO UPDATE SET value = excluded.value`,
		s.opt.Clock().UTC().Format(time.RFC3339))
}

// aggregate builds minute rows from raw rows and hour rows from minute
// rows, then deletes whatever has fallen outside its retention window.
func (s *Store) aggregate(ctx context.Context) error {
	if err := s.rollup(ctx, Raw, Minute, 60); err != nil {
		return err
	}
	if err := s.rollup(ctx, Minute, Hour, 3600); err != nil {
		return err
	}
	return s.prune(ctx)
}

// rollup averages rows of one resolution into buckets of bucketSeconds at
// the next resolution, skipping the bucket that is still filling up.
func (s *Store) rollup(ctx context.Context, from, to Resolution, bucketSeconds int64) error {
	// The newest bucket is deliberately left alone: it is still growing,
	// and averaging it now would store a value that never happened.
	cutoff := s.opt.Clock().Unix() - bucketSeconds
	_, err := s.db.ExecContext(ctx, `
INSERT INTO samples (ts, resolution, cpu_pct, mem_pct, swap_pct, disk_pct, load1,
                     rx_bps, tx_bps, procs, issues, crit, online, net_ms, temp_c,
                     disk_read, disk_write)
SELECT (ts / ?) * ? AS bucket,
       ?,
       AVG(cpu_pct), AVG(mem_pct), AVG(swap_pct), AVG(disk_pct), AVG(load1),
       AVG(rx_bps), AVG(tx_bps), CAST(AVG(procs) AS INTEGER),
       CAST(AVG(issues) AS INTEGER), CAST(AVG(crit) AS INTEGER),
       CAST(AVG(online) AS INTEGER), AVG(net_ms), AVG(temp_c),
       AVG(disk_read), AVG(disk_write)
FROM samples
WHERE resolution = ? AND ts < ?
GROUP BY bucket
ON CONFLICT (ts, resolution) DO UPDATE SET
    cpu_pct = excluded.cpu_pct,
    mem_pct = excluded.mem_pct,
    swap_pct = excluded.swap_pct,
    disk_pct = excluded.disk_pct,
    load1 = excluded.load1,
    rx_bps = excluded.rx_bps,
    tx_bps = excluded.tx_bps,
    procs = excluded.procs,
    issues = excluded.issues,
    crit = excluded.crit,
    online = excluded.online,
    net_ms = excluded.net_ms,
    temp_c = excluded.temp_c,
    disk_read = excluded.disk_read,
    disk_write = excluded.disk_write`,
		bucketSeconds, bucketSeconds, string(to), string(from), cutoff)
	if err != nil {
		return fmt.Errorf("store: cannot roll %s into %s: %w", from, to, err)
	}
	return nil
}

// prune deletes rows past their retention window.
func (s *Store) prune(ctx context.Context) error {
	now := s.opt.Clock().Unix()
	windows := []struct {
		res    Resolution
		retain time.Duration
	}{
		{Raw, s.opt.RetainRaw},
		{Minute, s.opt.RetainMinute},
		{Hour, s.opt.RetainHour},
	}
	for _, w := range windows {
		cutoff := now - int64(w.retain.Seconds())
		if _, err := s.db.ExecContext(ctx,
			`DELETE FROM samples WHERE resolution = ? AND ts < ?`, string(w.res), cutoff); err != nil {
			return fmt.Errorf("store: cannot prune %s: %w", w.res, err)
		}
	}
	// Events are kept for the longest retention window.
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM events WHERE ts < ?`, now-int64(s.opt.RetainHour.Seconds())); err != nil {
		return fmt.Errorf("store: cannot prune events: %w", err)
	}
	return nil
}

// RecordEvents stores findings that are worth remembering after they stop
// being visible in the live snapshot, such as a unit that failed an hour
// ago and was restarted since.
func (s *Store) RecordEvents(ctx context.Context, snap model.Snapshot, previous map[string]bool) error {
	var fresh []model.Issue
	for _, i := range snap.Issues {
		if i.Severity.Rank() < model.SevWarn.Rank() {
			continue
		}
		if previous[i.ID+i.Subject] {
			continue
		}
		fresh = append(fresh, i)
	}
	if len(fresh) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO events (ts, severity, id, title, detail) VALUES (?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, i := range fresh {
		if _, err := stmt.ExecContext(ctx, snap.Time, string(i.Severity), i.ID, i.Title, i.Detail); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Events returns the most recent recorded findings, newest first.
func (s *Store) Events(ctx context.Context, limit int) ([]Event, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT ts, severity, id, title, detail FROM events ORDER BY ts DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.Time, &e.Severity, &e.ID, &e.Title, &e.Detail); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Event is one recorded finding.
type Event struct {
	Time     int64  `json:"time"`
	Severity string `json:"severity"`
	ID       string `json:"id"`
	Title    string `json:"title"`
	Detail   string `json:"detail"`
}

// History returns the points covering the given window, at the coarsest
// resolution that still answers the question with enough detail.
// History returns the points covering the given window.
//
// The level of detail is chosen from the length of the window and the
// cadence each level is written at, and trimmed if it turns out to hold
// more rows than the caller asked for. A coarser level is only used when
// it genuinely carries the window: if the aggregate table is nearly empty
// — which is the normal state of affairs for the first minutes after a
// restart, before aggregation has run — the individual samples are better
// than the handful of averages.
func (s *Store) History(ctx context.Context, from, to time.Time, maxPoints int) ([]model.HistoryPoint, HistoryInfo, error) {
	if maxPoints <= 0 {
		maxPoints = 600
	}
	info := HistoryInfo{From: from.UnixMilli(), To: to.UnixMilli()}

	// ts is stored in whole seconds. The window bounds are converted here
	// rather than passed as time values, because the driver would bind
	// those as formatted text and the comparison would quietly match
	// nothing at all.
	raw, overflow, err := s.read(ctx, Raw, from, to, maxPoints)
	if err != nil {
		return nil, info, err
	}
	if !overflow {
		// The individual samples fit in the budget, so they are always the
		// better answer: they are the same numbers the aggregates were
		// averaged from.
		info.Resolution = Raw
		return raw, info, nil
	}

	// Too many samples: try the aggregates, coarsest first, and take the
	// first level that both fits and has something to say.
	var best []model.HistoryPoint
	for _, res := range []Resolution{Minute, Hour} {
		points, over, err := s.read(ctx, res, from, to, maxPoints)
		if err != nil {
			return nil, info, err
		}
		if over {
			continue
		}
		if len(points) > 0 {
			info.Resolution = res
			return points, info, nil
		}
		best = points
	}
	if best == nil {
		best = []model.HistoryPoint{}
	}
	// Nothing at any level. Reporting raw resolution is the honest answer:
	// there is simply no history in this window yet.
	info.Resolution = Raw
	return best, info, nil
}

// HistoryInfo describes what a history query actually returned.
type HistoryInfo struct {
	// Resolution is the level the points came from: raw, minute or hour.
	Resolution Resolution `json:"resolution"`
	// From and To are the window that was queried, in Unix milliseconds.
	From int64 `json:"from"`
	To   int64 `json:"to"`
}

// read fetches one resolution, reporting whether the result was cut off by
// the point budget. It asks for one row more than allowed so that "too
// many" can be told apart from "exactly enough".
func (s *Store) read(ctx context.Context, res Resolution, from, to time.Time, maxPoints int) ([]model.HistoryPoint, bool, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT ts, cpu_pct, mem_pct, swap_pct, disk_pct, load1, rx_bps, tx_bps, procs,
       issues, crit, online, net_ms, temp_c, disk_read, disk_write
FROM samples
WHERE resolution = ? AND ts >= ? AND ts <= ?
ORDER BY ts
LIMIT ?`,
		string(res), from.Unix(), to.Unix(), maxPoints+1)
	if err != nil {
		return nil, false, fmt.Errorf("store: cannot read history: %w", err)
	}
	defer rows.Close()

	out := make([]model.HistoryPoint, 0, 256)
	for rows.Next() {
		var p model.HistoryPoint
		var online int
		if err := rows.Scan(&p.Time, &p.CPUPct, &p.MemPct, &p.SwapPct, &p.DiskPct,
			&p.Load1, &p.RxBps, &p.TxBps, &p.Procs, &p.Issues, &p.Crit, &online,
			&p.NetMS, &p.TempC, &p.DiskRead, &p.DiskWrit); err != nil {
			return nil, false, err
		}
		p.Time *= 1000
		p.Online = online != 0
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	if len(out) > maxPoints {
		return nil, true, nil
	}
	return out, false, nil
}

// Stats describes what the database currently holds, for the footer of
// the panel.
type Stats struct {
	Path    string `json:"path"`
	Size    int64  `json:"size"`
	Raw     int64  `json:"raw"`
	Minute  int64  `json:"minute"`
	Hour    int64  `json:"hour"`
	Oldest  int64  `json:"oldest"`
	Newest  int64  `json:"newest"`
	Events  int64  `json:"events"`
	Healthy bool   `json:"healthy"`
}

// Stats reports row counts and the covered time range.
func (s *Store) Stats(ctx context.Context) (Stats, error) {
	st := Stats{Path: s.opt.Path, Healthy: true}
	if fi, err := os.Stat(s.opt.Path); err == nil {
		st.Size = fi.Size()
	}
	for res, target := range map[Resolution]*int64{Raw: &st.Raw, Minute: &st.Minute, Hour: &st.Hour} {
		if err := s.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM samples WHERE resolution = ?`, string(res)).Scan(target); err != nil {
			return st, err
		}
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM events`).Scan(&st.Events); err != nil {
		return st, err
	}
	var oldest, newest sql.NullInt64
	if err := s.db.QueryRowContext(ctx,
		`SELECT MIN(ts), MAX(ts) FROM samples`).Scan(&oldest, &newest); err != nil {
		return st, err
	}
	if oldest.Valid {
		st.Oldest = oldest.Int64 * 1000
	}
	if newest.Valid {
		st.Newest = newest.Int64 * 1000
	}
	return st, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
