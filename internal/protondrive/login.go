package protondrive

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
)

// LoginTimeout is how long a sign-in may stay open.
const LoginTimeout = 10 * time.Minute

var (
	// Both need a character after them: output arrives in pieces, and a
	// link or code still being written must not be taken half-read.
	linkRE = regexp.MustCompile(`https://[^\s"'<>]+\s`)
	codeRE = regexp.MustCompile(`(?i)\bcode\b[^A-Za-z0-9]{0,4}([A-Z0-9]{3,}(?:-[A-Z0-9]+)*)\s`)
)

// LoginInfo is what the UI shows during a sign-in.
type LoginInfo struct {
	// Link is the Proton page to open (on any device).
	Link string `json:"link,omitempty"`
	// Code is the confirmation code the page asks for, when there is one.
	Code string `json:"code,omitempty"`
	// Done is true when the CLI has exited.
	Done bool `json:"done"`
	// Error is why the sign-in did not complete, in plain English.
	Error string `json:"error,omitempty"`
}

// Login is a sign-in in progress: the CLI's own browser sign-in, run as
// a subprocess whose link Ballast shows. The password and 2FA code are
// entered on Proton's page and never pass through Ballast.
type Login struct {
	mu     sync.Mutex
	info   LoginInfo
	buf    strings.Builder
	cancel context.CancelFunc
}

// Write receives the CLI's output as it runs and picks out the link and
// code. It makes Login an io.Writer for runner.Run.
func (l *Login) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.buf.Write(p)
	text := l.buf.String()
	l.scan(text)
	return len(p), nil
}

// scan picks the link and code out of text (mu held).
func (l *Login) scan(text string) {
	if l.info.Link == "" {
		l.info.Link = strings.TrimRight(linkRE.FindString(text), ".,;)\n\r\t ")
	}
	if l.info.Code == "" {
		if m := codeRE.FindStringSubmatch(text); m != nil {
			l.info.Code = m[1]
		}
	}
}

// Info returns the current state of the sign-in.
func (l *Login) Info() LoginInfo {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.info
}

// Cancel abandons the sign-in.
func (l *Login) Cancel() { l.cancel() }

func (l *Login) finish(err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.info.Done = true
	l.scan(l.buf.String() + "\n") // the last line may have had no newline
	if err != nil {
		l.info.Error = err.Error()
	}
}

// StartLogin starts `auth login` in the background and returns at once;
// poll Login.Info for the link. When the CLI exits cleanly the sign-in
// is proven by opening (or creating) folder. The sign-in is not tied to
// any request: it ends when finished, cancelled or after LoginTimeout.
func (c *Client) StartLogin(folder string) (*Login, error) {
	if err := c.ensureState(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), LoginTimeout)
	l := &Login{cancel: cancel}
	go func() {
		defer cancel()
		release, err := c.Lock(ctx)
		if err != nil {
			l.finish(err)
			return
		}
		// os/exec rather than the runner: the link may arrive on either
		// stream and must be read while the command is still running.
		cmd := exec.CommandContext(ctx, c.bin(), "auth", "login")
		cmd.Env = append(os.Environ(), c.env()...)
		cmd.Stdout, cmd.Stderr = l, l
		err = cmd.Run()
		release()
		switch {
		case err != nil && (errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist)):
			l.finish(fmt.Errorf("%w (install it from %s)", ErrNotInstalled, DownloadURL))
		case err != nil && ctx.Err() != nil:
			l.finish(fmt.Errorf("the sign-in was not finished in time or was cancelled"))
		case err != nil:
			l.finish(fmt.Errorf("Proton Drive sign-in did not complete: %w", err))
		default:
			l.finish(c.TestConnection(context.Background(), folder))
		}
	}()
	return l, nil
}
