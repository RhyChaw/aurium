package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/RhyChaw/aurium/internal/agent"
	"github.com/RhyChaw/aurium/internal/app"
	"github.com/RhyChaw/aurium/internal/events"
	"github.com/RhyChaw/aurium/internal/preflight"
	"github.com/RhyChaw/aurium/internal/runtime/driver"
	"github.com/RhyChaw/aurium/internal/store"
	"github.com/spf13/cobra"
)

func newExecCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "exec <container> -- <command>...",
		Short: "Run a command inside a container",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withApp(func(ctx context.Context, a *app.App) error {
				c, err := resolveContainer(ctx, a, args[0])
				if err != nil {
					return err
				}
				drv, err := a.Manager.Drivers.Get(c.Driver)
				if err != nil {
					return wrap(CodeDriver, err)
				}

				res, err := drv.Exec(ctx, c.RuntimeID, args[1:], driver.ExecOpts{Workdir: c.Worktree})
				if err != nil {
					return wrap(CodeDriver, err)
				}
				fmt.Print(res.Stdout)
				fmt.Fprint(os.Stderr, res.Stderr)
				if res.ExitCode != 0 {
					// Pass the command's own exit code through, so scripts
					// wrapping `aurium exec` behave as if run directly.
					return &ExitError{Code: res.ExitCode}
				}
				return nil
			})
		},
	}
}

func newAttachCmd() *cobra.Command {
	var shell bool

	cmd := &cobra.Command{
		Use:   "attach <container>",
		Short: "Attach to the agent running in a container",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withApp(func(ctx context.Context, a *app.App) error {
				c, err := resolveContainer(ctx, a, args[0])
				if err != nil {
					return err
				}
				drv, err := a.Manager.Drivers.Get(c.Driver)
				if err != nil {
					return wrap(CodeDriver, err)
				}
				if !drv.Capabilities().Tmux {
					return exitf(CodeDriver,
						"the %s driver does not supervise sessions; there is nothing to attach to. "+
							"Work directly in %s", c.Driver, c.Worktree)
				}

				agents, err := a.Store.ListLiveAgents(ctx, c.ID)
				if err != nil {
					return wrap(CodeUsage, err)
				}
				session := "agent"
				if len(agents) > 0 {
					session = agents[0].TmuxSession
				}

				var argv []string
				if shell {
					argv = []string{"exec", "-it", c.RuntimeID, "bash", "-l"}
				} else {
					argv = []string{"exec", "-it", c.RuntimeID, "tmux", "attach", "-t", session}
				}

				// Hand the terminal over: attaching is interactive, so this
				// process becomes the passthrough rather than capturing output.
				proc := exec.Command(a.Manager.DockerBin, argv...)
				proc.Stdin, proc.Stdout, proc.Stderr = os.Stdin, os.Stdout, os.Stderr
				if err := proc.Run(); err != nil {
					return wrap(CodeDriver, err)
				}
				return nil
			})
		},
	}
	cmd.Flags().BoolVar(&shell, "shell", false, "attach a plain shell instead of the agent session")
	return cmd
}

func newTaskCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "task", Short: "Create and track tasks"}

	var adapterName, role string
	var noContainer bool
	create := &cobra.Command{
		Use:   "create <title>",
		Short: "Create a task, with a container and agent for it",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withApp(func(ctx context.Context, a *app.App) error {
				p, repo, cfg, err := a.Project(ctx, cwd())
				if err != nil {
					return wrap(CodeUsage, err)
				}
				t, err := a.Store.CreateTask(ctx, p.ID, args[0], "")
				if err != nil {
					return wrap(CodeUsage, err)
				}
				if err := a.Events.Emit(ctx, events.Event{
					Type: events.TaskCreated, Actor: events.ActorHuman,
					ProjectID: p.ID, TaskID: t.ID,
					Payload: map[string]any{"title": t.Title},
				}); err != nil {
					return wrap(CodeUsage, err)
				}
				fmt.Printf("Created task %s: %s\n", t.ID, t.Title)

				if noContainer {
					return nil
				}
				if adapterName == "" {
					adapterName = cfg.Sandbox.Agent
				}
				if role == "" {
					role = store.RolePrimary
				}

				branch := slugForTask(args[0])
				c, err := a.Manager.Create(ctx, runtimeCreateOpts(p.ID, repo, cfg, branch, t.ID, adapterName, role))
				if err != nil {
					return wrap(CodeDriver, err)
				}
				printContainer(a, c)
				return nil
			})
		},
	}
	create.Flags().StringVar(&adapterName, "agent", "", "agent adapter")
	create.Flags().StringVar(&role, "role", "", "agent role: primary|master|worker")
	create.Flags().BoolVar(&noContainer, "no-container", false, "create the task only")

	list := &cobra.Command{
		Use:   "list",
		Short: "List tasks",
		RunE: func(cmd *cobra.Command, args []string) error {
			return withApp(func(ctx context.Context, a *app.App) error {
				p, _, _, err := a.Project(ctx, cwd())
				if err != nil {
					return wrap(CodeUsage, err)
				}
				ts, err := a.Store.ListTasks(ctx, p.ID)
				if err != nil {
					return wrap(CodeUsage, err)
				}
				if emit(ts) {
					return nil
				}
				w := tabWriter()
				fmt.Fprintln(w, "ID\tSTATUS\tTITLE")
				for _, t := range ts {
					fmt.Fprintf(w, "%s\t%s\t%s\n", t.ID, t.Status, t.Title)
				}
				return w.Flush()
			})
		},
	}

	transition := &cobra.Command{
		Use:   "transition <task> <status>",
		Short: "Move a task to a new status",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withApp(func(ctx context.Context, a *app.App) error {
				if err := a.Store.TransitionTask(ctx, args[0], args[1]); err != nil {
					return wrap(CodeUsage, err)
				}
				t, _ := a.Store.GetTask(ctx, args[0])
				if err := a.Events.Emit(ctx, events.Event{
					Type: events.TaskTransitioned, Actor: events.ActorHuman,
					ProjectID: t.ProjectID, TaskID: t.ID,
					Payload: map[string]any{"status": args[1]},
				}); err != nil {
					return wrap(CodeUsage, err)
				}
				fmt.Printf("%s -> %s\n", args[0], args[1])
				return nil
			})
		},
	}

	cmd.AddCommand(create, list, transition)
	return cmd
}

func newEventsCmd() *cobra.Command {
	var follow bool
	var types []string
	var containerID string

	cmd := &cobra.Command{
		Use:   "events",
		Short: "Tail the event stream — the audit trail behind everything",
		RunE: func(cmd *cobra.Command, args []string) error {
			return withApp(func(ctx context.Context, a *app.App) error {
				filter := events.Filter{Types: types, ContainerID: containerID}

				past, err := a.Events.Replay(ctx, 0, filter, 200)
				if err != nil {
					return wrap(CodeUsage, err)
				}
				for _, e := range past {
					printEvent(e)
				}
				if !follow {
					return nil
				}

				ch, cancel := a.Events.Subscribe(filter)
				defer cancel()
				for e := range ch {
					printEvent(e)
				}
				return nil
			})
		},
	}
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "stream new events as they happen")
	cmd.Flags().StringSliceVar(&types, "type", nil, "filter by event type")
	cmd.Flags().StringVar(&containerID, "container", "", "filter by container")
	return cmd
}

func printEvent(e events.Event) {
	if emit(e) {
		return
	}
	var extras []string
	for k, v := range e.Payload {
		extras = append(extras, fmt.Sprintf("%s=%v", k, v))
	}
	line := fmt.Sprintf("%s  %-28s %-12s", e.TS, e.Type, e.Actor)
	if e.ContainerID != "" {
		line += " " + e.ContainerID
	}
	if len(extras) > 0 {
		line += "  " + strings.Join(extras, " ")
	}
	fmt.Println(line)
}

func newStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Summarise this project",
		RunE: func(cmd *cobra.Command, args []string) error {
			return withApp(func(ctx context.Context, a *app.App) error {
				p, _, cfg, err := a.Project(ctx, cwd())
				if err != nil {
					return wrap(CodeUsage, err)
				}
				cs, _ := a.Store.ListContainers(ctx, p.ID)
				ts, _ := a.Store.ListTasks(ctx, p.ID)

				byStatus := map[string]int{}
				agentCount := 0
				for _, c := range cs {
					byStatus[c.Status]++
					ags, _ := a.Store.ListLiveAgents(ctx, c.ID)
					agentCount += len(ags)
				}
				if emit(map[string]any{
					"project": p, "containers": len(cs), "tasks": len(ts),
					"agents": agentCount, "by_status": byStatus,
				}) {
					return nil
				}

				fmt.Printf("%s  (%s)\n", cfg.Project.Name, p.Root)
				fmt.Printf("  containers %d\n", len(cs))
				for st, n := range byStatus {
					fmt.Printf("    %-10s %d\n", st, n)
				}
				fmt.Printf("  agents     %d\n", agentCount)
				fmt.Printf("  tasks      %d\n", len(ts))
				return nil
			})
		},
	}
}

// doctorOK is the one place that decides whether results mean the machine
// can run Aurium: a failing optional check does not count against it, a
// failing required one always does. Both the human table and --json exit
// wiring call this, so they cannot drift apart.
func doctorOK(results []preflight.Result) bool {
	for _, r := range results {
		if !r.OK && r.Severity != string(preflight.Optional) {
			return false
		}
	}
	return true
}

// writeDoctorJSON encodes results as JSON to w — the payload a caller like
// CI parses — and only then reports the command's verdict as an error, so
// the full diagnostic is always written even when the command is about to
// fail. A failing required check yields an *ExitError; a failing optional
// one does not.
func writeDoctorJSON(w io.Writer, results []preflight.Result) error {
	if err := json.NewEncoder(w).Encode(results); err != nil {
		return err
	}
	if !doctorOK(results) {
		return &ExitError{Code: CodeUsage}
	}
	return nil
}

// renderDoctor prints results and reports whether the machine is usable.
// Optional failures are printed but do not fail the command.
func renderDoctor(w io.Writer, results []preflight.Result) bool {
	for _, r := range results {
		switch {
		case r.OK && r.Detail != "":
			fmt.Fprintf(w, "  ok    %s: %s\n", r.Name, r.Detail)
		case r.OK:
			fmt.Fprintf(w, "  ok    %s\n", r.Name)
		case r.Severity == string(preflight.Optional):
			fmt.Fprintf(w, "  warn  %s: %s\n", r.Name, r.Error)
		default:
			fmt.Fprintf(w, "  FAIL  %s: %s\n", r.Name, r.Error)
		}
		if !r.OK && r.Remedy != "" {
			fmt.Fprintf(w, "        fix: %s\n", r.Remedy)
		}
	}
	return doctorOK(results)
}

func newDoctorCmd() *cobra.Command {
	var (
		jsonOut bool
		addr    string
		fix     bool
	)

	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check that this machine can run Aurium",
		RunE: func(cmd *cobra.Command, args []string) error {
			checks := preflight.Checks(addr)
			var results []preflight.Result
			if fix {
				results = preflight.Fix(cmd.Context(), checks)
			} else {
				results = preflight.Run(cmd.Context(), checks)
			}
			// Appended to the same slice both modes render and both modes
			// grade. These rows used to be printed straight to stdout from
			// inside the human branch, below a `return` that --json took
			// first: a failing database ping failed `doctor` and passed
			// `doctor --json`, and CI reads the second one.
			results = append(results, appDiagnostics()...)

			if jsonOut {
				return writeDoctorJSON(cmd.OutOrStdout(), results)
			}

			fmt.Fprintln(cmd.OutOrStdout(), "Aurium doctor")
			if !renderDoctor(cmd.OutOrStdout(), results) {
				return &ExitError{Code: CodeUsage}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "machine-readable output")
	// doctor checks the address the daemon would bind, so it has to know it. The
	// default matches `aurium up`.
	cmd.Flags().StringVar(&addr, "addr", "127.0.0.1:7770", "loopback address to check")
	cmd.Flags().BoolVar(&fix, "fix", false, "apply automatic remedies")
	return cmd
}

// appDiagnostics runs the checks that need an open App — the database, the
// project config, the agent adapter's credentials — and returns them in the
// same shape as the machine checks, so there is one list, one renderer and
// one verdict rather than a second diagnostic reachable only without --json.
//
// Opening the App failing is reported as an optional row rather than a
// required one: that error used to be discarded outright, and this wave is
// not the place to start failing `doctor` on a case nobody has looked at. It
// is visible now, which is the part that was missing.
func appDiagnostics() []preflight.Result {
	var out []preflight.Result
	required := func(name string, err error) {
		r := preflight.Result{Name: name, OK: err == nil, Severity: string(preflight.Required)}
		if err != nil {
			r.Error = err.Error()
		}
		out = append(out, r)
	}
	info := func(name, detail string) {
		out = append(out, preflight.Result{
			Name: name, OK: true, Severity: string(preflight.Optional), Detail: detail,
		})
	}
	warn := func(name, msg string) {
		out = append(out, preflight.Result{
			Name: name, OK: false, Severity: string(preflight.Optional), Error: msg,
		})
	}

	if err := withApp(func(ctx context.Context, a *app.App) error {
		required("database", a.Store.DB().Ping())

		p, _, cfg, err := a.Project(ctx, cwd())
		if err != nil {
			// Not being inside a project is not a fault: `doctor` is run from
			// anywhere, and most often from a clone before `aurium init`.
			info("project config", fmt.Sprintf("not inside an initialised project (%v)", err))
			return nil
		}
		required("project config", nil)
		for _, w := range cfg.Warnings() {
			warn("project config", w)
		}
		cs, _ := a.Store.ListContainers(ctx, p.ID)
		info("containers", fmt.Sprintf("%d container(s)", len(cs)))

		// Verify the adapters' credentials are actually present, since
		// a missing key fails deep inside a container otherwise.
		if ad, ok := agent.DefaultRegistry().Get(cfg.Sandbox.Agent); ok {
			names := ad.AuthEnv()
			if len(names) > 0 && !anyEnvSet(names) {
				warn("agent credentials", fmt.Sprintf("none of %v is set; the %s agent will not authenticate",
					names, cfg.Sandbox.Agent))
			}
		}
		return nil
	}); err != nil {
		warn("daemon state", fmt.Sprintf("could not open ~/.aurium: %v", err))
	}
	return out
}

func anyEnvSet(names []string) bool {
	for _, n := range names {
		if os.Getenv(n) != "" {
			return true
		}
	}
	return false
}
