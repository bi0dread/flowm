package flowm

import (
	"errors"
	"fmt"
	"reflect"
)

// Outcome is the completed result of one action after its attempts and policies.
// Values contains non-error returns from the final attempt. Err is the underlying
// error, nil on success. Attempts counts calls to this action, not its branches.
// Predicates should treat Values and the objects it contains as read-only.
type Outcome struct {
	Name     string
	Values   []interface{}
	Err      error
	Attempts int
}

type route struct {
	predicate func(Outcome) bool
	action    Action
}

var outcomeType = reflect.TypeOf(Outcome{})

func (o Outcome) failure() error {
	if o.Err == nil {
		return nil
	}
	return &ActionError{Name: o.Name, Attempts: o.Attempts, Err: o.Err}
}

func (f *flowm) reportOutcome(outcome Outcome) {
	if f.hooks == nil {
		return
	}
	if outcome.Err != nil {
		if f.hooks.OnError != nil {
			f.hooks.OnError(outcome.Name, outcome.failure())
		}
	} else if f.hooks.OnSuccess != nil {
		f.hooks.OnSuccess(outcome.Name, outcome.Values)
	}
}

func (f *flowm) runAction(st stub) Outcome {
	values, err, attempts := f.executeAction(st)
	outcome := Outcome{Name: st.Name, Values: filterErrorValues(values), Err: err, Attempts: attempts}
	if canceled := f.ctx.Err(); canceled != nil {
		if !errors.Is(outcome.Err, canceled) {
			outcome.Err = errors.Join(outcome.Err, canceled)
		}
		f.reportOutcome(outcome)
		return outcome
	}
	f.reportOutcome(outcome)
	for _, rule := range st.Action.routes {
		name := actionName(rule.action)
		if canceled := f.ctx.Err(); canceled != nil {
			return f.routingFailure(name, outcome.Err, canceled)
		}
		matched, predicateErr := matchOutcome(rule.predicate, outcome)
		if predicateErr != nil {
			return f.routingFailure(name, outcome.Err, predicateErr)
		}
		if canceled := f.ctx.Err(); canceled != nil {
			return f.routingFailure(name, outcome.Err, canceled)
		}
		if !matched {
			if f.hooks != nil && f.hooks.OnSkip != nil {
				f.hooks.OnSkip(name)
			}
			continue
		}
		branch := bindOutcome(rule.action, outcome)
		return f.runAction(stub{Name: name, Action: branch})
	}
	return outcome
}

func (f *flowm) routingFailure(name string, original, err error) Outcome {
	outcome := Outcome{Name: name, Err: errors.Join(original, err)}
	f.reportOutcome(outcome)
	return outcome
}

func matchOutcome(predicate func(Outcome) bool, outcome Outcome) (matched bool, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("When predicate panic: %v", recovered)
		}
	}()
	return predicate(outcome), nil
}

// A branch may request Outcome as its first parameter after an optional context.
// Other parameters are explicit; parent values are never appended implicitly.
func bindOutcome(action Action, outcome Outcome) Action {
	t, err := functionType(action.Func)
	if err != nil {
		return action // preflight reports invalid branch definitions
	}
	position, argument := 0, 0
	if t.NumIn() > 0 && t.In(0) == contextType {
		position = 1
		if explicitContext(action.Params) && (t.IsVariadic() || len(action.Params) >= t.NumIn()-1) {
			argument = 1
		}
	}
	if t.NumIn() > position && t.In(position) == outcomeType {
		params := make([]interface{}, 0, len(action.Params)+1)
		params = append(params, action.Params[:argument]...)
		params = append(params, outcome)
		action.Params = append(params, action.Params[argument:]...)
	}
	return action
}
