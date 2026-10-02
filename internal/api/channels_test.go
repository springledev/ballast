package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/springledev/ballast/internal/inventory"
	"github.com/springledev/ballast/internal/notify"
)

func TestChannelsLifecycleAndSecrets(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	var gotAuth, gotTitle string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotTitle = r.Header.Get("Authorization"), r.Header.Get("Title")
	}))
	defer srv.Close()
	topicURL := srv.URL + "/very-secret-topic"

	v, err := h.ops.PutChannel(ctx, ChannelInput{Name: "phone", URL: topicURL, Token: "tk_secret", ApplyToAll: true})
	if err != nil {
		t.Fatal(err)
	}
	if !v.HasToken || strings.Contains(v.URLDisplay, "very-secret-topic") || v.URLDisplay == "" {
		t.Errorf("view = %+v", v)
	}
	// Rule 2: neither the API view nor the database holds a secret.
	raw, _ := json.Marshal(v)
	for _, s := range []string{"very-secret-topic", "tk_secret"} {
		if strings.Contains(string(raw), s) {
			t.Errorf("view leaks %q", s)
		}
		assertNotInDB(t, h.ops.DB, s)
	}
	if b, err := os.ReadFile(filepath.Join(h.credDir, "ballast-channel.phone.url")); err != nil || !strings.Contains(string(b), "very-secret-topic") {
		t.Errorf("url credential file: %q, %v", b, err)
	}

	if err := h.ops.TestChannel(ctx, "phone"); err != nil {
		t.Fatalf("TestChannel: %v", err)
	}
	if gotAuth != "Bearer tk_secret" || gotTitle != "Ballast test notification" {
		t.Errorf("auth %q title %q", gotAuth, gotTitle)
	}

	// Editing without retyping secrets keeps them.
	if _, err := h.ops.PutChannel(ctx, ChannelInput{Name: "phone", ApplyToAll: true}); err != nil {
		t.Fatal(err)
	}
	if err := h.ops.TestChannel(ctx, "phone"); err != nil || gotAuth != "Bearer tk_secret" {
		t.Errorf("secrets lost on edit: %v %q", err, gotAuth)
	}

	if err := h.ops.DeleteChannel(ctx, "phone"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(h.credDir, "ballast-channel.phone.url")); !os.IsNotExist(err) {
		t.Errorf("credential file not removed: %v", err)
	}
	if err := h.ops.TestChannel(ctx, "phone"); !errors.Is(err, inventory.ErrNotFound) {
		t.Errorf("test deleted channel err = %v", err)
	}
}

func TestPutChannelValidation(t *testing.T) {
	h := newHarness(t)
	tests := []struct {
		name string
		in   ChannelInput
	}{
		{"bad name", ChannelInput{Name: "Bad Name", URL: "https://ntfy.sh/x"}},
		{"bad url", ChannelInput{Name: "a", URL: "just-a-topic"}},
		{"missing url", ChannelInput{Name: "a"}},
		{"bad type", ChannelInput{Name: "a", Type: "smoke", URL: "https://ntfy.sh/x"}},
		{"unknown service", ChannelInput{Name: "a", URL: "https://ntfy.sh/x", Services: []string{"ghost"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := h.ops.PutChannel(context.Background(), tt.in)
			if err == nil {
				t.Fatal("want error")
			}
			if !errors.Is(err, ErrValidation) && !errors.Is(err, inventory.ErrNotFound) {
				t.Errorf("err = %v", err)
			}
		})
	}
}

func TestChannelTestFailureNamesChannelNotAddress(t *testing.T) {
	h := newHarness(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(401) }))
	defer srv.Close()
	if _, err := h.ops.PutChannel(context.Background(), ChannelInput{Name: "locked", URL: srv.URL + "/topic-xyz"}); err != nil {
		t.Fatal(err)
	}
	err := h.ops.TestChannel(context.Background(), "locked")
	if err == nil || !strings.Contains(err.Error(), "locked") || strings.Contains(err.Error(), "topic-xyz") {
		t.Errorf("err = %v", err)
	}
}

func assertNotInDB(t *testing.T, db *sql.DB, needle string) {
	t.Helper()
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type='table'`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var n string
		_ = rows.Scan(&n)
		tables = append(tables, n)
	}
	rows.Close()
	for _, tb := range tables {
		r, err := db.Query(`SELECT * FROM "` + tb + `"`)
		if err != nil {
			continue
		}
		cols, _ := r.Columns()
		for r.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			_ = r.Scan(ptrs...)
			for _, v := range vals {
				if strings.Contains(strings.ToLower(toString(v)), strings.ToLower(needle)) {
					t.Errorf("secret %q found in table %s", needle, tb)
				}
			}
		}
		r.Close()
	}
}

func toString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case []byte:
		return string(x)
	}
	return ""
}

func TestWebhookAndEmailChannels(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	defer srv.Close()

	v, err := h.ops.PutChannel(ctx, ChannelInput{Name: "hook", Type: "webhook", URL: srv.URL + "/secret-path", Token: "hmac-key", ApplyToAll: true, Events: []string{"job_failure"}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(v.URLDisplay, "secret-path") || !v.HasToken || len(v.Events) != 1 {
		t.Errorf("view = %+v", v)
	}
	detail, err := h.ops.TestChannelResult(ctx, "hook")
	if err != nil || !strings.Contains(detail, "204") {
		t.Fatalf("test detail = %q, %v", detail, err)
	}

	es := &notify.EmailSettings{Host: "smtp.example.com", Port: 587, Security: "starttls", From: "a@b.c", To: []string{"d@e.f"}}
	mv, err := h.ops.PutChannel(ctx, ChannelInput{Name: "mail", Type: "email", Email: es, Token: "mail-pw-secret", ApplyToAll: true})
	if err != nil {
		t.Fatal(err)
	}
	if mv.Email == nil || mv.URLDisplay != "smtp.example.com" {
		t.Errorf("mail view = %+v", mv)
	}
	b, _ := json.Marshal(mv)
	if strings.Contains(string(b), "mail-pw-secret") {
		t.Error("password in API view")
	}
	// Editing without mail settings or filters keeps the stored ones.
	mv, err = h.ops.PutChannel(ctx, ChannelInput{Name: "mail", ApplyToAll: true})
	if err != nil || mv.Email == nil || mv.Type != "email" {
		t.Fatalf("edit = %+v, %v", mv, err)
	}
	for name, in := range map[string]ChannelInput{
		"email needs settings":     {Name: "m2", Type: "email"},
		"email has no url":         {Name: "m3", Type: "email", Email: es, URL: "https://x.example"},
		"type cannot change":       {Name: "hook", Type: "email", Email: es},
		"unknown event is refused": {Name: "w2", Type: "webhook", URL: "https://x.example", Events: []string{"nope"}},
	} {
		if _, err := h.ops.PutChannel(ctx, in); !errors.Is(err, ErrValidation) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}
