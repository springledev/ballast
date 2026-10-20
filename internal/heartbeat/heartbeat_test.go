package heartbeat

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type memMeta map[string]string

func (m memMeta) MetaSet(k, v string) error { m[k] = v; return nil }

func TestBeatInterval(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer srv.Close()
	meta := memMeta{}
	p := &Pinger{Credential: func() (string, error) { return srv.URL + "/ping", nil }, Interval: 5 * time.Minute, Meta: meta}
	t0 := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		at   time.Duration
		want int32
	}{{0, 1}, {time.Minute, 1}, {4 * time.Minute, 1}, {5 * time.Minute, 2}, {6 * time.Minute, 2}, {10 * time.Minute, 3}}
	for _, tt := range tests {
		p.Beat(context.Background(), t0.Add(tt.at))
		if got := hits.Load(); got != tt.want {
			t.Errorf("after beat at +%v: %d pings, want %d", tt.at, got, tt.want)
		}
	}
	if meta[MetaKeyLastOK] != t0.Add(10*time.Minute).Format(time.RFC3339) {
		t.Errorf("last ok = %q", meta[MetaKeyLastOK])
	}
}

func TestNoURLConfiguredDoesNothing(t *testing.T) {
	meta := memMeta{}
	p := &Pinger{Credential: func() (string, error) { return "", nil }, Meta: meta}
	p.Beat(context.Background(), time.Now())
	if len(meta) != 0 {
		t.Errorf("recorded %v with no heartbeat configured", meta)
	}
}

func TestFailuresDoNotRecordSuccessAndHideURL(t *testing.T) {
	const token = "SECRET-HEARTBEAT-TOKEN-do-not-leak-QQ77"
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }))
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close()
	good.Close() // closed: connection refused
	tests := map[string]string{
		"500":         bad.URL + "/ping/" + token,
		"refused":     deadURL + "/ping/" + token + "?a=" + token,
		"bad address": "http://exa mple.com/" + token,
		"bad scheme":  "ftp://" + token + "@host/" + token,
	}
	for name, u := range tests {
		t.Run(name, func(t *testing.T) {
			meta := memMeta{}
			var logged strings.Builder
			p := &Pinger{Credential: func() (string, error) { return u, nil }, Meta: meta,
				Log: func(f string, a ...any) { logged.WriteString(strings.TrimSpace(f)) }}
			err := p.Ping(context.Background(), time.Now())
			if err == nil {
				t.Fatal("want an error")
			}
			if strings.Contains(err.Error(), token) {
				t.Errorf("error leaks the token: %v", err)
			}
			if len(meta) != 0 {
				t.Errorf("failure recorded as success: %v", meta)
			}
		})
	}
	bad.Close()
}

func TestCanceledContextStopsPings(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer srv.Close()
	p := &Pinger{Credential: func() (string, error) { return srv.URL, nil }}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p.Beat(ctx, time.Now())
	if hits.Load() != 0 {
		t.Error("pinged with a canceled context")
	}
}
