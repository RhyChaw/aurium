package cli

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/RhyChaw/aurium/internal/daemon"
	"github.com/spf13/cobra"
)

func newDaemonCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "daemon",
		Short: "Manage auriumd",
		Long:  "The daemon is normally started automatically; these commands are for when it is not.",
	}

	var addr string
	var foreground bool
	start := &cobra.Command{
		Use:   "start",
		Short: "Start the daemon",
		RunE: func(cmd *cobra.Command, args []string) error {
			if foreground {
				d, err := daemon.New(daemon.Options{Addr: addr, Verbose: flagVerbose})
				if err != nil {
					return wrap(CodeUsage, err)
				}
				return d.Run(cmd.Context())
			}
			if err := StartDaemon(addr); err != nil {
				return wrap(CodeUsage, err)
			}
			fmt.Printf("auriumd listening on %s\n", addr)
			return nil
		},
	}
	start.Flags().StringVar(&addr, "addr", daemon.DefaultAddr, "loopback address")
	start.Flags().BoolVar(&foreground, "foreground", false, "run in the foreground")

	stop := &cobra.Command{
		Use:   "stop",
		Short: "Stop the daemon listening on this address",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := StopDaemon(addr); err != nil {
				return exitf(CodeUsage, "%v", err)
			}
			fmt.Printf("auriumd on %s stopped\n", addr)
			return nil
		},
	}
	stop.Flags().StringVar(&addr, "addr", daemon.DefaultAddr, "loopback address")

	status := &cobra.Command{
		Use:   "status",
		Short: "Report whether the daemon is running",
		RunE: func(cmd *cobra.Command, args []string) error {
			health, err := DaemonHealth(addr)
			if err != nil {
				fmt.Printf("auriumd is not running (%v)\n", err)
				return &ExitError{Code: CodeUsage}
			}
			if emit(health) {
				return nil
			}
			fmt.Printf("auriumd %s on %s\n", health["version"], addr)
			return nil
		},
	}
	status.Flags().StringVar(&addr, "addr", daemon.DefaultAddr, "loopback address")

	cmd.AddCommand(start, stop, status)
	return cmd
}

func newDashboardCmd() *cobra.Command {
	var addr string
	var noOpen bool

	return &cobra.Command{
		Use:   "dashboard",
		Short: "Open the web dashboard",
		RunE: func(cmd *cobra.Command, args []string) error {
			if addr == "" {
				addr = daemon.DefaultAddr
			}
			if _, err := DaemonHealth(addr); err != nil {
				if err := StartDaemon(addr); err != nil {
					return wrap(CodeUsage, err)
				}
			}
			url := "http://" + addr + "/"
			fmt.Println(url)
			if noOpen {
				return nil
			}
			return openBrowser(url)
		},
	}
}

// StartDaemon spawns auriumd detached and waits for it to answer.
//
// The CLI starts the daemon rather than asking the user to, because a tool
// that requires a background process to be running before it works is a tool
// people abandon.
func StartDaemon(addr string) error {
	if addr == "" {
		addr = daemon.DefaultAddr
	}
	if _, err := DaemonHealth(addr); err == nil {
		return nil // already up
	}

	bin, err := os.Executable()
	if err != nil {
		return err
	}
	// auriumd sits next to aurium in any normal install.
	candidate := filepath.Join(filepath.Dir(bin), "auriumd")
	if _, err := os.Stat(candidate); err != nil {
		if found, lookErr := exec.LookPath("auriumd"); lookErr == nil {
			candidate = found
		} else {
			return fmt.Errorf("cannot find auriumd next to %s or on PATH; "+
				"build it with `make build`", bin)
		}
	}

	proc := exec.Command(candidate, "--addr", addr)
	proc.Stdout, proc.Stderr = nil, nil
	// Detach: the daemon must outlive the CLI invocation that started it.
	proc.SysProcAttr = detachAttr()
	if err := proc.Start(); err != nil {
		return fmt.Errorf("starting auriumd: %w", err)
	}
	_ = proc.Process.Release()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := DaemonHealth(addr); err == nil {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("auriumd did not become healthy within 5s")
}

// StopDaemon asks the daemon on addr to shut down, and waits for it to.
//
// SIGTERM rather than SIGKILL: the daemon's own handler closes its listeners,
// removes its socket and closes the database. Killing it outright would leave
// a stale socket and a database mid-write, which is the state `aurium up` then
// has to explain.
//
// The pid comes from /v1/health, which is unauthenticated by design — but this
// only ever signals a process on loopback that answered as auriumd, and the
// daemon is per-user, so the signal is one the caller could send anyway.
func StopDaemon(addr string) error {
	if addr == "" {
		addr = daemon.DefaultAddr
	}
	health, err := DaemonHealth(addr)
	if err != nil {
		return fmt.Errorf("nothing is listening on %s", addr)
	}

	pid := 0
	if raw, ok := health["pid"].(float64); ok && raw > 0 { // JSON numbers decode as float64
		pid = int(raw)
	} else {
		// A daemon old enough not to report its pid is precisely the one worth
		// replacing, so falling back to asking the OS who holds the port is
		// what makes --restart work in the case it exists for.
		pid = pidOnPort(addr)
		if pid == 0 {
			return fmt.Errorf("the daemon on %s reports no pid and nothing could be "+
				"found holding that port; stop it by hand (`ps aux | grep auriumd`)", addr)
		}
	}

	proc, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("finding auriumd (pid %d): %w", pid, err)
	}
	if err := proc.Signal(syscall.SIGTERM); err != nil {
		return fmt.Errorf("stopping auriumd (pid %d): %w", pid, err)
	}

	// Wait for the listener to actually go, so a caller that starts a
	// replacement does not race the old one for the port.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := DaemonHealth(addr); err != nil {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("auriumd (pid %d) did not stop within 10s", pid)
}

// pidOnPort asks the OS which process is listening on addr's port.
//
// Shelling out to lsof rather than reading /proc or a platform API: this is a
// fallback for an old daemon on a developer's machine, and a failure here is
// reported rather than fatal.
func pidOnPort(addr string) int {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return 0
	}
	out, err := exec.Command("lsof", "-ti", "tcp:"+port, "-sTCP:LISTEN").Output()
	if err != nil {
		return 0
	}
	// Several lines if several processes hold it; the first is enough, and a
	// second pass of --restart would get the next.
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if pid, err := strconv.Atoi(strings.TrimSpace(line)); err == nil && pid > 0 {
			return pid
		}
	}
	return 0
}

// DaemonHealth queries /v1/health, which needs no token.
func DaemonHealth(addr string) (map[string]any, error) {
	if addr == "" {
		addr = daemon.DefaultAddr
	}
	client := &http.Client{Timeout: 2 * time.Second}
	res, err := client.Get("http://" + addr + "/v1/health")
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("health returned %d", res.StatusCode)
	}
	var out map[string]any
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

func openBrowser(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err != nil {
		// Not fatal: the URL is already printed, so the user can open it.
		return nil
	}
	return nil
}
