// Package cli implements the aurium command line (§12.1).
package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/RhyChaw/aurium/internal/app"
	"github.com/spf13/cobra"
)

var (
	flagProject string
	flagJSON    bool
	flagVerbose bool
)

// Execute runs the CLI.
func Execute() error {
	root := &cobra.Command{
		Use:   "aurium",
		Short: "An operating environment for parallel coding agents",
		Long: "Aurium runs coding agents in isolated, snapshot-able, stackable containers.\n" +
			"Each container is a git worktree, a derived image, and one agent.",
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	root.PersistentFlags().StringVar(&flagProject, "project", "",
		"project path or id (default: the project containing the working directory)")
	root.PersistentFlags().BoolVar(&flagJSON, "json", false, "machine-readable output")
	root.PersistentFlags().BoolVarP(&flagVerbose, "verbose", "v", false, "echo git and docker commands")

	root.AddCommand(
		newInitCmd(),
		newContainerCmd(),
		newTreeCmd(),
		newSyncCmd(),
		newExecCmd(),
		newAttachCmd(),
		newDestroyCmd(),
		newSnapshotCmd(),
		newRestoreCmd(),
		newForkCmd(),
		newStackCmd(),
		newTaskCmd(),
		newEventsCmd(),
		newStatusCmd(),
		newDoctorCmd(),
		newDaemonCmd(),
		newDashboardCmd(),
		newContextCmd(),
		newInboxCmd(),
		newApproveCmd(false),
		newApproveCmd(true),
		newIntegrationCmd(),
		newVersionCmd(),
	)
	return root.Execute()
}

// withApp opens the store and runs fn, closing afterwards.
func withApp(fn func(ctx context.Context, a *app.App) error) error {
	a, err := app.Open(flagVerbose)
	if err != nil {
		return wrap(CodeUsage, err)
	}
	defer a.Close()
	return fn(context.Background(), a)
}

// cwd is the directory commands resolve a project from.
func cwd() string {
	if flagProject != "" {
		return flagProject
	}
	d, err := os.Getwd()
	if err != nil {
		return "."
	}
	return d
}

// emit prints a value as JSON when --json is set and returns whether it did,
// so each command can keep its human output separate.
func emit(v any) bool {
	if !flagJSON {
		return false
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		fmt.Fprintln(os.Stderr, "aurium: encoding output:", err)
	}
	return true
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the Aurium version",
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Println("aurium " + Version)
			return nil
		},
	}
}

// Version is set at build time with -ldflags.
var Version = "0.1.0-dev"
