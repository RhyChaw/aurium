// Package assets holds files Aurium writes onto the host or into images:
// git guard hooks, the derived-image Dockerfile template, and the script that
// bakes the host uid/gid into a container image.
package assets

import "embed"

// Hooks contains the git hooks installed into <repo>/.aurium/hooks.
//
//go:embed hooks/*
var Hooks embed.FS

// Templates contains the Dockerfile template and container setup scripts.
//
//go:embed all:templates
var Templates embed.FS
