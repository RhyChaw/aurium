package github

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Clone brings a repository onto the machine so a project can contain it.
//
// It shells out to git rather than using the API, for the same reason the rest
// of Aurium does (D1): git is the thing that understands git, and a clone that
// went through an HTTP client would not honour the user's credential helper,
// their SSH keys or their proxy.
//
// The token is passed through the environment for an HTTPS clone, never in the
// URL: a URL with a token in it ends up in `git remote -v`, in `.git/config`,
// and in every log that records the command.
func Clone(ctx context.Context, cloneURL, dest, token string) error {
	if strings.TrimSpace(cloneURL) == "" {
		return fmt.Errorf("github: clone needs a URL")
	}
	if strings.TrimSpace(dest) == "" {
		return fmt.Errorf("github: clone needs a destination")
	}

	// Refuse to clone into a directory that already has something in it. The
	// alternative — git failing halfway with its own message — leaves the
	// user guessing whether anything was overwritten.
	if entries, err := os.ReadDir(dest); err == nil && len(entries) > 0 {
		return fmt.Errorf("github: %s already exists and is not empty", dest)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}

	cmd := exec.CommandContext(ctx, "git", "clone", cloneURL, dest)
	cmd.Env = append(os.Environ(),
		// Never prompt. A daemon has no terminal, so a credential prompt is a
		// hang rather than a question.
		"GIT_TERMINAL_PROMPT=0",
	)
	if token != "" && strings.HasPrefix(cloneURL, "https://") {
		// A credential helper for the life of this one command: git asks, the
		// script prints, and nothing lands in the remote URL or .git/config.
		script, err := askpassScript(token)
		if err != nil {
			return err
		}
		// Removed here, unconditionally, because git may not call it at all —
		// a public repository needs no credential — and a script that deletes
		// itself on first run therefore never runs and never goes away,
		// leaving a file with a token in it in the temp directory. It may also
		// be called twice, once for the username and once for the password,
		// which self-deletion would break on the second call.
		defer os.Remove(script)

		cmd.Env = append(cmd.Env,
			"GIT_ASKPASS="+script,
			"GIT_CONFIG_COUNT=1",
			"GIT_CONFIG_KEY_0=credential.helper",
			"GIT_CONFIG_VALUE_0=",
		)
	}

	out, err := cmd.CombinedOutput()
	if err != nil {
		// git's own message is the useful one. It is scrubbed first, because a
		// failing clone can echo the URL it was given.
		return fmt.Errorf("github: cloning %s: %s", redact(cloneURL, token),
			redact(strings.TrimSpace(string(out)), token))
	}
	return nil
}

// askpassScript returns a path to a tiny program that prints the token.
//
// GIT_ASKPASS must be an executable, so the token lives in a 0700 file for the
// life of the clone rather than on a command line, where `ps` would show it to
// every process on the machine. The caller removes it; see Clone.
func askpassScript(token string) (string, error) {
	f, err := os.CreateTemp("", "aurium-askpass-*.sh")
	if err != nil {
		return "", fmt.Errorf("github: preparing credentials: %w", err)
	}
	// 0700 before anything is written, so there is no window in which the file
	// holds a token at the default mode.
	if err := f.Chmod(0o700); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", fmt.Errorf("github: preparing credentials: %w", err)
	}
	if _, err := fmt.Fprintf(f, "#!/bin/sh\nprintf '%%s' '%s'\n",
		strings.ReplaceAll(token, "'", "'\\''")); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", fmt.Errorf("github: preparing credentials: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return "", fmt.Errorf("github: preparing credentials: %w", err)
	}
	return f.Name(), nil
}

// redact keeps a credential out of an error message.
func redact(s, token string) string {
	if token == "" {
		return s
	}
	return strings.ReplaceAll(s, token, "…")
}

// OwnerRepo splits a remote URL into its owner and repository.
//
// Both HTTPS and SSH forms, because a repository cloned by hand may be either
// and the PR lookup needs the pair whichever it is.
func OwnerRepo(remote string) (owner, repo string, ok bool) {
	s := strings.TrimSpace(remote)
	s = strings.TrimSuffix(s, ".git")
	switch {
	case strings.HasPrefix(s, "git@"):
		// git@github.com:owner/repo
		_, rest, found := strings.Cut(s, ":")
		if !found {
			return "", "", false
		}
		s = rest
	case strings.Contains(s, "://"):
		// https://github.com/owner/repo
		_, rest, found := strings.Cut(s, "://")
		if !found {
			return "", "", false
		}
		if _, after, found := strings.Cut(rest, "/"); found {
			s = after
		} else {
			return "", "", false
		}
	default:
		return "", "", false
	}

	owner, repo, found := strings.Cut(s, "/")
	if !found || owner == "" || repo == "" || strings.Contains(repo, "/") {
		return "", "", false
	}
	return owner, repo, true
}
