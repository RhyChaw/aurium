package gitx

import (
	"errors"
	"os/exec"
)

func asExitError(err error, target **exec.ExitError) bool {
	return errors.As(err, target)
}

func asGitError(err error, target **Error) bool {
	return errors.As(err, target)
}
