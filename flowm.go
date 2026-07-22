package flowm

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"time"
)

type Action struct {
	Func     interface{}
	Params   []interface{}
	Policies []Policy
}

type stub struct {
	Name   string
	Action Action
}

type Flowm interface {
	AddAction(action Action)
	WithContext(ctx context.Context) Flowm
	WithHooks(h Hooks) Flowm
	Do(function interface{}, params ...interface{}) Flowm
	With(policy Policy) Flowm
	PipeAll() Flowm
	Pipe(indices ...int) Flowm
	Map(mapper func([]interface{}) []interface{}) Flowm
	ContinueOnError() Flowm
	OnError(function interface{}, params ...interface{}) Flowm
	When(pred func(piped []interface{}) bool) Flowm
	Compensate(function interface{}, params ...interface{}) Flowm
	AddParallel(actions ...Action) Flowm
	DoFlow(sub Flowm) Flowm
	ForEach(function interface{}, policies ...Policy) Flowm
	Start() error
	StartWithResult() ([]interface{}, error)
}

type flowm struct {
	stubStorage []stub
	ctx         context.Context
	hooks       *Hooks
	lastIndex   int
}

func New() Flowm {
	return &flowm{
		stubStorage: make([]stub, 0),
		ctx:         context.Background(),
		hooks:       &Hooks{},
		lastIndex:   -1,
	}
}

func (f *flowm) AddAction(act Action) {
	name, _ := f.getFunctionName(act.Func)
	f.stubStorage = append(f.stubStorage, stub{Name: name, Action: act})
	f.lastIndex = len(f.stubStorage) - 1
}

func (f *flowm) Start() error {
	_, err := f.StartWithResult()
	return err
}

// StartWithResult runs the flow and returns the last executed action's
// non-error return values alongside the flow error.
func (f *flowm) StartWithResult() ([]interface{}, error) {
	var pipedValues []interface{}
	hasPiped := false
	var lastResult []interface{}
	var compensations []stub
	for i := range f.stubStorage {
		st := f.stubStorage[i]
		if cp := conditionPolicyOf(st.Action); cp != nil && cp.If != nil && !cp.If(pipedValues) {
			// skipped actions are transparent: piped values stay available
			continue
		}
		if fe := forEachPolicyOf(st.Action); fe != nil {
			st = f.buildForEachStub(st, fe, hasPiped, pipedValues)
			hasPiped = false
			pipedValues = nil
		} else if hasPiped {
			if st.Action.Params == nil {
				st.Action.Params = []interface{}{}
			}
			st.Action.Params = append(st.Action.Params, pipedValues...)
			hasPiped = false
			pipedValues = nil
		}
		result, err, pipeNext, pipeVals, continueOnErr := f.runSingleAction(st)
		lastResult = result
		if pipeNext {
			pipedValues = pipeVals
			hasPiped = true
		}
		if err != nil {
			if continueOnErr {
				// swallow error and continue
				continue
			}
			if cerr := f.runCompensations(compensations); cerr != nil {
				return nil, errors.Join(err, cerr)
			}
			return nil, err
		}
		if compensatePolicyOf(st.Action) != nil {
			compensations = append(compensations, st)
		}
	}
	return filterErrorValues(lastResult), nil
}

// runCompensations executes registered compensations in reverse order.
func (f *flowm) runCompensations(compensations []stub) error {
	var errs []error
	for i := len(compensations) - 1; i >= 0; i-- {
		cp := compensatePolicyOf(compensations[i].Action)
		func() {
			defer func() {
				if r := recover(); r != nil {
					errs = append(errs, fmt.Errorf("compensation panic: %v", r))
				}
			}()
			if _, cerr := f.callFunc(cp.Func, cp.Params...); cerr != nil {
				errs = append(errs, cerr)
			}
		}()
	}
	return errors.Join(errs...)
}

// buildForEachStub turns a ForEach action into a parallel group with one
// member per piped element. A single piped slice is expanded into elements;
// non-ForEach policies (piping, continue-on-error, ...) carry over to the group.
func (f *flowm) buildForEachStub(st stub, fe *ForEachPolicy, hasPiped bool, pipedValues []interface{}) stub {
	var elems []interface{}
	if hasPiped {
		elems = pipedValues
		if len(pipedValues) == 1 {
			rv := reflect.ValueOf(pipedValues[0])
			if rv.IsValid() && (rv.Kind() == reflect.Slice || rv.Kind() == reflect.Array) {
				elems = make([]interface{}, rv.Len())
				for j := 0; j < rv.Len(); j++ {
					elems[j] = rv.Index(j).Interface()
				}
			}
		}
	}
	acts := make([]Action, len(elems))
	for j, e := range elems {
		params := make([]interface{}, 0, len(st.Action.Params)+1)
		params = append(params, st.Action.Params...)
		params = append(params, e)
		acts[j] = Action{Func: st.Action.Func, Params: params, Policies: fe.Policies}
	}
	par := f.newParallelAction(acts, nil)
	for _, p := range st.Action.Policies {
		if _, isFE := p.(*ForEachPolicy); !isFE {
			par.Policies = append(par.Policies, p)
		}
	}
	return stub{Name: st.Name, Action: par}
}

func conditionPolicyOf(a Action) *ConditionPolicy {
	for _, p := range a.Policies {
		if c, ok := p.(*ConditionPolicy); ok {
			return c
		}
	}
	return nil
}

func compensatePolicyOf(a Action) *CompensatePolicy {
	for _, p := range a.Policies {
		if c, ok := p.(*CompensatePolicy); ok {
			return c
		}
	}
	return nil
}

func forEachPolicyOf(a Action) *ForEachPolicy {
	for _, p := range a.Policies {
		if fe, ok := p.(*ForEachPolicy); ok {
			return fe
		}
	}
	return nil
}

// filterErrorValues drops error-typed values from a result set.
func filterErrorValues(vals []interface{}) []interface{} {
	filtered := make([]interface{}, 0, len(vals))
	for _, v := range vals {
		if _, isErr := v.(error); isErr {
			continue
		}
		filtered = append(filtered, v)
	}
	return filtered
}

func NewAction(function interface{}, Params []interface{}, Policy ...Policy) Action {
	return Action{
		Func:     function,
		Params:   Params,
		Policies: Policy,
	}
}

// Do builds an Action with the same signature as Flowm.Do; attach policies by
// chaining, e.g. flowm.Do(fn, arg).With(retry).OnError(fallback). Intended for
// AddParallel and anywhere an Action value is needed.
func Do(function interface{}, params ...interface{}) Action {
	return Action{Func: function, Params: params}
}

// With returns a copy of the action with the policies attached.
func (a Action) With(policies ...Policy) Action {
	combined := make([]Policy, 0, len(a.Policies)+len(policies))
	combined = append(combined, a.Policies...)
	combined = append(combined, policies...)
	a.Policies = combined
	return a
}

// OnError attaches a fallback function to the action, like Flowm.OnError.
func (a Action) OnError(function interface{}, params ...interface{}) Action {
	return a.With(&FallbackPolicy{Func: function, Params: params})
}

// Compensate attaches a rollback function to the action, like Flowm.Compensate.
// Compensations are tracked for top-level flow actions, not parallel members.
func (a Action) Compensate(function interface{}, params ...interface{}) Action {
	return a.With(&CompensatePolicy{Func: function, Params: params})
}

func (f *flowm) callFunc(funcValue interface{}, params ...interface{}) (result []interface{}, err error) {
	function := reflect.ValueOf(funcValue)
	// inject context if first param is context.Context and provided args are one fewer
	expected := function.Type().NumIn()
	if expected > 0 && function.Type().In(0) == reflect.TypeOf((*context.Context)(nil)).Elem() {
		if len(params) == expected-1 {
			injected := []interface{}{f.ctx}
			injected = append(injected, params...)
			return callReflect(funcValue, injected...)
		}
	}
	return callReflect(funcValue, params...)
}

func callReflect(funcValue interface{}, params ...interface{}) (result []interface{}, err error) {
	function := reflect.ValueOf(funcValue)
	if len(params) != function.Type().NumIn() {
		err = errors.New("The number of params is out of index")
		return
	}
	in := make([]reflect.Value, len(params))
	for k, param := range params {
		in[k] = reflect.ValueOf(param)
	}
	var res []reflect.Value
	res = function.Call(in)
	var mainErr error
	for _, v := range res {
		if v.Type().ConvertibleTo(reflect.TypeOf((*error)(nil)).Elem()) {
			if v.IsNil() {
				continue
			}
			if err = v.Interface().(error); err != nil {
				mainErr = err
			}
		}
		result = append(result, v.Interface())
	}
	return result, mainErr
}

func (f *flowm) runSingleAction(st stub) (result []interface{}, err error, pipeNext bool, pipeVals []interface{}, continueOnErr bool) {
	// propagate context to policies if supported
	for _, p := range st.Action.Policies {
		if cp, ok := p.(ContextAwarePolicy); ok {
			cp.SetContext(f.ctx)
		}
		if rh, ok := p.(RetryHookAwarePolicy); ok {
			if f.hooks != nil && f.hooks.OnRetry != nil {
				rh.SetOnRetry(f.hooks.OnRetry)
			}
		}
		if fa, ok := p.(FallbackAwarePolicy); ok {
			fa.SetFlowCaller(f.callFunc)
			fa.SetResultSink(func(res []interface{}) { result = res })
		}
	}
	var perAttempt, overall time.Duration
	for _, p := range st.Action.Policies {
		if t, ok := p.(*TimeoutPolicy); ok {
			perAttempt, overall = t.PerAttempt, t.Overall
			break
		}
	}
	var overallDeadline time.Time
	if overall > 0 {
		overallDeadline = time.Now().Add(overall)
	}
	// executeOnce runs one full attempt: pre-execution checks (rate limit,
	// circuit breaker), context injection, timeouts, and panic recovery. Both
	// the initial call and every retry go through it. Executed attempts are
	// reported to AttemptObserverPolicy implementations; attempts rejected by
	// a pre-execution policy or an exhausted overall budget are not.
	executeOnce := func() (res []interface{}, execErr error) {
		executed := false
		defer func() {
			if r := recover(); r != nil {
				res = nil
				execErr = fmt.Errorf("panic: %v", r)
			}
			if executed {
				for _, p := range st.Action.Policies {
					if ob, ok := p.(AttemptObserverPolicy); ok {
						ob.ObserveAttempt(execErr)
					}
				}
			}
		}()
		timeout := perAttempt
		if overall > 0 {
			remaining := time.Until(overallDeadline)
			if remaining <= 0 {
				return nil, context.DeadlineExceeded
			}
			if timeout == 0 || remaining < timeout {
				timeout = remaining
			}
		}
		for _, p := range st.Action.Policies {
			if pre, ok := p.(PreExecutionPolicy); ok {
				if perr := pre.Before(f.ctx, st.Action); perr != nil {
					return nil, perr
				}
			}
		}
		executed = true
		if timeout > 0 {
			return f.callWithTimeout(st.Action, timeout)
		}
		return f.callFunc(st.Action.Func, st.Action.Params...)
	}
	for _, p := range st.Action.Policies {
		if ea, ok := p.(ExecutorAwarePolicy); ok {
			ea.SetExecutor(func() ([]interface{}, error) {
				res, execErr := executeOnce()
				if execErr == nil {
					// a successful retry replaces the piped result
					result = res
				}
				return res, execErr
			})
		}
	}
	if f.hooks != nil && f.hooks.OnStart != nil {
		f.hooks.OnStart(st.Name)
	}
	result, err = executeOnce()
	for _, policy := range st.Action.Policies {
		if c, ok := policy.(*ContinueOnErrorPolicy); ok && c.Continue {
			continueOnErr = true
		}
		if t, ok := policy.(*TerminatePolicy); ok && t.Terminate {
			continueOnErr = true
		}
		err = policy.Do(st.Action, err)
	}
	if len(result) > 0 {
		// prepare values for piping: drop error-typed returns and expand nested []interface{}
		filtered := filterErrorValues(result)
		// helper to set pipe all semantics
		setPipeAll := func(vals []interface{}) {
			if len(vals) == 1 {
				if inner, ok := vals[0].([]interface{}); ok {
					pipeNext = true
					pipeVals = inner
					return
				}
			}
			pipeNext = true
			pipeVals = vals
		}
		for _, policy := range st.Action.Policies {
			switch p := policy.(type) {
			case *PassValuesPolicy, *PipeAllPolicy:
				setPipeAll(filtered)
			case *PipeIndicesPolicy:
				out := make([]interface{}, 0, len(p.Indices))
				for _, idx := range p.Indices {
					if idx >= 0 && idx < len(filtered) {
						out = append(out, filtered[idx])
					}
				}
				setPipeAll(out)
			case *PipeMapPolicy:
				if p.Mapper != nil {
					setPipeAll(p.Mapper(filtered))
				}
			}
		}
	}
	if err != nil {
		if f.hooks != nil && f.hooks.OnError != nil {
			f.hooks.OnError(st.Name, err)
		}
	} else {
		if f.hooks != nil && f.hooks.OnSuccess != nil {
			f.hooks.OnSuccess(st.Name, result)
		}
	}
	return
}

func (f *flowm) callWithTimeout(a Action, perAttempt time.Duration) ([]interface{}, error) {
	type callRes struct {
		res []interface{}
		err error
	}
	ch := make(chan callRes, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				ch <- callRes{nil, fmt.Errorf("panic: %v", r)}
			}
		}()
		r, e := f.callFunc(a.Func, a.Params...)
		ch <- callRes{r, e}
	}()
	select {
	case <-f.ctx.Done():
		return nil, f.ctx.Err()
	case out := <-ch:
		return out.res, out.err
	case <-time.After(perAttempt):
		return nil, context.DeadlineExceeded
	}
}

func (f *flowm) newParallelAction(actions []Action, reducer func([][]interface{}) []interface{}) Action {
	if reducer == nil {
		reducer = func(results [][]interface{}) []interface{} {
			flat := []interface{}{}
			for _, r := range results {
				flat = append(flat, r...)
			}
			return flat
		}
	}
	runner := func() ([]interface{}, error) {
		type r struct {
			res []interface{}
			err error
		}
		wg := sync.WaitGroup{}
		out := make([]r, len(actions))
		var sem chan struct{}
		semSize := 0
		for _, a := range actions {
			for _, p := range a.Policies {
				if wp, ok := p.(*WorkerPoolPolicy); ok && wp.Size > 0 {
					semSize = wp.Size
					break
				}
			}
		}
		if semSize > 0 {
			sem = make(chan struct{}, semSize)
		}
		for i := range actions {
			i := i
			wg.Add(1)
			go func() {
				defer wg.Done()
				if sem != nil {
					sem <- struct{}{}
					defer func() { <-sem }()
				}
				st := stub{Name: functionNameOf(actions[i].Func), Action: actions[i]}
				rres, rerr, _, _, _ := f.runSingleAction(st)
				out[i] = r{res: rres, err: rerr}
			}()
		}
		wg.Wait()
		var merr MultiError
		results := make([][]interface{}, len(out))
		for i := range out {
			if out[i].err != nil {
				merr.Append(out[i].err)
			}
			results[i] = out[i].res
		}
		if merr.Len() > 0 {
			return reducer(results), merr
		}
		return reducer(results), nil
	}
	return NewAction(runner, nil)
}

func (f *flowm) getFunctionName(i interface{}) (name string, isMethod bool) {
	if fullName, ok := i.(string); ok {
		return fullName, false
	}
	fullName := runtime.FuncForPC(reflect.ValueOf(i).Pointer()).Name()
	isMethod = strings.ContainsAny(fullName, "*")
	elements := strings.Split(fullName, ".")
	shortName := elements[len(elements)-1]

	return strings.TrimSuffix(shortName, "-fm"), isMethod
}

type Policy interface {
	Do(action Action, err error) error
}

type PreExecutionPolicy interface {
	Before(ctx context.Context, action Action) error
}

type ContextAwarePolicy interface {
	SetContext(ctx context.Context)
}

type RetryHookAwarePolicy interface {
	SetOnRetry(fn func(actionName string, attempt int, err error))
}

// ExecutorAwarePolicy lets a policy re-run the action through the flow's
// normal execution path (pre-execution checks, context injection, per-attempt
// timeout, panic recovery) instead of invoking the raw function directly.
type ExecutorAwarePolicy interface {
	SetExecutor(fn func() ([]interface{}, error))
}

// FallbackAwarePolicy lets a policy invoke alternative functions through the
// flow's calling machinery (context injection) and replace the action's piped
// result when it recovers from an error.
type FallbackAwarePolicy interface {
	SetFlowCaller(call func(fn interface{}, params ...interface{}) ([]interface{}, error))
	SetResultSink(sink func([]interface{}))
}

// AttemptObserverPolicy is notified after every executed attempt (the initial
// call and each retry) with the attempt's error, nil on success. Attempts
// rejected by a pre-execution policy are not reported.
type AttemptObserverPolicy interface {
	ObserveAttempt(err error)
}

type RetryPolicy struct {
	Attempts      int
	Delay         time.Duration
	MaxDelay      time.Duration
	BackoffFactor float64
	Jitter        bool
	Fn            func() error
	RetryIf       func(error) bool
	MaxElapsed    time.Duration
	ctx           context.Context
	onRetry       func(actionName string, attempt int, err error)
	execute       func() ([]interface{}, error)
}

// PassValuesPolicy is deprecated: it behaves like PipeAllPolicy.
type PassValuesPolicy struct{}

func (r *PassValuesPolicy) Do(action Action, mainErr error) error {
	return mainErr
}

// ConditionPolicy skips the action when If returns false. The predicate
// receives the values piped to this action (nil when nothing was piped);
// skipped actions are transparent — piped values stay available for the next
// action. Sugar: Flowm.When.
type ConditionPolicy struct {
	If func(piped []interface{}) bool
}

func (c *ConditionPolicy) Do(action Action, mainErr error) error { return mainErr }

// CompensatePolicy registers a compensation (rollback) function for the
// action. When a later action fails terminally, compensations of previously
// succeeded actions run in reverse order. Sugar: Flowm.Compensate.
type CompensatePolicy struct {
	Func   interface{}
	Params []interface{}
}

func (c *CompensatePolicy) Do(action Action, mainErr error) error { return mainErr }

// ForEachPolicy marks an action added via Flowm.ForEach: its function runs
// once per piped element (concurrently), with Policies applied per element.
type ForEachPolicy struct {
	Policies []Policy
}

func (fe *ForEachPolicy) Do(action Action, mainErr error) error { return mainErr }

type TerminatePolicy struct {
	Terminate bool
}

func (r *TerminatePolicy) Do(action Action, mainErr error) error {
	return mainErr
}

type ContinueOnErrorPolicy struct{ Continue bool }

func (c *ContinueOnErrorPolicy) Do(action Action, mainErr error) error { return mainErr }

// FallbackPolicy runs an alternative function when the action fails. If the
// fallback succeeds, the error is swallowed and the fallback's return values
// replace the action's outputs for piping. The action's error is passed to
// the fallback if it declares a leading error parameter (after an optional
// context.Context). Chain multiple fallbacks: the next one runs only if the
// previous fallback also failed.
type FallbackPolicy struct {
	Func   interface{}
	Params []interface{}
	caller func(fn interface{}, params ...interface{}) ([]interface{}, error)
	sink   func([]interface{})
}

func (p *FallbackPolicy) SetFlowCaller(call func(fn interface{}, params ...interface{}) ([]interface{}, error)) {
	p.caller = call
}

func (p *FallbackPolicy) SetResultSink(sink func([]interface{})) { p.sink = sink }

func (p *FallbackPolicy) Do(action Action, mainErr error) (err error) {
	if mainErr == nil || p.Func == nil {
		return mainErr
	}
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	params := p.Params
	if fnType := reflect.TypeOf(p.Func); fnType != nil && fnType.Kind() == reflect.Func {
		params = injectFallbackError(fnType, params, mainErr)
	}
	call := p.caller
	if call == nil {
		// standalone use outside a flow: no caller injected
		call = func(fn interface{}, ps ...interface{}) ([]interface{}, error) { return callReflect(fn, ps...) }
	}
	res, ferr := call(p.Func, params...)
	if ferr != nil {
		return ferr
	}
	if p.sink != nil {
		p.sink(res)
	}
	return nil
}

// injectFallbackError prepends the action's error to the fallback params when
// the function declares an error parameter first (after an optional context).
func injectFallbackError(fnType reflect.Type, params []interface{}, mainErr error) []interface{} {
	errType := reflect.TypeOf((*error)(nil)).Elem()
	ctxType := reflect.TypeOf((*context.Context)(nil)).Elem()
	pos := 0
	if fnType.NumIn() > 0 && fnType.In(0) == ctxType {
		pos = 1
	}
	if fnType.NumIn() > pos && fnType.In(pos) == errType && len(params) == fnType.NumIn()-pos-1 {
		injected := make([]interface{}, 0, len(params)+1)
		injected = append(injected, mainErr)
		injected = append(injected, params...)
		return injected
	}
	return params
}

type PipeAllPolicy struct{}

type PipeIndicesPolicy struct{ Indices []int }

type PipeMapPolicy struct {
	Mapper func([]interface{}) []interface{}
}

func (p *PipeAllPolicy) Do(action Action, mainErr error) error     { return mainErr }
func (p *PipeIndicesPolicy) Do(action Action, mainErr error) error { return mainErr }
func (p *PipeMapPolicy) Do(action Action, mainErr error) error     { return mainErr }

type TimeoutPolicy struct {
	PerAttempt time.Duration
	Overall    time.Duration
}

func (t *TimeoutPolicy) Do(action Action, mainErr error) error { return mainErr }

type CircuitBreakerPolicy struct {
	Failures int
	Window   time.Duration
	Cooldown time.Duration

	mu           sync.Mutex
	failureTimes []time.Time
	openUntil    time.Time
}

func (c *CircuitBreakerPolicy) Before(ctx context.Context, action Action) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if now.Before(c.openUntil) {
		return errors.New("circuit breaker open")
	}
	cutoff := now.Add(-c.Window)
	j := 0
	for _, ts := range c.failureTimes {
		if ts.After(cutoff) {
			c.failureTimes[j] = ts
			j++
		}
	}
	c.failureTimes = c.failureTimes[:j]
	return nil
}

// ObserveAttempt records every executed attempt's failure (including retries),
// opening the breaker when the threshold is reached within the window.
func (c *CircuitBreakerPolicy) ObserveAttempt(err error) {
	if err == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	c.failureTimes = append(c.failureTimes, now)
	cutoff := now.Add(-c.Window)
	cnt := 0
	for _, ts := range c.failureTimes {
		if ts.After(cutoff) {
			cnt++
		}
	}
	if cnt >= c.Failures {
		c.openUntil = now.Add(c.Cooldown)
	}
}

func (c *CircuitBreakerPolicy) Do(action Action, mainErr error) error { return mainErr }

type RateLimitPolicy struct {
	Per    time.Duration
	Burst  int
	mu     sync.Mutex
	tokens float64
	last   time.Time
}

func (r *RateLimitPolicy) Before(ctx context.Context, action Action) error {
	r.mu.Lock()
	if r.last.IsZero() {
		r.last = time.Now()
		r.tokens = float64(r.Burst)
	}
	r.mu.Unlock()
	for {
		r.mu.Lock()
		now := time.Now()
		elapsed := now.Sub(r.last).Seconds()
		ratePerSec := 1.0 / r.Per.Seconds()
		r.tokens = minFloat(r.tokens+elapsed*ratePerSec, float64(r.Burst))
		r.last = now
		if r.tokens >= 1.0 {
			r.tokens -= 1.0
			r.mu.Unlock()
			return nil
		}
		r.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func (r *RateLimitPolicy) Do(action Action, mainErr error) error { return mainErr }

type WorkerPoolPolicy struct{ Size int }

func (w *WorkerPoolPolicy) Do(action Action, mainErr error) error { return mainErr }

func (r *RetryPolicy) SetContext(ctx context.Context)                                { r.ctx = ctx }
func (r *RetryPolicy) SetOnRetry(fn func(actionName string, attempt int, err error)) { r.onRetry = fn }
func (r *RetryPolicy) SetExecutor(fn func() ([]interface{}, error))                  { r.execute = fn }

func (r *RetryPolicy) Do(action Action, mainErr error) error {
	if mainErr == nil {
		return nil
	}
	var dnr DoNotRetry
	if errors.As(mainErr, &dnr) {
		return nil
	}
	// Attempts counts total tries including the original call already made
	if r.Attempts <= 1 {
		return mainErr
	}
	ctx := r.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	start := time.Now()
	delay := r.Delay
	lastErr := mainErr
	for attempt := 2; attempt <= r.Attempts; attempt++ {
		if r.MaxElapsed > 0 && time.Since(start) > r.MaxElapsed {
			break
		}
		if r.Fn != nil {
			if err := r.Fn(); err != nil {
				// pre-check failed: stop retrying, keep the underlying error
				return lastErr
			}
		}
		if r.BackoffFactor > 1 {
			delay = time.Duration(float64(delay) * r.BackoffFactor)
		}
		if r.MaxDelay > 0 && delay > r.MaxDelay {
			delay = r.MaxDelay
		}
		if r.Jitter {
			delay = time.Duration(float64(delay) * (0.5 + rand.Float64()/2))
		}
		var callErr error
		if r.execute != nil {
			_, callErr = r.execute()
		} else {
			// standalone use outside a flow: no executor injected
			_, callErr = callReflect(action.Func, action.Params...)
		}
		if callErr == nil {
			if r.onRetry != nil {
				r.onRetry(functionNameOf(action.Func), attempt, nil)
			}
			return nil
		}
		lastErr = callErr
		if r.onRetry != nil {
			r.onRetry(functionNameOf(action.Func), attempt, callErr)
		}
		if errors.As(callErr, &dnr) {
			return nil
		}
		if r.RetryIf != nil && !r.RetryIf(callErr) {
			break
		}
		if attempt == r.Attempts {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
	}
	return lastErr
}

type DoNotRetry interface {
	DoNotRetry()
}

type permanentError struct{ err error }

func (p permanentError) Error() string { return p.err.Error() }
func (p permanentError) Unwrap() error { return p.err }
func (p permanentError) DoNotRetry()   {}

// Permanent marks err as non-retryable: RetryPolicy stops retrying when it
// sees it (directly or wrapped) and treats the action as handled. Returns nil
// for a nil err. errors.Is and errors.As keep working through the wrapper.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return permanentError{err: err}
}

type MultiError struct{ errs []error }

func (m *MultiError) Append(err error) {
	if err != nil {
		m.errs = append(m.errs, err)
	}
}

func (m MultiError) Error() string {
	if len(m.errs) == 0 {
		return ""
	}
	if len(m.errs) == 1 {
		return m.errs[0].Error()
	}
	b := strings.Builder{}
	b.WriteString("multiple errors: ")
	for i, e := range m.errs {
		if i > 0 {
			b.WriteString("; ")
		}
		b.WriteString(e.Error())
	}
	return b.String()
}

func (m MultiError) Len() int { return len(m.errs) }

// Unwrap supports errors.Is and errors.As over the collected errors.
func (m MultiError) Unwrap() []error { return m.errs }

func functionNameOf(fn interface{}) string {
	fullName := runtime.FuncForPC(reflect.ValueOf(fn).Pointer()).Name()
	elems := strings.Split(fullName, ".")
	short := elems[len(elems)-1]
	return strings.TrimSuffix(short, "-fm")
}

func minFloat(a, b float64) float64 {
	if a < b {
		return a
	} else {
		return b
	}
}

type Hooks struct {
	OnStart   func(actionName string)
	OnSuccess func(actionName string, result []interface{})
	OnError   func(actionName string, err error)
	OnRetry   func(actionName string, attempt int, err error)
}

func (f *flowm) WithContext(ctx context.Context) Flowm {
	if ctx == nil {
		ctx = context.Background()
	}
	f.ctx = ctx
	return f
}

func (f *flowm) WithHooks(h Hooks) Flowm {
	f.hooks = &h
	return f
}

// Fluent builder helpers
func (f *flowm) Do(function interface{}, params ...interface{}) Flowm {
	f.AddAction(NewAction(function, params))
	return f
}

func (f *flowm) With(policy Policy) Flowm {
	if f.lastIndex >= 0 && f.lastIndex < len(f.stubStorage) {
		f.stubStorage[f.lastIndex].Action.Policies = append(f.stubStorage[f.lastIndex].Action.Policies, policy)
	}
	return f
}

func (f *flowm) PipeAll() Flowm { return f.With(&PipeAllPolicy{}) }

func (f *flowm) Pipe(indices ...int) Flowm { return f.With(&PipeIndicesPolicy{Indices: indices}) }

func (f *flowm) Map(mapper func([]interface{}) []interface{}) Flowm {
	return f.With(&PipeMapPolicy{Mapper: mapper})
}

func (f *flowm) ContinueOnError() Flowm { return f.With(&ContinueOnErrorPolicy{Continue: true}) }

// OnError attaches a fallback function to the last added action: it runs when
// the action fails, and if it succeeds the flow continues with its return
// values as the piped outputs.
func (f *flowm) OnError(function interface{}, params ...interface{}) Flowm {
	return f.With(&FallbackPolicy{Func: function, Params: params})
}

// When makes the last added action conditional: it is skipped (transparently
// for piping) unless pred returns true. pred receives the values piped to the
// action, nil when nothing was piped.
func (f *flowm) When(pred func(piped []interface{}) bool) Flowm {
	return f.With(&ConditionPolicy{If: pred})
}

// Compensate registers a rollback function for the last added action; it runs
// (in reverse registration order) when a later action fails terminally.
func (f *flowm) Compensate(function interface{}, params ...interface{}) Flowm {
	return f.With(&CompensatePolicy{Func: function, Params: params})
}

// DoFlow embeds another flow as a single action: the subflow runs with this
// flow's context, and its final results become this step's outputs (pipeable).
func (f *flowm) DoFlow(sub Flowm) Flowm {
	runner := func(ctx context.Context) ([]interface{}, error) {
		sub.WithContext(ctx)
		return sub.StartWithResult()
	}
	f.AddAction(NewAction(runner, nil))
	return f
}

// ForEach runs function once per piped element, concurrently, and merges the
// results in element order for piping onward. A single piped slice is expanded
// into its elements. policies apply per element (e.g. WorkerPoolPolicy, retry).
func (f *flowm) ForEach(function interface{}, policies ...Policy) Flowm {
	f.AddAction(Action{Func: function, Policies: []Policy{&ForEachPolicy{Policies: policies}}})
	return f
}

func (f *flowm) AddParallel(actions ...Action) Flowm {
	par := f.newParallelAction(actions, nil)
	f.AddAction(par)
	return f
}
