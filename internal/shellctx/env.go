package shellctx

import (
	"os"
	"path/filepath"
)

// Environment variable names the shell integration sets. They are read here
// rather than in the flag layer so the widget and the bare CLI behave alike.
const (
	EnvLastCommand = "_INCANT_LAST_CMD"
	EnvLastStatus  = "_INCANT_LAST_STATUS"
	EnvLastStderr  = "_INCANT_LAST_STDERR"
)

// envShell returns the basename of $SHELL, or "".
func envShell() string {
	if sh := os.Getenv("SHELL"); sh != "" {
		return filepath.Base(sh)
	}
	return ""
}
