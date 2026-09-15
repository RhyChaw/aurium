package preflight

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
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
func PortFree(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err == nil {
		ln.Close()
		return nil
	}
	if owner := portOwner(addr); owner != "" {
		return fmt.Errorf("in use by %s", owner)
	}
	return fmt.Errorf("in use: %w", err)
}

// portOwner is best-effort and deliberately quiet on failure: a missing lsof
// must degrade to a vaguer message, never to an error of its own.
func portOwner(addr string) string {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, "lsof", "-nP", "-iTCP:"+port, "-sTCP:LISTEN", "-F", "un").Output()
	if err != nil {
		return ""
	}
	var user, pid string
	for _, line := range strings.Split(string(out), "\n") {
		if len(line) < 2 {
			continue
		}
		switch line[0] {
		case 'p':
			pid = line[1:]
		case 'u':
			user = line[1:]
		}
	}
	if user == "" {
		return ""
	}
	if u := os.Getenv("USER"); u != "" && u == user {
		return fmt.Sprintf("your own process (pid %s) — replace it with: aurium up --restart", pid)
	}
	return fmt.Sprintf("another user %q (pid %s) — that process is not yours to restart; use --addr to pick a free port", user, pid)
}

// Checks is the single list of what Aurium needs. doctor, GET /v1/preflight
// and the dashboard wizard all render this and nothing else.
//
// An empty addr omits the port check. Over HTTP it is meaningless — if a
// caller reached /v1/preflight then the port is held, by the daemon answering
// them — so the API passes "" rather than reporting a failure that is really
// proof of success.
func Checks(home, addr string) []Check {
	checks := []Check{
		{
			Name:     "go",
			Severity: Required,
			Probe: func(ctx context.Context) error {
				out, err := exec.CommandContext(ctx, "go", "version").Output()
				if err != nil {
					return BinaryWorks(ctx, "go", "version")
				}
				return GoVersionAtLeast(string(out), 1, 25)
			},
			Remedy: Remedy{
				Kind:    Manual,
				Command: "./setup.sh",
				Note:    "installs a checksum-verified go1.27.1 into ~/.local/go without sudo",
			},
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
				_, err := os.Stat(home)
				return err
			},
			Remedy: Remedy{
				Kind: Auto,
				Fix:  func(ctx context.Context) error { return os.MkdirAll(home, 0o755) },
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
