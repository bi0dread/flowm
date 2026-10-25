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
	AddParallel(actions ...Action) Flowm
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
	f.stubStorage = append(f.stubStorage, stub{Name: functionNameOf(act.Func), Action: act})
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
	for i := range f.stubStorage {
		st := f.stubStorage[i]
		if condition := conditionPolicyOf(st.Action); condition != nil && condition.If != nil && !condition.If(pipedValues) {
			// A skipped action leaves the incoming values for the next action.
			continue
		}
		if hasPiped {
			params := make([]interface{}, 0, len(st.Action.Params)+len(pipedValues))
			params = append(params, st.Action.Params...)
			st.Action.Params = append(params, pipedValues...)
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
			return nil, err
		}
	}
	return filterErrorValues(lastResult), nil
}

func conditionPolicyOf(action Action) *ConditionPolicy {
	for _, policy := range action.Policies {
		if condition, ok := policy.(*ConditionPolicy); ok {
			return condition
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
	// Execution callbacks belong to this invocation, not shared configuration.
	st.Action.Policies = append([]Policy(nil), st.Action.Policies...)
	for i, p := range st.Action.Policies {
		switch p := p.(type) {
		case *RetryPolicy:
			local := *p
			st.Action.Policies[i] = &local
		case *FallbackPolicy:
			local := *p
			st.Action.Policies[i] = &local
		}
	}
	var perAttempt, overall time.Duration
	for _, p := range st.Action.Policies {
		if t, ok := p.(*TimeoutPolicy); ok {
			perAttempt, overall = t.PerAttempt, t.Overall
			break
		}
	}
	ctx := f.ctx
	if overall > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, overall)
		defer cancel()
	}
	// propagate context to policies if supported
	for _, p := range st.Action.Policies {
		if cp, ok := p.(ContextAwarePolicy); ok {
			cp.SetContext(ctx)
		}
		if rh, ok := p.(RetryHookAwarePolicy); ok {
			var onRetry func(string, int, error)
			if f.hooks != nil {
				onRetry = f.hooks.OnRetry
			}
			rh.SetOnRetry(onRetry)
		}
		if fa, ok := p.(FallbackAwarePolicy); ok {
			fa.SetFlowCaller(f.callFunc)
			fa.SetResultSink(func(res []interface{}) { result = res })
		}
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
		attemptCtx := ctx
		if perAttempt > 0 {
			var cancel context.CancelFunc
			attemptCtx, cancel = context.WithTimeout(ctx, perAttempt)
			defer cancel()
		}
		if err := attemptCtx.Err(); err != nil {
			return nil, err
		}
		for _, p := range st.Action.Policies {
			if pre, ok := p.(PreExecutionPolicy); ok {
				if perr := pre.Before(attemptCtx, st.Action); perr != nil {
					return nil, perr
				}
			}
		}
		if err := attemptCtx.Err(); err != nil {
			return nil, err
		}
		executed = true
		attemptFlow := *f
		attemptFlow.ctx = attemptCtx
		if attemptCtx.Done() != nil {
			return attemptFlow.callWithContext(st.Action)
		}
		return attemptFlow.callFunc(st.Action.Func, st.Action.Params...)
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
			case *PipeAllPolicy:
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

func (f *flowm) callWithContext(a Action) ([]interface{}, error) {
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
	}
}

func (f *flowm) newParallelAction(actions []Action) Action {
	runner := func(ctx context.Context) ([]interface{}, error) {
		group := *f
		group.ctx = ctx
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
			wg.Add(1)
			go func() {
				defer wg.Done()
				if sem != nil {
					select {
					case sem <- struct{}{}:
					case <-ctx.Done():
						out[i].err = ctx.Err()
						return
					}
					defer func() { <-sem }()
				}
				st := stub{Name: functionNameOf(actions[i].Func), Action: actions[i]}
				rres, rerr, _, _, continueOnErr := group.runSingleAction(st)
				if continueOnErr {
					rerr = nil
				}
				out[i] = r{res: rres, err: rerr}
			}()
		}
		wg.Wait()
		var errs []error
		var results []interface{}
		for i := range out {
			if out[i].err != nil {
				errs = append(errs, out[i].err)
			}
			results = append(results, filterErrorValues(out[i].res)...)
		}
		return results, errors.Join(errs...)
	}
	return Do(runner)
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
	RetryIf       func(error) bool
	ctx           context.Context
	onRetry       func(actionName string, attempt int, err error)
	execute       func() ([]interface{}, error)
}

type ContinueOnErrorPolicy struct{ Continue bool }

func (c *ContinueOnErrorPolicy) Do(action Action, mainErr error) error { return mainErr }

// ConditionPolicy gates a top-level flow action using its incoming piped values.
// A false predicate skips the action without consuming those values.
// A nil predicate allows the action to run. Sugar: Flowm.When.
type ConditionPolicy struct {
	If func(piped []interface{}) bool
}

func (c *ConditionPolicy) Do(action Action, mainErr error) error { return mainErr }

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
		r.tokens = min(r.tokens+elapsed*ratePerSec, float64(r.Burst))
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
	if mainErr == nil || r.Attempts <= 1 {
		return mainErr
	}
	ctx := r.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	delay := r.Delay
	lastErr := mainErr
	for attempt := 2; attempt <= r.Attempts; attempt++ {
		var dnr DoNotRetry
		if errors.As(lastErr, &dnr) || (r.RetryIf != nil && !r.RetryIf(lastErr)) {
			return lastErr
		}
		wait := delay
		if r.Jitter {
			wait = retryDelay(wait, 0.5+rand.Float64())
		}
		if r.MaxDelay > 0 && wait > r.MaxDelay {
			wait = r.MaxDelay
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		var callErr error
		if r.execute != nil {
			_, callErr = r.execute()
		} else {
			_, callErr = callReflect(action.Func, action.Params...)
		}
		if r.onRetry != nil {
			r.onRetry(functionNameOf(action.Func), attempt, callErr)
		}
		if callErr == nil {
			return nil
		}
		lastErr = callErr
		if r.BackoffFactor > 1 {
			delay = retryDelay(delay, r.BackoffFactor)
		}
	}
	return lastErr
}

// Saturate backoff and jitter instead of overflowing into an immediate retry.
func retryDelay(delay time.Duration, factor float64) time.Duration {
	const maxDuration = time.Duration(1<<63 - 1)
	next := float64(delay) * factor
	if next >= float64(maxDuration) {
		return maxDuration
	}
	return time.Duration(next)
}

type DoNotRetry interface {
	DoNotRetry()
}

type permanentError struct{ err error }

func (p permanentError) Error() string { return p.err.Error() }
func (p permanentError) Unwrap() error { return p.err }
func (p permanentError) DoNotRetry()   {}

// Permanent marks err as non-retryable: RetryPolicy stops retrying when it
// sees it (directly or wrapped) and preserves it for the caller or fallback.
// Returns nil for a nil err. errors.Is and errors.As work through the wrapper.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return permanentError{err: err}
}

func functionNameOf(fn interface{}) string {
	value := reflect.ValueOf(fn)
	if !value.IsValid() || value.Kind() != reflect.Func || value.IsNil() {
		return fmt.Sprintf("%T", fn)
	}
	fullName := runtime.FuncForPC(value.Pointer()).Name()
	elems := strings.Split(fullName, ".")
	short := elems[len(elems)-1]
	return strings.TrimSuffix(short, "-fm")
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
	f.AddAction(Do(function, params...))
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

// When runs the last added action only if pred accepts its incoming piped values.
// The predicate receives nil when nothing was piped. Skipping the action leaves
// the values available for the next action.
func (f *flowm) When(pred func(piped []interface{}) bool) Flowm {
	return f.With(&ConditionPolicy{If: pred})
}

func (f *flowm) AddParallel(actions ...Action) Flowm {
	par := f.newParallelAction(actions)
	f.AddAction(par)
	return f
}
