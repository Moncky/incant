// Package shellinit serves the shell integration scripts.
//
// The scripts under scripts/ are the canonical copies and are embedded in the
// binary, so installing incant is one binary plus one line in a dotfile with
// no separate files to keep in sync. They are served rather than installed on
// purpose: `eval "$(incant shell-init zsh)"` means the integration is always
// the version the binary was built with.
package shellinit

import (
	"embed"
	"fmt"
	"sort"
	"strings"
)

//go:embed scripts/*.sh
var scripts embed.FS

// Supported lists the shells with an integration script.
func Supported() []string {
	entries, err := scripts.ReadDir("scripts")
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, strings.TrimSuffix(e.Name(), ".sh"))
	}
	sort.Strings(out)
	return out
}

// Script returns the integration script for a shell.
func Script(shell string) (string, error) {
	shell = strings.ToLower(strings.TrimSpace(shell))
	b, err := scripts.ReadFile("scripts/" + shell + ".sh")
	if err != nil {
		return "", fmt.Errorf("no integration for shell %q (have: %s)",
			shell, strings.Join(Supported(), ", "))
	}
	return string(b), nil
}
