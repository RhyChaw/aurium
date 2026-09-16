package preflight

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

var goVersionRe = regexp.MustCompile(`go(\d+)\.(\d+)(?:\.(\d+))?`)

// GoVersionAtLeast parses `go version` output against a floor. Unparseable
// output is a failure: a toolchain we cannot identify is one we cannot vouch
// for, and guessing "probably fine" is how a broken build reaches a build log.
func GoVersionAtLeast(out string, major, minor int) error {
	m := goVersionRe.FindStringSubmatch(out)
	if m == nil {
		return fmt.Errorf("could not read a version from %q", strings.TrimSpace(out))
	}
	haveMajor, _ := strconv.Atoi(m[1])
	haveMinor, _ := strconv.Atoi(m[2])
	if haveMajor > major || (haveMajor == major && haveMinor >= minor) {
		return nil
	}
	return fmt.Errorf("go%d.%d is older than the required go%d.%d", haveMajor, haveMinor, major, minor)
}

// PortFree reports whether addr can be listened on, and when it cannot, who
// is holding it. The owner matters: `aurium up --restart` is good advice for
// your own stale daemon and useless for another user's live one.
//
// The one holder that is not a problem at all is *this user's own* Aurium
// daemon, which is the normal state of a working machine: doctor defaults to
// the address `aurium up` binds, so a healthy machine failed its own port
// check, and `setup.sh` — which ends in `doctor --fix` — closed on a red FAIL
// telling you to restart the daemon you had just started. That case returns
// an InfoError: a passing check with something to say.
func PortFree(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err == nil {
		ln.Close()
		return nil
	}
	holder, probed := portOwner(addr)
	switch {
	case holder != nil && holder.mine:
		// Asked, not assumed: "something of mine answers on 7770" is not
		// evidence it is Aurium, and calling an unrelated server of theirs an
		// Aurium daemon would be its own confident lie.
		if who, ok := auriumListening(addr); ok {
			return Info("held by %s — your own, already serving this address", who)
		}
		return fmt.Errorf("in use by your own process (pid %s) — replace it with: aurium up --restart", holder.pid)

	case holder != nil:
		return fmt.Errorf("in use by another user %q (pid %s) — that process is not yours to restart; "+
			"use --addr to pick a free port", holder.login, holder.pid)

	case probed:
		// lsof ran and named nobody. Unprivileged lsof does not list other
		// users' sockets on macOS, so this is the *expected* result for the
		// motivating case — another user's daemon — and falling through to the
		// raw "bind: address already in use" here would put back exactly the
		// opaque message the owner lookup was written to replace.
		return errors.New("in use by a process this user cannot see — most likely another user's daemon; " +
			"pick a free port with --addr")

	default:
		// Ownership could not be established at all (no lsof, or it timed
		// out). An Aurium daemon answering here is still the likeliest reading
		// by a wide margin, and saying so beats failing a machine that works —
		// but the sentence admits what it does not know.
		if who, ok := auriumListening(addr); ok {
			return Info("held by %s — ownership could not be confirmed on this host, but Aurium is serving the address", who)
		}
		return fmt.Errorf("in use: %w", err)
	}
}

// auriumListening reports whether the thing holding addr is an Aurium daemon,
// by asking it. /v1/health needs no token, which is what makes this usable
// from a preflight check that has no credentials of its own.
func auriumListening(addr string) (string, bool) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", false
	}
	// A wildcard bind is not an address you can dial; loopback is where a
	// daemon bound to it is reachable.
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	client := &http.Client{Timeout: time.Second}
	res, err := client.Get("http://" + net.JoinHostPort(host, port) + "/v1/health")
	if err != nil {
		return "", false
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return "", false
	}
	var body struct {
		Status    string `json:"status"`
		Version   string `json:"version"`
		PID       int    `json:"pid"`
		StartedAt string `json:"started_at"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 64<<10)).Decode(&body); err != nil {
		return "", false
	}
	// All four together are Aurium's health shape; "status: ok" alone is a
	// thing half the world's health endpoints say.
	if body.Status != "ok" || body.Version == "" || body.StartedAt == "" || body.PID == 0 {
		return "", false
	}
	return fmt.Sprintf("an Aurium daemon (version %s, pid %d, up %s)", body.Version, body.PID, body.StartedAt), true
}

// portHolder is who lsof named. mine is the distinction the advice turns on:
// you can restart your own daemon and you cannot restart anyone else's.
type portHolder struct {
	login string
	pid   string
	mine  bool
}

// portOwner is best-effort. It returns the holder when it found one, and
// whether lsof actually got to answer — the caller needs to tell "lsof said
// nobody" (which on macOS means another user) from "lsof never ran", because
// those two deserve different sentences and only one of them is a finding.
func portOwner(addr string) (holder *portHolder, probed bool) {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, false
	}
	lsof, err := exec.LookPath("lsof")
	if err != nil {
		return nil, false // nothing was learned, so claim nothing
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	// The exit status is deliberately ignored: lsof exits 1 with no output
	// when nothing matched, and that is a real answer, not a failure to run.
	out, _ := exec.CommandContext(ctx, lsof, "-nP", "-iTCP:"+port, "-sTCP:LISTEN", "-F", "Lpn").Output()
	if ctx.Err() != nil {
		return nil, false // timed out mid-probe: also nothing learned
	}
	var login, pid string
	for _, line := range strings.Split(string(out), "\n") {
		if len(line) < 2 {
			continue
		}
		switch line[0] {
		case 'L':
			login = line[1:]
		case 'p':
			pid = line[1:]
		}
	}
	if login == "" {
		return nil, true
	}
	me := currentUsername()
	return &portHolder{login: login, pid: pid, mine: me != "" && me == login}, true
}

// currentUsername asks the OS, not the environment. $USER is unset under
// launchd and in some containers, and wrong under `sudo -E` — and each of
// those turns "your own daemon" into "another user's, not yours to restart",
// which is advice that cannot work. $USER stays only as a last resort for the
// platforms where os/user has nothing to read.
func currentUsername() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	return os.Getenv("USER")
}

// Checks is the single list of what Aurium needs. doctor, GET /v1/preflight
// and the dashboard wizard all render this and nothing else.
//
// An empty addr omits the port check. Over HTTP it is meaningless — if a
// caller reached /v1/preflight then the port is held, by the daemon answering
// them — so the API passes "" rather than reporting a failure that is really
// proof of success.
func Checks(addr string) []Check {
	checks := []Check{
		{
			Name:     "go",
			Severity: Required,
			Probe: func(ctx context.Context) error {
				return goWorks(ctx)
			},
			Remedy: goRemedy(),
		},
		{
			Name:     "git",
			Severity: Required,
			Probe:    func(ctx context.Context) error { return BinaryWorks(ctx, "git", "--version") },
			Remedy:   gitRemedy(),
		},
		{
			Name:     "docker",
			Severity: Required,
			Probe:    func(ctx context.Context) error { return BinaryWorks(ctx, "docker", "--version") },
			Remedy: Remedy{
				Kind:    Manual,
				Command: "install Docker Desktop, OrbStack or podman",
				Note:    "a container runtime cannot be installed unprivileged",
			},
		},
		{
			Name:     "docker daemon",
			Severity: Required,
			Probe: func(ctx context.Context) error {
				// Bounded like every BinaryWorks probe. `docker info` talks to
				// the daemon socket, so it is the *most* likely of them to
				// hang — and unbounded it hangs doctor and the
				// /v1/preflight handler with it.
				ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
				defer cancel()

				cmd := exec.CommandContext(ctx, "docker", "info")
				var stderr bytes.Buffer
				cmd.Stderr = &stderr
				if err := cmd.Run(); err != nil {
					if line := firstLine(stderr.String()); line != "" {
						return fmt.Errorf("%s", line)
					}
					return err
				}
				return nil
			},
			Remedy: dockerDaemonRemedy(),
		},
		{
			Name:     "tmux",
			Severity: Optional,
			Probe:    func(ctx context.Context) error { return BinaryWorks(ctx, "tmux", "-V") },
			Remedy: Remedy{
				Kind:    Manual,
				Command: "brew install tmux   # or: apt-get install tmux",
				Note:    "host-side convenience only; containers get their own from the image",
			},
		},
		{
			Name:     "~/.aurium",
			Severity: Required,
			Probe: func(ctx context.Context) error {
				dir, err := auriumHome()
				if err != nil {
					return err
				}
				fi, err := os.Stat(dir)
				if err != nil {
					return err
				}
				if !fi.IsDir() {
					return fmt.Errorf("%s exists but is not a directory", dir)
				}
				return nil
			},
			Remedy: Remedy{
				Kind: Auto,
				Fix: func(ctx context.Context) error {
					dir, err := auriumHome()
					if err != nil {
						return err
					}
					return os.MkdirAll(dir, 0o755)
				},
			},
		},
	}

	if addr != "" {
		checks = append(checks, Check{
			Name:     "port",
			Severity: Required,
			Probe:    func(ctx context.Context) error { return PortFree(addr) },
			Remedy: Remedy{
				Kind:    Manual,
				Command: "aurium up --addr 127.0.0.1:7771",
				Note:    "or stop whatever holds the address; the probe names the owner",
			},
		})
	}
	return checks
}

// auriumHome answers where ~/.aurium is *without* creating it, and that
// omission is the whole point. app.Home() creates the directory as a side
// effect of being asked where it is — a contract other code depends on — so a
// check handed app.Home()'s answer could only ever stat a directory that had
// just been made for it. The check passed on every machine, including one
// where the directory could not be created at all, and its Auto remedy was
// unreachable code; on Linux, where no other remedy is automatic, that left
// `doctor --fix` with nothing whatsoever to apply.
//
// It mirrors app.Home()'s rule exactly (AURIUM_HOME wins, else ~/.aurium) and
// stops there. The two must agree about the path or the check is checking
// somewhere nothing uses.
func auriumHome() (string, error) {
	if custom := os.Getenv("AURIUM_HOME"); custom != "" {
		return custom, nil
	}
	h, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(h, ".aurium"), nil
}

// goInstallPath is where setup.sh puts the toolchain it installs. The probe
// looks here as well as on PATH, because a daemon does not inherit a login
// shell's environment: launched from the app bundle, or from a terminal opened
// before the profile line was added, PATH lacks this directory while the
// toolchain sits in it perfectly usable.
//
// Reporting "not installed" for software this project's own installer placed
// is the `ok git` mistake inverted — a check answering a different question
// than the one it prints.
func goInstallPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".local", "go", "bin", "go")
}

// goWorks judges the toolchain by running it, on PATH first and then where
// setup.sh installs. Either one satisfies the requirement: what matters is
// that a usable go exists, not which directory it was found in.
func goWorks(ctx context.Context) error {
	if out, err := exec.CommandContext(ctx, "go", "version").Output(); err == nil {
		return GoVersionAtLeast(string(out), 1, 25)
	}
	if p := goInstallPath(); p != "" {
		if out, err := exec.CommandContext(ctx, p, "version").Output(); err == nil {
			return GoVersionAtLeast(string(out), 1, 25)
		}
	}
	// Neither worked. Report the PATH attempt, since that is the one whose
	// failure the user can act on.
	return BinaryWorks(ctx, "go", "version")
}

// goRemedy tells a contributor what to actually do about a failing "go"
// check, and that depends on which of two very different things is true.
// Both are Manual: PATH lives in a shell profile that is not this tool's to
// rewrite, so neither case can be applied unprivileged by --fix.
//
//   - Nothing resolves on PATH, and setup.sh never installed anything at
//     ~/.local/go/bin either: "./setup.sh" is correct, first-time advice.
//   - setup.sh already installed a working go1.27.1 at ~/.local/go/bin/go,
//     but this shell's PATH doesn't include it: telling them to run
//     setup.sh again is circular — it would do the same work and change
//     nothing. The actual fix is putting that directory on PATH.
//
// Something already on PATH (even a too-old or broken go) still gets the
// setup.sh advice: that is the tool that gets them a working toolchain.
func goRemedy() Remedy {
	if _, err := exec.LookPath("go"); err == nil {
		return Remedy{
			Kind:    Manual,
			Command: "./setup.sh",
			Note:    "installs a checksum-verified go1.27.1 into ~/.local/go without sudo",
		}
	}
	if candidate := goInstallPath(); candidate != "" {
		if fi, err := os.Stat(candidate); err == nil && !fi.IsDir() {
			return Remedy{
				Kind:    Manual,
				Command: `export PATH="$HOME/.local/go/bin:$PATH"`,
				Note:    "setup.sh already installed go1.27.1 there; add this line to your shell profile so a new shell finds it too",
			}
		}
	}
	return Remedy{
		Kind:    Manual,
		Command: "./setup.sh",
		Note:    "installs a checksum-verified go1.27.1 into ~/.local/go without sudo",
	}
}

func gitRemedy() Remedy {
	if runtime.GOOS == "darwin" {
		// The overwhelmingly common macOS cause: git is present and refuses to
		// run. Needs a TTY for the password, so setup.sh cannot do it.
		return Remedy{
			Kind:    Manual,
			Command: "sudo xcodebuild -license accept",
			Note:    "run this in a real terminal — sudo needs a TTY to read a password",
		}
	}
	return Remedy{Kind: Manual, Command: "apt-get install git   # or your distro's equivalent"}
}

func dockerDaemonRemedy() Remedy {
	if runtime.GOOS == "darwin" {
		return Remedy{
			Kind: Auto,
			Fix: func(ctx context.Context) error {
				return exec.CommandContext(ctx, "open", "-a", "Docker").Run()
			},
		}
	}
	return Remedy{
		Kind:    Manual,
		Command: "sudo systemctl start docker",
		Note:    "starting a system service requires privileges setup will not take",
	}
}
