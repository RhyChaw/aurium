// Package daemon runs auriumd: the single writer to the database and the only
// holder of secrets (D17).
package daemon

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/RhyChaw/aurium/internal/api"
	"github.com/RhyChaw/aurium/internal/app"
	"github.com/RhyChaw/aurium/internal/events"
)

// DefaultAddr is the loopback TCP address the daemon listens on.
const DefaultAddr = "127.0.0.1:7770"

// Daemon owns the listeners and the background workers.
type Daemon struct {
	App    *app.App
	Server *api.Server
	Log    *slog.Logger

	Addr       string
	SocketPath string
	TokenPath  string

	httpServer *http.Server
	listeners  []net.Listener
	watcher    *Watcher
}

// Options configure a daemon.
type Options struct {
	Addr    string
	Verbose bool
	Log     *slog.Logger
}

// New builds a daemon, creating the host token if it does not exist.
func New(o Options) (*Daemon, error) {
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.Addr == "" {
		o.Addr = DefaultAddr
	}

	a, err := app.Open(o.Verbose)
	if err != nil {
		return nil, err
	}

	token, err := ensureHostToken(filepath.Join(a.Home, "token"))
	if err != nil {
		a.Close()
		return nil, err
	}

	d := &Daemon{
		App:        a,
		Server:     api.New(a, token, o.Log),
		Log:        o.Log,
		Addr:       o.Addr,
		SocketPath: filepath.Join(a.Home, "auriumd.sock"),
		TokenPath:  filepath.Join(a.Home, "token"),
	}
	d.watcher = NewWatcher(a, o.Log)
	return d, nil
}

// ensureHostToken reads or creates ~/.aurium/token.
//
// Mode 0600 is the whole access-control story for host clients: anything that
// can read the file can drive Aurium, and nothing else can. That is the right
// boundary for a per-user daemon, and it is why the daemon binds loopback only.
func ensureHostToken(path string) (string, error) {
	if body, err := os.ReadFile(path); err == nil && len(body) > 0 {
		return string(body), nil
	}

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("daemon: generate host token: %w", err)
	}
	token := "aurh_" + base64.RawURLEncoding.EncodeToString(raw)

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(token), 0o600); err != nil {
		return "", fmt.Errorf("daemon: write host token: %w", err)
	}
	return token, nil
}

// Run starts the listeners and blocks until ctx is cancelled.
func (d *Daemon) Run(ctx context.Context) error {
	d.httpServer = &http.Server{
		Handler:           d.Server,
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: SSE connections are meant to stay open, and a
		// write deadline would sever the dashboard's event stream on a
		// schedule.
	}

	tcp, err := net.Listen("tcp", d.Addr)
	if err != nil {
		return fmt.Errorf("daemon: listen on %s: %w", d.Addr, err)
	}
	d.listeners = append(d.listeners, tcp)

	// A stale socket from a crashed daemon would block binding; removing it is
	// safe because a live daemon holds the port above, which would have failed
	// first.
	_ = os.Remove(d.SocketPath)
	if unix, err := net.Listen("unix", d.SocketPath); err == nil {
		if err := os.Chmod(d.SocketPath, 0o600); err != nil {
			d.Log.Warn("daemon: could not restrict socket permissions", "err", err)
		}
		d.listeners = append(d.listeners, unix)
	} else {
		d.Log.Warn("daemon: unix socket unavailable, TCP only", "err", err)
	}

	errs := make(chan error, len(d.listeners))
	for _, l := range d.listeners {
		go func(l net.Listener) {
			if err := d.httpServer.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errs <- err
			}
		}(l)
	}

	go d.watcher.Run(ctx)

	// Until every client goes through the API (D17), the CLI writes to the
	// same database in this process's blind spot. Tailing the log means the
	// dashboard sees what the user does at the terminal, which is most of what
	// is worth watching.
	go d.App.Events.Tail(ctx, events.DefaultTailInterval)

	if err := d.App.Events.Emit(ctx, events.Event{
		Type: events.DaemonStarted, Actor: events.ActorDaemon,
		Payload: map[string]any{"addr": d.Addr, "socket": d.SocketPath},
	}); err != nil {
		d.Log.Warn("daemon: could not record startup", "err", err)
	}
	d.Log.Info("aurium daemon listening", "addr", d.Addr, "socket", d.SocketPath)

	select {
	case <-ctx.Done():
		return d.Shutdown()
	case err := <-errs:
		d.Shutdown()
		return err
	}
}

// Shutdown closes listeners and the database.
func (d *Daemon) Shutdown() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if d.httpServer != nil {
		_ = d.httpServer.Shutdown(ctx)
	}
	_ = os.Remove(d.SocketPath)
	return d.App.Close()
}
