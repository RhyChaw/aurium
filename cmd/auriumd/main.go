// Command auriumd is the Aurium daemon.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/RhyChaw/aurium/internal/daemon"
)

func main() {
	addr := flag.String("addr", daemon.DefaultAddr, "loopback address to listen on")
	verbose := flag.Bool("v", false, "echo git and docker commands")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	d, err := daemon.New(daemon.Options{Addr: *addr, Verbose: *verbose, Log: log})
	if err != nil {
		fmt.Fprintln(os.Stderr, "auriumd:", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := d.Run(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "auriumd:", err)
		os.Exit(1)
	}
}
