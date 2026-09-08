package driver

import (
	"strings"
	"testing"
)

func specFixture() Spec {
	return Spec{
		Name:    "aurium-app-implement-oauth",
		Image:   "aurium-local/node-20-alpine:3fa1bc",
		Network: "aurium-app-implement-oauth",
		Workdir: "/Users/alice/app/.aurium/wt/implement-oauth",
		User:    "501:20",
		Env:     []string{"AURIUM_TOKEN=tok_secret", "AURIUM_URL=http://host.docker.internal:7770"},
		Binds: []Bind{
			{Host: "/Users/alice/app/.git", Container: "/Users/alice/app/.git", RW: true},
			{Host: "/Users/alice/app/.aurium/hooks", Container: "/Users/alice/app/.aurium/hooks", RW: false},
		},
		Volumes: []VolumeMount{
			{Name: "aurium-app-implement-oauth-node_modules", Path: "/Users/alice/app/.aurium/wt/implement-oauth/node_modules"},
			{Name: "aurium-npm-cache", Path: "/home/aurium/.npm", Shared: true},
		},
		Ports:     []PortSpec{{Internal: 3000, Env: "PORT"}},
		Labels:    map[string]string{LabelContainer: "c_01J", LabelProject: "p_01J"},
		Resources: Resources{CPUs: 2, Memory: "2g", PIDs: 2048},
	}
}

func TestCreateArgsCarryLabelsBindsAndLocalhostPorts(t *testing.T) {
	joined := strings.Join(createArgs(specFixture()), " ")

	for _, want := range []string{
		"--label aurium.container=c_01J",
		"--label aurium.project=p_01J",
		// D3: identical host path on both sides of the colon.
		"-v /Users/alice/app/.git:/Users/alice/app/.git:rw",
		"-v /Users/alice/app/.aurium/hooks:/Users/alice/app/.aurium/hooks:ro",
		// D11: never expose a dev server to the network.
		"-p 127.0.0.1:0:3000",
		"--network aurium-app-implement-oauth",
		// D5: the container is a place to run things, kept alive by a
		// no-op PID 1 with --init to reap zombies.
		"--init",
		"sleep infinity",
		"--user 501:20",
		"--cpus 2",
		"--memory 2g",
		"--pids-limit 2048",
		"-v aurium-app-implement-oauth-node_modules:",
		"-v aurium-npm-cache:/home/aurium/.npm",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("docker create args missing %q\ngot: %s", want, joined)
		}
	}
}

func TestCreateArgsNeverBindAllInterfaces(t *testing.T) {
	joined := strings.Join(createArgs(specFixture()), " ")
	if strings.Contains(joined, "0.0.0.0") {
		t.Fatal("published ports must bind 127.0.0.1 only (D11)")
	}
	// A bare "-p 3000:3000" would also expose on all interfaces.
	if strings.Contains(joined, "-p 3000:") {
		t.Fatal("ports must be published with an explicit 127.0.0.1 host address")
	}
}

func TestCreateArgsPassEnvironmentButNeverInline(t *testing.T) {
	args := createArgs(specFixture())
	// Env must be passed as separate argv elements, never concatenated into a
	// shell string, or a token containing a space or quote would break out.
	found := false
	for i, a := range args {
		if a == "-e" && i+1 < len(args) && args[i+1] == "AURIUM_TOKEN=tok_secret" {
			found = true
		}
	}
	if !found {
		t.Fatalf("env must be passed as discrete -e KEY=VALUE argv pairs, got %v", args)
	}
}

func TestPortEnvIsExportedToTheContainer(t *testing.T) {
	joined := strings.Join(createArgs(specFixture()), " ")
	// aurium.yaml declares `ports: [{internal: 3000, env: PORT}]`, meaning the
	// app inside should listen on 3000 and read it from $PORT.
	if !strings.Contains(joined, "PORT=3000") {
		t.Fatalf("declared port env var not set inside the container: %s", joined)
	}
}

func TestExecArgsQuoteNothingAndCarryWorkdir(t *testing.T) {
	args := execArgs("deadbeef", []string{"sh", "-lc", "echo hello world"}, ExecOpts{
		Workdir: "/w", User: "501:20", Env: []string{"A=b"},
	})
	joined := strings.Join(args, " ")
	for _, want := range []string{"exec", "--workdir /w", "--user 501:20", "-e A=b", "deadbeef"} {
		if !strings.Contains(joined, want) {
			t.Errorf("exec args missing %q, got: %s", want, joined)
		}
	}
	// The command must survive as three separate argv elements.
	if args[len(args)-1] != "echo hello world" {
		t.Fatalf("command argument was mangled: %q", args[len(args)-1])
	}
}

func TestSnapshotArgsDoNotUnpauseTheContainer(t *testing.T) {
	// §6.2 pauses first, then commits. `docker commit` pauses by default and
	// would unpause afterwards, so Aurium passes --pause=false to keep the
	// container frozen across the volume archiving that follows.
	joined := strings.Join(commitArgs("deadbeef", "aurium-snap/c_1:7"), " ")
	if !strings.Contains(joined, "--pause=false") {
		t.Fatalf("commit must not manage pausing itself: %s", joined)
	}
	if !strings.Contains(joined, "aurium-snap/c_1:7") {
		t.Fatalf("commit must tag the snapshot image: %s", joined)
	}
}

func TestParsePortsFromInspect(t *testing.T) {
	// Shape of `docker inspect --format {{json .NetworkSettings.Ports}}`.
	got, err := parsePorts(`{"3000/tcp":[{"HostIp":"127.0.0.1","HostPort":"53012"}],"9229/tcp":null}`)
	if err != nil {
		t.Fatal(err)
	}
	if got[3000] != 53012 {
		t.Fatalf("ports = %v, want 3000 -> 53012", got)
	}
	if _, ok := got[9229]; ok {
		t.Fatal("an unpublished port must not appear in the map")
	}
}

func TestVolumeNameIsStableAndScoped(t *testing.T) {
	a := VolumeName("app", "implement-oauth", "node_modules")
	b := VolumeName("app", "implement-oauth", "node_modules")
	if a != b {
		t.Fatal("volume names must be deterministic")
	}
	if a == VolumeName("app", "other-branch", "node_modules") {
		t.Fatal("per-container volumes must not collide across containers")
	}
	if strings.ContainsAny(a, " /:") {
		t.Fatalf("volume name %q contains characters docker rejects", a)
	}
}
