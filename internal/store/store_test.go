package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/nelvixboy/eye-of-lotus/internal/model"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(Options{
		Path:         filepath.Join(t.TempDir(), "history.db"),
		RetainRaw:    time.Hour,
		RetainMinute: 24 * time.Hour,
		RetainHour:   48 * time.Hour,
		// Tests write every sample; the throttle has its own test below.
		WriteInterval: -1,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func sample(at time.Time, cpu, mem, disk float64) model.Snapshot {
	return model.Snapshot{
		Time: at.UnixMilli(),
		CPU:  model.CPU{Usage: cpu, Load1: 1.5},
		Memory: model.Memory{
			UsedPct: mem, Total: 1000, Used: uint64(mem * 10), SwapPct: 12,
		},
		Disks:      []model.Disk{{Mount: "/", UsedPct: disk, ReadBps: 1000, WriteBps: 2000}},
		Net:        model.Network{Online: true, LatencyMS: 12, RxBps: 5000, TxBps: 900},
		TotalProcs: 214,
		TempC:      47.5,
		Issues: []model.Issue{
			{ID: "disk.high", Severity: model.SevWarn, Title: "Filesystem filling up"},
			{ID: "cpu.load_high", Severity: model.SevWarn, Title: "Load average is elevated"},
		},
	}
}

func TestRecordAndHistory(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	now := time.Now()
	for i := 0; i < 3; i++ {
		if err := s.Record(ctx, sample(now.Add(time.Duration(i-3)*time.Minute), 10+float64(i), 40+float64(i), 50+float64(i))); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}

	points, _, err := s.History(ctx, now.Add(-15*time.Minute), now, 600)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(points) != 3 {
		t.Fatalf("expected 3 points, got %d: %+v", len(points), points)
	}
	first := points[0]
	if first.CPUPct != 10 || first.MemPct != 40 || first.DiskPct != 50 {
		t.Errorf("first point carries wrong values: %+v", first)
	}
	if first.Load1 != 1.5 || first.Procs != 214 || first.TempC != 47.5 {
		t.Errorf("scalar fields wrong: %+v", first)
	}
	if first.DiskRead != 1000 || first.DiskWrit != 2000 {
		t.Errorf("disk throughput wrong: %+v", first)
	}
	if first.Issues != 2 || first.Crit != 0 {
		t.Errorf("issue counts wrong: %+v", first)
	}
	if !first.Online {
		t.Errorf("online flag lost: %+v", first)
	}
	// Timestamps come back in milliseconds, ready for JavaScript.
	if first.Time < now.Add(-4*time.Minute).UnixMilli() {
		t.Errorf("timestamp not converted to milliseconds: %d", first.Time)
	}
}

func TestHistoryWindowExcludesOldPoints(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	now := time.Now()

	if err := s.Record(ctx, sample(now.Add(-3*time.Hour), 5, 10, 20)); err != nil {
		t.Fatal(err)
	}
	if err := s.Record(ctx, sample(now, 50, 60, 70)); err != nil {
		t.Fatal(err)
	}

	recent, _, err := s.History(ctx, now.Add(-10*time.Minute), now, 600)
	if err != nil {
		t.Fatal(err)
	}
	if len(recent) != 1 {
		t.Fatalf("expected only the recent point, got %d", len(recent))
	}
	if recent[0].CPUPct != 50 {
		t.Errorf("wrong point returned: %+v", recent[0])
	}
}

func TestRollupThenHistory(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	now := time.Now()

	// Twelve raw samples inside one finished minute bucket.
	for i := 0; i < 12; i++ {
		at := now.Add(-2 * time.Minute).Add(time.Duration(i) * 3 * time.Second)
		if err := s.Record(ctx, sample(at, 20, 50, 60)); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.aggregate(ctx); err != nil {
		t.Fatalf("aggregate: %v", err)
	}

	stats, err := s.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Minute == 0 {
		t.Fatalf("rollup produced no minute rows: %+v", stats)
	}
	if stats.Raw != 12 {
		t.Errorf("raw rows = %d, want 12", stats.Raw)
	}

	// A four hour window is answered from the minute resolution.
	points, _, err := s.History(ctx, now.Add(-4*time.Hour), now, 600)
	if err != nil {
		t.Fatal(err)
	}
	if len(points) == 0 {
		t.Fatal("history returned nothing after rollup")
	}
	if points[0].CPUPct < 19 || points[0].CPUPct > 21 {
		t.Errorf("averaged CPU = %v, want about 20", points[0].CPUPct)
	}
}

func TestRecordEventsOnlyReportsChanges(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	now := time.Now()
	snap := sample(now, 10, 40, 50)

	seen := map[string]bool{}
	if err := s.RecordEvents(ctx, snap, seen); err != nil {
		t.Fatal(err)
	}
	events, err := s.Events(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}

	// The same findings on the next sample must not be recorded again.
	for _, i := range snap.Issues {
		seen[i.ID+i.Subject] = true
	}
	if err := s.RecordEvents(ctx, snap, seen); err != nil {
		t.Fatal(err)
	}
	events, _ = s.Events(ctx, 10)
	if len(events) != 2 {
		t.Fatalf("a steady problem was recorded twice: %d events", len(events))
	}
}

func TestPruneRemovesExpiredRows(t *testing.T) {
	ctx := context.Background()
	s, err := Open(Options{
		Path:          filepath.Join(t.TempDir(), "history.db"),
		RetainRaw:     time.Minute,
		RetainMinute:  time.Minute,
		RetainHour:    time.Minute,
		WriteInterval: -1,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if err := s.Record(ctx, sample(time.Now().Add(-48*time.Hour), 10, 20, 30)); err != nil {
		t.Fatal(err)
	}
	if err := s.prune(ctx); err != nil {
		t.Fatalf("prune: %v", err)
	}
	stats, err := s.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Raw != 0 {
		t.Errorf("expired rows survived pruning: %+v", stats)
	}
}

func TestHistoryAnswersFromRawWhenAggregatesAreMissing(t *testing.T) {
	// Immediately after a restart the coarse resolutions are empty while
	// the raw table still holds the recent past. A one hour request must
	// answer with raw detail rather than with nothing.
	ctx := context.Background()
	s := newTestStore(t)
	now := time.Now()
	for i := 0; i < 4; i++ {
		if err := s.Record(ctx, sample(now.Add(time.Duration(i-4)*time.Minute), 30, 40, 50)); err != nil {
			t.Fatal(err)
		}
	}
	points, info, err := s.History(ctx, now.Add(-time.Hour), now, 360)
	if err != nil {
		t.Fatal(err)
	}
	if len(points) != 4 {
		t.Fatalf("expected the raw rows to be used, got %d points", len(points))
	}
	if info.Resolution != Raw {
		t.Errorf("expected raw resolution, got %s", info.Resolution)
	}
}

func TestHistoryPrefersSamplesWhenTheyFit(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	now := time.Now()
	for i := 0; i < 5; i++ {
		if err := s.Record(ctx, sample(now.Add(time.Duration(i-5)*time.Minute), 30, 40, 50)); err != nil {
			t.Fatal(err)
		}
	}
	points, info, err := s.History(ctx, now.Add(-time.Hour), now, 360)
	if err != nil {
		t.Fatal(err)
	}
	if info.Resolution != Raw {
		t.Errorf("individual samples fit the budget, so raw was expected, got %s", info.Resolution)
	}
	if len(points) != 5 {
		t.Errorf("expected 5 points, got %d", len(points))
	}
	if info.From >= info.To {
		t.Errorf("window bounds look wrong: %+v", info)
	}
}

func TestHistoryAggregatesWhenSamplesOverflow(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	now := time.Now()

	// Ten minutes of samples five seconds apart is 120 rows: far more than
	// the caller allows, so the answer must come from the minute table.
	for i := 0; i < 120; i++ {
		at := now.Add(-10*time.Minute + time.Duration(i)*5*time.Second)
		if err := s.Record(ctx, sample(at, 20, 40, 50)); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.aggregate(ctx); err != nil {
		t.Fatal(err)
	}

	points, info, err := s.History(ctx, now.Add(-10*time.Minute), now, 10)
	if err != nil {
		t.Fatal(err)
	}
	if info.Resolution != Minute {
		t.Errorf("expected the minute level, got %s", info.Resolution)
	}
	if len(points) == 0 || len(points) > 10 {
		t.Errorf("point budget ignored: %d points for a budget of 10", len(points))
	}
	for _, p := range points {
		if p.CPUPct < 19 || p.CPUPct > 21 {
			t.Errorf("averaged CPU = %v, want about 20", p.CPUPct)
			break
		}
	}
}

func TestWriteThrottleFollowsSampleTime(t *testing.T) {
	ctx := context.Background()
	s, err := Open(Options{
		Path:          filepath.Join(t.TempDir(), "throttle.db"),
		WriteInterval: 5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now()

	// Two samples two seconds apart, with a five second cadence: the
	// second must be dropped, and the third, four seconds later, kept.
	for _, offset := range []time.Duration{-10 * time.Second, -8 * time.Second, -4 * time.Second} {
		if err := s.Record(ctx, sample(now.Add(offset), 10, 20, 30)); err != nil {
			t.Fatal(err)
		}
	}
	var raw int64
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM samples WHERE resolution = 'raw'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if raw != 2 {
		t.Errorf("raw rows = %d, want 2 (the sample inside the cadence must be dropped)", raw)
	}
}
