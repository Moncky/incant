// Package backend turns a request into candidate commands.
//
// Two implementations exist and they are not equals. The API backend is the
// real one: it owns a probe tool surface and can look at the user's actual
// files before answering. The fallback shells out to the user's own claude CLI
// and runs one-shot with no probing -- it exists so incant does something
// useful the moment it is installed, with no key to configure, and it is
// expected to be worse.
package backend

import (
	"context"
	"errors"

	"github.com/callumscott/incant/internal/shellctx"
)

// Request is one incantation.
type Request struct {
	Query   string
	Context *shellctx.Context

	// Fix marks a repair request: the previous command failed and the model
	// should diagnose it from Context.LastCommand and LastStatus.
	Fix bool

	// N is how many distinct candidates to return. The keybind cycles through
	// them on repeat presses, which is how incant answers an ambiguous
	// request without interrupting to ask a question.
	N int
}

// Candidate is one suggested command.
type Candidate struct {
	// Command is a single line, ready to place in the line editor.
	Command string
	// Rationale is one short clause, shown only in verbose and interactive
	// modes. It is never placed in the buffer.
	Rationale string
	// Probes counts the reconnaissance calls made to produce this candidate.
	// Zero means the answer came from pre-collected context alone.
	Probes int
}

// Backend produces candidates for a request.
type Backend interface {
	// Suggest returns at least one candidate, or an error.
	Suggest(ctx context.Context, req Request) ([]Candidate, error)
	// Name identifies the backend in diagnostics.
	Name() string
}

// ErrUnsupported means the model declined to express the request as a command.
// It is a normal outcome, not a malfunction: the shell integration reports it
// and leaves the user's buffer untouched.
var ErrUnsupported = errors.New("request cannot be expressed as a shell command")

// ErrUnavailable means this backend cannot run here -- no credentials, no
// claude CLI on PATH. The caller may fall back to another backend.
var ErrUnavailable = errors.New("backend unavailable")
