package flowm

import (
	"errors"
	"fmt"
	"strings"
)

const defaultMaxJumps = 1000

// ErrJumpLimitExceeded means a flow tried to take more than its configured
// number of jumps in one Start or StartWithResult invocation.
var ErrJumpLimitExceeded = errors.New("flow jump limit exceeded")

type jumpTarget struct {
	name string
}

// Jump selects a named top-level step as the next step to execute. Return it
// from a When callback. The name must identify exactly one explicitly Named step.
// A selected jump handles the source error and retains its values; source
// piping, if configured, passes those values to the target. Jump cannot carry
// policies or nested routes, or appear inside a parallel worker.
func Jump(name string) Action {
	return Action{jump: &jumpTarget{name: name}}
}

// WithMaxJumps limits the total number of forward, backward, and self jumps
// per run. The default is 1000. The limit must be positive. A jump beyond the
// limit stops execution with ErrJumpLimitExceeded wrapped in an ActionError.
func (f *flowm) WithMaxJumps(limit int) Flowm {
	f.maxJumps = limit
	return f
}

// Index registered names without invoking callbacks. Dynamic jump destinations
// are checked when returned, so unreferenced duplicate names remain valid.
func (f *flowm) indexTargets() (map[string]int, []error) {
	targets := make(map[string]int)
	for i, st := range f.stubStorage {
		if st.Action.Name == "" {
			continue
		}
		if _, exists := targets[st.Action.Name]; exists {
			targets[st.Action.Name] = -1
		} else {
			targets[st.Action.Name] = i
		}
	}
	var errs []error
	if f.maxJumps < 1 {
		errs = append(errs, &ActionError{Name: "flow", Err: errors.New("jump limit must be positive")})
	}
	return targets, errs
}

func (f *flowm) validateJump(action Action) error {
	if action.Func != nil || len(action.Params) != 0 || len(action.Policies) != 0 || len(action.routes) != 0 || action.parallel != nil {
		return errors.New("Jump cannot have a function, arguments, policies, nested routes, or parallel members")
	}
	index, exists := f.targets[action.jump.name]
	switch {
	case f.inParallel:
		return errors.New("Jump cannot run inside a parallel member; return it from the completed group's callback")
	case strings.TrimSpace(action.jump.name) == "":
		return errors.New("jump target name must not be empty")
	case !exists:
		return fmt.Errorf("jump target %q does not identify an explicitly Named top-level step", action.jump.name)
	case index < 0:
		return fmt.Errorf("jump target %q is ambiguous; use a unique step name", action.jump.name)
	default:
		return nil
	}
}
