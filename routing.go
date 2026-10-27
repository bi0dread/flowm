package flowm

import (
	"errors"
	"fmt"
	"reflect"
)

// Outcome is the completed result of one action after its attempts and policies.
// Values contains non-error returns from the final attempt. Err is the underlying
// error, nil on success. Attempts counts calls to this action, not its branches.
// Decision callbacks should treat Values and its contents as read-only.
type Outcome struct {
	Name     string
	Values   []interface{}
	Err      error
	Attempts int
}

type route func(Outcome) Action

func (a Action) empty() bool {
	return !a.defined && a.Name == "" && a.Func == nil && len(a.Params) == 0 &&
		len(a.Policies) == 0 && len(a.routes) == 0 && a.parallel == nil && a.jump == nil
}

// A jump is execution control, not an action error or a user return value.
type actionResult struct {
	Outcome
	jump *jumpTarget
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

func (f *flowm) runAction(st stub) actionResult {
actionLoop:
	for {
		values, err, attempts := f.executeAction(st)
		outcome := Outcome{Name: st.Name, Values: filterErrorValues(values), Err: err, Attempts: attempts}
		if canceled := f.ctx.Err(); canceled != nil {
			if !errors.Is(outcome.Err, canceled) {
				outcome.Err = errors.Join(outcome.Err, canceled)
			}
			f.reportOutcome(outcome)
			return actionResult{Outcome: outcome}
		}
		f.reportOutcome(outcome)
		for _, decide := range st.Action.routes {
			if canceled := f.ctx.Err(); canceled != nil {
				return f.routingFailure(st.Name, outcome.Err, canceled)
			}
			branch, decisionErr := decideOutcome(decide, outcome)
			if decisionErr != nil {
				return f.routingFailure(st.Name, outcome.Err, decisionErr)
			}
			if canceled := f.ctx.Err(); canceled != nil {
				return f.routingFailure(st.Name, outcome.Err, canceled)
			}
			if branch.empty() {
				if f.hooks != nil && f.hooks.OnSkip != nil {
					f.hooks.OnSkip(st.Name)
				}
				continue
			}
			name := actionName(branch)
			if branch.jump != nil {
				if err := f.validateJump(branch); err != nil {
					return f.routingFailure(name, outcome.Err, err)
				}
				outcome.Err = nil
				return actionResult{Outcome: outcome, jump: branch.jump}
			}
			if hasPiping(branch) {
				return f.routingFailure(name, outcome.Err, errors.New("put piping on the source step, not the returned action"))
			}
			branch = bindOutcome(branch, outcome)
			if err := validateAction(branch, false, false); err != nil {
				return f.routingFailure(name, outcome.Err, err)
			}
			st = stub{Name: name, Action: branch}
			continue actionLoop
		}
		return actionResult{Outcome: outcome}
	}
}

func (f *flowm) routingFailure(name string, original, err error) actionResult {
	outcome := Outcome{Name: name, Err: errors.Join(original, err)}
	f.reportOutcome(outcome)
	return actionResult{Outcome: outcome}
}

func decideOutcome(decide func(Outcome) Action, outcome Outcome) (action Action, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("When callback panic: %v", recovered)
		}
	}()
	return decide(outcome), nil
}

// A branch may request Outcome as its first parameter after an optional context.
// Other parameters are explicit; parent values are never appended implicitly.
func bindOutcome(action Action, outcome Outcome) Action {
	t, err := functionType(action.Func)
	if err != nil {
		return action // validation reports invalid branch definitions
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
