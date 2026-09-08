package driver

import (
	"errors"
	"os/exec"
)

func asExit(err error, target **exec.ExitError) bool {
	return errors.As(err, target)
}
