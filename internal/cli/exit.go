package cli

import "fmt"

// Exit codes (§12.1). They are a public interface: scripts, CI and the daemon
// branch on them instead of parsing error text.
const (
	CodeOK              = 0
	CodeUsage           = 1
	CodeGit             = 2
	CodeDriver          = 3
	CodeConflict        = 4
	CodeLocked          = 5
	CodePermission      = 6
	CodeApprovalPending = 7
)

// ExitError carries an exit code out of a command.
type ExitError struct {
	Code    int
	Message string
	err     error
}

func (e *ExitError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	if e.err != nil {
		return e.err.Error()
	}
	return fmt.Sprintf("exit %d", e.Code)
}

func (e *ExitError) Unwrap() error { return e.err }

func exitf(code int, format string, args ...any) *ExitError {
	return &ExitError{Code: code, Message: fmt.Sprintf(format, args...)}
}

func wrap(code int, err error) *ExitError {
	if err == nil {
		return nil
	}
	return &ExitError{Code: code, Message: err.Error(), err: err}
}
