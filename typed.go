package flowm

import "context"

// TypedStep is a compile-time-typed chain of actions: building it with Step,
// StepCtx, and Then guarantees that each step's output type matches the next
// step's input type at compile time. Attach policies per step with With, then
// execute the chain with RunTyped.
type TypedStep[I, O any] struct {
	actions []Action
}

// Step wraps a typed function as a single-step chain.
func Step[I, O any](fn func(I) (O, error)) TypedStep[I, O] {
	return TypedStep[I, O]{actions: []Action{{Func: fn}}}
}

// StepCtx wraps a context-aware typed function; the flow's context is
// injected when the chain runs.
func StepCtx[I, O any](fn func(context.Context, I) (O, error)) TypedStep[I, O] {
	return TypedStep[I, O]{actions: []Action{{Func: fn}}}
}

// With attaches policies to the last step of the chain.
func (s TypedStep[I, O]) With(policies ...Policy) TypedStep[I, O] {
	if len(s.actions) == 0 {
		return s
	}
	out := make([]Action, len(s.actions))
	copy(out, s.actions)
	out[len(out)-1] = out[len(out)-1].With(policies...)
	s.actions = out
	return s
}

// Then composes two typed chains; mismatched types fail at compile time.
// Each step keeps its own policies.
func Then[I, M, O any](first TypedStep[I, M], next TypedStep[M, O]) TypedStep[I, O] {
	combined := make([]Action, 0, len(first.actions)+len(next.actions))
	combined = append(combined, first.actions...)
	combined = append(combined, next.actions...)
	return TypedStep[I, O]{actions: combined}
}

// RunTyped executes the chain on a fresh flow with the given context and
// input, returning the typed result of the final step.
func RunTyped[I, O any](ctx context.Context, s TypedStep[I, O], input I) (O, error) {
	var out O
	f := New().WithContext(ctx)
	for i := range s.actions {
		a := s.actions[i]
		if i == 0 {
			a.Params = append([]interface{}{input}, a.Params...)
		}
		f.AddAction(a)
		f.PipeAll()
	}
	f.Do(func(v O) { out = v })
	if err := f.Start(); err != nil {
		return out, err
	}
	return out, nil
}
