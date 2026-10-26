package flowm

import "fmt"

// ActionError identifies a failed action while preserving its underlying error.
// Attempts counts actual calls to the primary function, including retries. It is
// zero for validation failures, predicate failures, and rejected execution.
// A selected branch has its own name and attempt count.
type ActionError struct {
	Name     string
	Attempts int
	Err      error
}

func (e *ActionError) Error() string {
	return fmt.Sprintf("action %q failed after %d attempt(s): %v", e.Name, e.Attempts, e.Err)
}

func (e *ActionError) Unwrap() error { return e.Err }
