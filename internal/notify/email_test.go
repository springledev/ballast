package notify

import (
	"bufio"
	"context"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeSMTP accepts one plain SMTP session and returns what it saw.
func fakeSMTP(t *testing.T, rejectAuth bool) (port int, got chan string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	got = make(chan string, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		r := bufio.NewReader(c)
		var seen strings.Builder
		say := func(s string) { c.Write([]byte(s + "\r\n")) } //nolint:errcheck
		say("220 fake ESMTP")
		inData := false
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				break
			}
			seen.WriteString(line)
			up := strings.ToUpper(strings.TrimSpace(line))
			switch {
			case inData:
				if strings.TrimSpace(line) == "." {
					inData = false
					say("250 queued")
				}
			case strings.HasPrefix(up, "EHLO"):
				say("250-fake\r\n250 AUTH PLAIN")
			case strings.HasPrefix(up, "AUTH"):
				if rejectAuth {
					say("535 bad credentials")
				} else {
					say("235 ok")
				}
			case up == "DATA":
				inData = true
				say("354 go")
			case up == "QUIT":
				say("221 bye")
				got <- seen.String()
				return
			default:
				say("250 ok")
			}
		}
		got <- seen.String()
	}()
	return ln.Addr().(*net.TCPAddr).Port, got
}

func TestEmailChannelSend(t *testing.T) {
	tests := []struct {
		name       string
		password   string
		rejectAuth bool
		wantErr    string
	}{
		{"delivers", "", false, ""},
		{"delivers with login", "hunter2", false, ""},
		{"bad login names the user, not the password", "hunter2", true, "rejected the login"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			port, got := fakeSMTP(t, tt.rejectAuth)
			e := &EmailChannel{
				EmailSettings: EmailSettings{Host: "127.0.0.1", Port: port, Security: EmailSecurityNone, From: "ballast@example.com", To: []string{"me@example.com"}},
				Username:      "me", Password: tt.password,
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			err := e.Send(ctx, Event{Service: "immich", Type: EventJobFailure, Message: "immich backup failed", Detail: "disk full", Timestamp: time.Now()})
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				if strings.Contains(err.Error(), tt.password) {
					t.Errorf("error leaks password: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Send: %v", err)
			}
			seen := <-got
			for _, want := range []string{"MAIL FROM:<ballast@example.com>", "RCPT TO:<me@example.com>", "Subject: [Ballast] immich backup failed", "disk full"} {
				if !strings.Contains(seen, want) {
					t.Errorf("session missing %q:\n%s", want, seen)
				}
			}
		})
	}
}

func TestEmailChannelUnreachable(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	e := &EmailChannel{EmailSettings: EmailSettings{Host: "127.0.0.1", Port: port, Security: EmailSecurityNone, From: "a@b.c", To: []string{"d@e.f"}}}
	err := e.Send(context.Background(), Event{Service: "x"})
	if err == nil || !strings.Contains(err.Error(), "connecting to mail server 127.0.0.1:"+strconv.Itoa(port)) {
		t.Fatalf("err = %v", err)
	}
}

func TestEmailSettingsValidate(t *testing.T) {
	good := EmailSettings{Host: "h", Port: 587, Security: "starttls", From: "a@b", To: []string{"c@d"}}
	tests := []struct {
		name string
		mut  func(*EmailSettings)
	}{
		{"no host", func(s *EmailSettings) { s.Host = " " }},
		{"bad port", func(s *EmailSettings) { s.Port = 0 }},
		{"bad security", func(s *EmailSettings) { s.Security = "ssl" }},
		{"bad from", func(s *EmailSettings) { s.From = "nope" }},
		{"no recipients", func(s *EmailSettings) { s.To = nil }},
		{"bad recipient", func(s *EmailSettings) { s.To = []string{"nope"} }},
	}
	if err := good.Validate(); err != nil {
		t.Fatalf("good settings: %v", err)
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := good
			tt.mut(&s)
			if err := s.Validate(); err == nil {
				t.Fatal("want error")
			}
		})
	}
}
