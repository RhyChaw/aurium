//go:build !windows

package cli

import "syscall"

// detachAttr puts the daemon in its own process group so it survives the
// terminal that started it closing, and so Ctrl-C in that terminal does not
// take it down with the CLI.
func detachAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}
