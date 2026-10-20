package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/springledev/ballast/internal/engine"
	"github.com/springledev/ballast/internal/heartbeat"
)

// driveLoop runs runScheduleLoop with a ticker the test controls and
// returns the tick channel plus a function that stops the loop and
// waits for it.
func driveLoop(t *testing.T, a *app) (chan time.Time, func()) {
	t.Helper()
	ticks := make(chan time.Time)
	old := scheduleTicker
	scheduleTicker = func() (<-chan time.Time, func()) { return ticks, func() {} }
	t.Cleanup(func() { scheduleTicker = old })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); runScheduleLoop(ctx, a) }()
	var once sync.Once
	stop := func() { once.Do(func() { cancel(); <-done }) }
	t.Cleanup(stop) // runs before the ticker is restored (LIFO)
	return ticks, stop
}

// TestHeartbeatFollowsSchedulerLoop proves pings happen on loop ticks at
// the configured interval and stop once the loop is stopped.
func TestHeartbeatFollowsSchedulerLoop(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer srv.Close()
	a := sweepApp(t, engine.SUCCESS)
	a.logw = &syncBuf{}
	a.heartbeat = &heartbeat.Pinger{Credential: func() (string, error) { return srv.URL, nil }, Interval: 5 * time.Minute, Meta: a.store, Log: a.logf}

	ticks, stop := driveLoop(t, a)
	t0 := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 7; i++ { // minutes 0..6
		ticks <- t0.Add(time.Duration(i) * time.Minute)
		// Wait for each expected ping so the goroutines the loop starts
		// per tick cannot reorder and skew the interval arithmetic.
		switch i {
		case 0:
			waitFor(t, func() bool { return hits.Load() >= 1 })
		case 5:
			waitFor(t, func() bool { return hits.Load() >= 2 })
		}
	}
	stop() // waits for every goroutine the loop started
	if got := hits.Load(); got != 2 {
		t.Fatalf("pings while loop ran = %d, want 2 (minute 0 and minute 5)", got)
	}
	if _, ok, _ := heartbeat.LastOK(a.store.DB()); !ok {
		t.Error("last success not recorded")
	}

	// The loop is stopped: nothing may ping any more.
	before := hits.Load()
	select {
	case ticks <- t0.Add(30 * time.Minute):
		t.Fatal("stopped loop still reads ticks")
	case <-time.After(50 * time.Millisecond):
	}
	if hits.Load() != before {
		t.Error("heartbeat kept pinging after the loop stopped")
	}
}

// TestHeartbeatFailureNeverAffectsBackups proves a dead monitor does not
// change a run's outcome, and that the URL never appears in logs, the
// database or the doctor report (spec rule 2).
func TestHeartbeatFailureNeverAffectsBackups(t *testing.T) {
	const token = "SECRET-HEARTBEAT-TOKEN-do-not-leak-QQ77"
	for name, status := range map[string]int{"monitor down": 0, "monitor errors": 500} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status) }))
			url := srv.URL + "/ping/" + token + "?k=" + token
			if status == 0 {
				srv.Close()
			} else {
				defer srv.Close()
			}
			a := sweepApp(t, engine.SUCCESS)
			a.logw = &syncBuf{}
			a.heartbeat = &heartbeat.Pinger{Credential: func() (string, error) { return url, nil }, Meta: a.store, Log: a.logf}

			due := time.Date(2026, 3, 5, 3, 0, 20, 0, time.UTC)
			ticks, stop := driveLoop(t, a)
			ticks <- due
			waitFor(t, func() bool {
				runs, _ := engine.RecentRuns(a.store.DB(), "svc", 5)
				return len(runs) == 1 && runs[0].State == engine.SUCCESS && strings.Contains(a.logw.(*syncBuf).String(), "heartbeat:")
			})
			stop()

			runs, err := engine.RecentRuns(a.store.DB(), "svc", 5)
			if err != nil || len(runs) != 1 || runs[0].State != engine.SUCCESS {
				t.Fatalf("runs = %+v, err = %v; want one SUCCESS run", runs, err)
			}
			if !strings.Contains(a.logw.(*syncBuf).String(), "heartbeat:") {
				t.Errorf("failed ping was not logged: %q", a.logw)
			}
			if _, ok, _ := heartbeat.LastOK(a.store.DB()); ok {
				t.Error("failed ping recorded as success")
			}

			surfaces := map[string]string{"log": a.logw.(*syncBuf).String()}
			rows, err := a.store.DB().Query(`SELECT value FROM meta`)
			if err != nil {
				t.Fatal(err)
			}
			for rows.Next() {
				var v string
				_ = rows.Scan(&v)
				surfaces["meta"] += v + "\n"
			}
			rows.Close()
			rep, _ := a.ops.Doctor(context.Background())
			b, _ := json.Marshal(rep)
			surfaces["doctor"] = string(b)
			for where, text := range surfaces {
				if strings.Contains(text, token) {
					t.Errorf("heartbeat token leaked into %s: %s", where, text)
				}
			}
		})
	}
}

// waitFor polls cond for up to five seconds.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for i := 0; i < 500; i++ {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met in time")
}

// syncBuf is a log sink safe for the concurrent sweeps a loop starts.
type syncBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }
