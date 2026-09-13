package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/RhyChaw/aurium/internal/app"
	"github.com/RhyChaw/aurium/internal/daemon"
)

// daemonAddr is where the CLI expects the daemon.
func daemonAddr() string {
	if addr := os.Getenv("AURIUM_ADDR"); addr != "" {
		return addr
	}
	return daemon.DefaultAddr
}

// readHostToken reads ~/.aurium/token, which the daemon writes 0600.
func readHostToken() (string, error) {
	home, err := app.Home()
	if err != nil {
		return "", err
	}
	body, err := os.ReadFile(filepath.Join(home, "token"))
	if err != nil {
		return "", fmt.Errorf("no daemon token yet; start it with `aurium daemon start`: %w", err)
	}
	return strings.TrimSpace(string(body)), nil
}
