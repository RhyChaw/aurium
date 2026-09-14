package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/RhyChaw/aurium/internal/daemon"
	"github.com/spf13/cobra"
)

// `aurium up` is the one command.
//
// Everything Aurium does was already reachable — `aurium daemon start`, then
// `aurium dashboard` — but "two commands and you have to know the order" is a
// tool people bounce off in the first minute. This runs the daemon in the
// foreground and opens the dashboard once it answers, so Ctrl-C stops the
// thing you started, which is what a command that says "up" should mean.
//
// The distinction from `aurium dashboard` is deliberate and kept: that one
// detaches, which is right when you want the daemon to outlive the terminal.

func newUpCmd() *cobra.Command {
	var addr string
	var noOpen bool
	var detach bool
	var restart bool

	cmd := &cobra.Command{
		Use:   "up",
		Short: "Start Aurium and open the dashboard",
		Long: "Runs the daemon in this terminal and opens the dashboard when it answers.\n" +
			"Ctrl-C stops it.\n\n" +
			"Use --detach to leave the daemon running after this command returns, which\n" +
			"is what `aurium dashboard` does on its own.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if addr == "" {
				addr = daemon.DefaultAddr
			}
			url := "http://" + addr + "/"

			// Already running — from a previous `aurium up`, or because any
			// CLI command auto-started it. Starting a second daemon would fail
			// on the port, and that is a confusing way to learn this.
			if health, err := DaemonHealth(addr); err == nil {
				if !restart {
					reportRunning(health, addr, url)
					if !noOpen {
						_ = openBrowser(url)
					}
					return nil
				}
				if err := StopDaemon(addr); err != nil {
					return exitf(CodeUsage, "%v", err)
				}
				fmt.Printf("Stopped the daemon that was on %s.\n", addr)
			}

			if detach {
				if err := StartDaemon(addr); err != nil {
					return wrap(CodeUsage, err)
				}
				fmt.Printf("auriumd listening on %s\n", addr)
				fmt.Println(url)
				if !noOpen {
					_ = openBrowser(url)
				}
				return nil
			}

			d, err := daemon.New(daemon.Options{Addr: addr, Verbose: flagVerbose})
			if err != nil {
				return wrap(CodeUsage, err)
			}

			// SIGINT and SIGTERM cancel the daemon's context so it closes its
			// listeners, removes its socket and closes the database, rather
			// than being killed mid-write and leaving a stale socket behind.
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			// Opening the browser waits for health rather than firing
			// immediately: a page loaded a beat before the listener is up
			// shows a connection error, and the user's instinct is then to
			// assume the command failed.
			if !noOpen {
				go openWhenHealthy(ctx, addr, url)
			}

			fmt.Printf("Aurium is starting on %s\n", url)
			fmt.Println("Ctrl-C to stop.")
			if err := d.Run(ctx); err != nil {
				return wrap(CodeUsage, err)
			}
			fmt.Println("\nStopped.")
			return nil
		},
	}
	cmd.Flags().StringVar(&addr, "addr", daemon.DefaultAddr, "loopback address")
	cmd.Flags().BoolVar(&noOpen, "no-open", false, "do not open a browser")
	cmd.Flags().BoolVar(&detach, "detach", false,
		"leave the daemon running after this command returns")
	cmd.Flags().BoolVar(&restart, "restart", false,
		"stop whatever is already on this address and start fresh")
	return cmd
}

// reportRunning says what is already there, and how old it is.
//
// The age is the point. A dev build reports the same version string across
// every rebuild, so "auriumd 0.1.0-dev is already running" is exactly as true
// of the daemon you started ten seconds ago as of the one you left running on
// Tuesday — and only the second one will be missing the feature you just built
// and are now looking for.
func reportRunning(health map[string]any, addr, url string) {
	fmt.Printf("auriumd %v is already running on %s", health["version"], addr)
	if pid, ok := health["pid"]; ok {
		fmt.Printf(" (pid %v", pid)
		if up, ok := health["uptime"].(string); ok {
			fmt.Printf(", up %s", up)
		}
		fmt.Print(")")
	}
	fmt.Println()

	if why := stale(health); why != "" {
		fmt.Println()
		fmt.Printf("%s.\n", why)
		fmt.Println("It is serving its own dashboard and API, not this binary's.")
		fmt.Println("Replace it with:  aurium up --restart")
		fmt.Println()
	}
	fmt.Println(url)
}

// staleAfter is when a running daemon stops being obviously the one you just
// started. An hour is arbitrary but errs the safe way: a false warning costs a
// line of output, a missing one costs an afternoon.
const staleAfter = time.Hour

// stale explains why the running daemon is probably not this build, or returns
// "" if there is no reason to think so.
func stale(health map[string]any) string {
	// Every build that has this check also reports a pid. A daemon that does
	// not report one is therefore older than this binary — which is a
	// certainty rather than the guess the age heuristic below makes, and it is
	// the case that actually bites: a daemon left running across a rebuild.
	if _, ok := health["pid"]; !ok {
		return "That daemon is too old to report its own pid, so it predates this build"
	}

	ts, ok := health["started_at"].(string)
	if !ok {
		return ""
	}
	started, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return ""
	}
	if age := time.Since(started); age > staleAfter {
		return fmt.Sprintf("That daemon has been up for %s, so it may predate the build you just made",
			age.Truncate(time.Minute))
	}
	return ""
}

// openWhenHealthy polls until the daemon answers, then opens the dashboard.
//
// It gives up quietly after the deadline. The URL is already on stdout, so a
// machine with no browser — a server, a container, a CI run — loses nothing by
// this failing, and an error about it would be noise.
func openWhenHealthy(ctx context.Context, addr, url string) {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return
		case <-time.After(150 * time.Millisecond):
		}
		if _, err := DaemonHealth(addr); err == nil {
			fmt.Println(url)
			_ = openBrowser(url)
			return
		}
	}
}
