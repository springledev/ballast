package proxmox

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClientTest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "PVEAPIToken=ballast@pve!backup=secret-123" {
			t.Errorf("Authorization header = %q", got)
		}
		if r.URL.Path != "/api2/json/version" {
			t.Errorf("path = %q", r.URL.Path)
		}
		w.Write([]byte(`{"data":{"version":"8.2.1"}}`))
	}))
	defer srv.Close()

	cl := &Client{Host: srv.URL, TokenID: "ballast@pve!backup", TokenSecret: "secret-123", HTTPClient: srv.Client()}
	res, err := cl.Test(context.Background())
	if err != nil {
		t.Fatalf("Test() error: %v", err)
	}
	if res.Version != "8.2.1" {
		t.Errorf("Version = %q, want 8.2.1", res.Version)
	}
}

func TestClientTestUnauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	cl := &Client{Host: srv.URL, TokenID: "x@pve!y", TokenSecret: "z", HTTPClient: srv.Client()}
	_, err := cl.Test(context.Background())
	if err == nil || !strings.Contains(err.Error(), "rejected the API token") {
		t.Fatalf("err = %v, want a rejected-token error", err)
	}
}

func TestClientListGuests(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api2/json/cluster/resources" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if r.URL.Query().Get("type") != "vm" {
			t.Errorf("query = %q", r.URL.RawQuery)
		}
		w.Write([]byte(`{"data":[
			{"node":"pve1","vmid":100,"name":"immich","status":"running","type":"lxc"},
			{"node":"pve1","vmid":101,"name":"win-vm","status":"stopped","type":"qemu"},
			{"node":"pve1","vmid":100,"type":"storage"}
		]}`))
	}))
	defer srv.Close()

	cl := &Client{Host: srv.URL, TokenID: "x@pve!y", TokenSecret: "z", HTTPClient: srv.Client()}
	guests, err := cl.ListGuests(context.Background())
	if err != nil {
		t.Fatalf("ListGuests() error: %v", err)
	}
	if len(guests) != 2 {
		t.Fatalf("len(guests) = %d, want 2 (storage resource excluded): %+v", len(guests), guests)
	}
	if guests[0].VMID != 100 || !guests[0].IsLXC() || guests[0].Name != "immich" {
		t.Errorf("guests[0] = %+v", guests[0])
	}
	if guests[1].VMID != 101 || guests[1].IsLXC() || guests[1].Status != "stopped" {
		t.Errorf("guests[1] = %+v", guests[1])
	}
}

func TestClientRequestFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("boom"))
	}))
	defer srv.Close()

	cl := &Client{Host: srv.URL, TokenID: "x@pve!y", TokenSecret: "z", HTTPClient: srv.Client()}
	_, err := cl.Test(context.Background())
	if err == nil || !strings.Contains(err.Error(), "HTTP 500") {
		t.Fatalf("err = %v, want an HTTP 500 error", err)
	}
}

func TestWrapNetErrHints(t *testing.T) {
	tests := []struct {
		msg  string
		want string
	}{
		{"x509: certificate signed by unknown authority", "self-signed"},
		{"dial tcp: lookup pve.local: no such host", "could not resolve"},
		{"dial tcp 1.2.3.4:8006: connect: connection refused", "refused the connection"},
		{"context deadline exceeded", "no answer from"},
	}
	for _, tt := range tests {
		err := wrapNetErr("pve.local", errString(tt.msg))
		if !strings.Contains(err.Error(), tt.want) {
			t.Errorf("wrapNetErr(%q) = %v, want containing %q", tt.msg, err, tt.want)
		}
	}
}

type errString string

func (e errString) Error() string { return string(e) }
