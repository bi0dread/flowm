package flowm

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
)

var (
	contextType = reflect.TypeOf((*context.Context)(nil)).Elem()
	errorType   = reflect.TypeOf((*error)(nil)).Elem()
)

// Validate checks all action definitions, explicit arguments, built-in policy
// settings, callbacks, and parallel members without invoking them. Returned
// actions, jump targets, and dynamic piped arguments are checked at runtime.
// Start calls Validate automatically.
func (f *flowm) Validate() error {
	_, err := f.validate()
	return err
}

func (f *flowm) validate() (map[string]int, error) {
	targets, errs := f.indexTargets()
	// A callback can return any named jump target, including from a nested
	// branch. If its source pipes values, named targets may receive them.
	dynamicPiping := false
	for _, st := range f.stubStorage {
		dynamicPiping = dynamicPiping || (len(st.Action.routes) > 0 && hasPiping(st.Action))
	}
	mayReceiveValues := false
	for _, st := range f.stubStorage {
		if err := validateAction(st.Action, mayReceiveValues || (dynamicPiping && st.Action.Name != ""), false); err != nil {
			errs = append(errs, err)
		}
		mayReceiveValues = hasPiping(st.Action)
	}
	return targets, errors.Join(errs...)
}

func hasPiping(action Action) bool {
	for _, policy := range action.Policies {
		switch policy.(type) {
		case *PipeAllPolicy, *PipeIndicesPolicy, *PipeMapPolicy:
			return true
		}
	}
	return false
}

func validateAction(action Action, mayReceiveValues, parallelMember bool) (err error) {
	defer func() {
		if err != nil {
			err = &ActionError{Name: actionName(action), Err: err}
		}
	}()
	if action.jump != nil {
		return errors.New("Jump must be returned from a When callback")
	}
	if action.parallel == nil {
		if _, err := prepareArguments(action.Func, action.Params, context.Background(), mayReceiveValues); err != nil {
			return err
		}
	} else {
		var errs []error
		for _, member := range action.parallel.actions {
			if err := validateAction(member, false, true); err != nil {
				errs = append(errs, err)
			}
		}
		if err := errors.Join(errs...); err != nil {
			return err
		}
	}
	seenTimeout, seenLimit := false, false
	for _, policy := range action.Policies {
		if policy == nil || nilValue(reflect.ValueOf(policy)) {
			return errors.New("policy must not be nil")
		}
		switch p := policy.(type) {
		case *RetryPolicy:
			if p.Attempts < 0 || p.Delay < 0 || p.MaxDelay < 0 {
				return errors.New("retry attempts and delays must not be negative")
			}
			if math.IsNaN(p.BackoffFactor) || math.IsInf(p.BackoffFactor, 0) || p.BackoffFactor < 0 || (p.BackoffFactor > 0 && p.BackoffFactor < 1) {
				return errors.New("retry backoff factor must be zero or a finite value >= 1")
			}
		case *TimeoutPolicy:
			if seenTimeout || p.PerAttempt < 0 || p.Overall < 0 {
				return errors.New("use one timeout policy with non-negative durations")
			}
			seenTimeout = true
		case *RateLimitPolicy:
			if p.Per <= 0 || p.Burst < 1 {
				return errors.New("rate limit requires Per > 0 and Burst >= 1")
			}
		case *CircuitBreakerPolicy:
			if p.Failures < 1 || p.Window <= 0 || p.Cooldown <= 0 {
				return errors.New("circuit breaker requires positive Failures, Window, and Cooldown")
			}
		case *WorkerPoolPolicy:
			if seenLimit || p.Size < 1 {
				return errors.New("use one positive concurrency limit")
			}
			if action.parallel == nil && !parallelMember {
				return errors.New("concurrency limits require a parallel group")
			}
			seenLimit = true
		case *PipeIndicesPolicy:
			for _, index := range p.Indices {
				if index < 0 {
					return errors.New("pipe indices must not be negative")
				}
			}
		case *PipeMapPolicy:
			if p.Mapper == nil {
				return errors.New("pipe mapper must not be nil")
			}
		}
	}
	for _, decide := range action.routes {
		if decide == nil {
			return errors.New("When requires a non-nil callback")
		}
	}
	if action.parallel != nil {
		_, err := parallelLimit(action)
		return err
	}
	return nil
}

func functionType(function interface{}) (reflect.Type, error) {
	value := reflect.ValueOf(function)
	if !value.IsValid() || value.Kind() != reflect.Func || value.IsNil() {
		return nil, fmt.Errorf("expected a non-nil function, got %T", function)
	}
	return value.Type(), nil
}

// prepareArguments is shared by preflight and execution. During preflight a
// known prefix is sufficient when a preceding step may supply piped arguments.
func prepareArguments(function interface{}, params []interface{}, ctx context.Context, partial bool) ([]reflect.Value, error) {
	t, err := functionType(function)
	if err != nil {
		return nil, err
	}
	if ctx != nil && t.NumIn() > 0 && t.In(0) == contextType {
		inject := !explicitContext(params)
		if !partial && !t.IsVariadic() {
			// Preserve the original exact-arity rule, including functions that
			// accept another context as an ordinary argument.
			inject = len(params) != t.NumIn()
		}
		if inject {
			params = append([]interface{}{ctx}, params...)
		}
	}
	minimum := t.NumIn()
	if t.IsVariadic() {
		minimum--
	}
	if (!partial && len(params) < minimum) || (!t.IsVariadic() && len(params) > t.NumIn()) {
		return nil, fmt.Errorf("expected %d argument(s), got %d", minimum, len(params))
	}
	args := make([]reflect.Value, len(params))
	for i, param := range params {
		var expected reflect.Type
		if t.IsVariadic() && i >= minimum {
			expected = t.In(t.NumIn() - 1).Elem()
		} else {
			expected = t.In(i)
		}
		if param == nil {
			if !nilable(expected.Kind()) {
				return nil, fmt.Errorf("argument %d: nil is not assignable to %v", i+1, expected)
			}
			args[i] = reflect.Zero(expected)
			continue
		}
		value := reflect.ValueOf(param)
		if !value.Type().AssignableTo(expected) {
			return nil, fmt.Errorf("argument %d: %v is not assignable to %v", i+1, value.Type(), expected)
		}
		args[i] = value
	}
	return args, nil
}

func explicitContext(params []interface{}) bool {
	return len(params) > 0 && params[0] != nil && reflect.TypeOf(params[0]).AssignableTo(contextType)
}

func nilable(kind reflect.Kind) bool {
	switch kind {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice, reflect.UnsafePointer:
		return true
	}
	return false
}

func nilValue(value reflect.Value) bool {
	return !value.IsValid() || (nilable(value.Kind()) && value.IsNil())
}
