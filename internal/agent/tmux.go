package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/RhyChaw/aurium/internal/runtime/driver"
)

// Session drives tmux inside a container.
//
// D5: agents run in tmux rather than as the container's PID 1. That is what
// lets a human attach to a running agent, what keeps the container alive when
// an agent exits, and what makes `send-keys` a usable IPC nudge channel for
// agents that have no other inbox.
type Session struct {
	Driver      driver.Driver
	ContainerID string
	Name        string
}

// Exists reports whether the tmux session is running.
func (s *Session) Exists(ctx context.Context) (bool, error) {
	res, err := s.Driver.Exec(ctx, s.ContainerID,
		[]string{"tmux", "has-session", "-t", s.Name}, driver.ExecOpts{})
	if err != nil {
		return false, err
	}
	return res.ExitCode == 0, nil
}

// Start creates a detached tmux session running cmd.
func (s *Session) Start(ctx context.Context, cmd []string, env []string, workdir string) error {
	args := append([]string{"tmux", "new-session", "-d", "-s", s.Name}, cmd...)
	res, err := s.Driver.Exec(ctx, s.ContainerID, args, driver.ExecOpts{
		Workdir: workdir,
		Env:     env,
	})
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("agent: start tmux session %q: %s", s.Name, strings.TrimSpace(res.Stderr))
	}
	return nil
}

// SendText types a line into the session and presses Enter.
//
// -l sends the text literally, so a message containing tmux key names
// ("C-c", "Escape") is typed rather than interpreted — otherwise an agent
// could send another agent a message that killed its process.
func (s *Session) SendText(ctx context.Context, text string) error {
	if _, err := s.Driver.Exec(ctx, s.ContainerID,
		[]string{"tmux", "send-keys", "-t", s.Name, "-l", text}, driver.ExecOpts{}); err != nil {
		return err
	}
	_, err := s.Driver.Exec(ctx, s.ContainerID,
		[]string{"tmux", "send-keys", "-t", s.Name, "Enter"}, driver.ExecOpts{})
	return err
}

// Nudge displays a one-line status message without typing into the agent's
// prompt, used for context updates and IPC arrivals (§8.5, §9.5).
func (s *Session) Nudge(ctx context.Context, text string) error {
	_, err := s.Driver.Exec(ctx, s.ContainerID,
		[]string{"tmux", "display-message", "-t", s.Name, "[aurium] " + text},
		driver.ExecOpts{})
	return err
}

// Capture returns the visible pane contents, which is how Status infers
// whether an interactive agent is working or waiting.
func (s *Session) Capture(ctx context.Context, lines int) (string, error) {
	res, err := s.Driver.Exec(ctx, s.ContainerID,
		[]string{"tmux", "capture-pane", "-p", "-t", s.Name, "-S", fmt.Sprintf("-%d", lines)},
		driver.ExecOpts{})
	if err != nil {
		return "", err
	}
	return res.Stdout, nil
}

// Kill ends the session.
func (s *Session) Kill(ctx context.Context) error {
	_, err := s.Driver.Exec(ctx, s.ContainerID,
		[]string{"tmux", "kill-session", "-t", s.Name}, driver.ExecOpts{})
	return err
}

// AttachCommand is what a human runs on the host to join the session. The
// dashboard shows this string rather than embedding a terminal emulator
// (§11.3).
func AttachCommand(dockerBin, runtimeID, session string) string {
	return fmt.Sprintf("%s exec -it %s tmux attach -t %s", dockerBin, runtimeID, session)
}
