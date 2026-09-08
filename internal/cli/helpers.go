package cli

import (
	"regexp"
	"strings"

	"github.com/RhyChaw/aurium/internal/config"
	"github.com/RhyChaw/aurium/internal/runtime"
	"github.com/RhyChaw/aurium/internal/store"
)

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

// slugForTask turns a task title into a branch name. Branch names end up in
// PR titles and `git log`, so they are derived from the title rather than
// from an opaque id.
func slugForTask(title string) string {
	s := nonSlug.ReplaceAllString(strings.ToLower(title), "-")
	s = strings.Trim(s, "-")
	if s == "" {
		s = "task"
	}
	if len(s) > 50 {
		s = strings.Trim(s[:50], "-")
	}
	return s
}

func runtimeCreateOpts(projectID string, repo store.Repository, cfg *config.Config,
	branch, taskID, adapterName, role string) runtime.CreateOpts {
	return runtime.CreateOpts{
		ProjectID:    projectID,
		RepoID:       repo.ID,
		RepoRoot:     repo.Path,
		TaskID:       taskID,
		Branch:       branch,
		ParentBranch: cfg.Project.BaseBranch,
		Config:       cfg,
		Adapter:      adapterName,
		Role:         role,
	}
}
