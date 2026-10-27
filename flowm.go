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
	Name     string
	Func     interface{}
	Params   []interface{}
	Policies []Policy
	parallel *parallelGroup
	routes   []route
	jump     *jumpTarget
	defined  bool // distinguishes Do(nil), which is invalid, from no decision
}

type stub struct {
	Name   string
	Action Action
}

type Flowm interface {
	AddAction(action Action)
	WithContext(ctx context.Context) Flowm
	WithHooks(h Hooks) Flowm
	WithMaxJumps(limit int) Flowm
	Do(function interface{}, params ...interface{}) Flowm
	Named(name string) Flowm
	With(policy Policy) Flowm
	PipeAll() Flowm
	Pipe(indices ...int) Flowm
	Map(mapper func([]interface{}) []interface{}) Flowm
	When(decide func(Outcome) Action) Flowm
	AddParallel(actions ...Action) Flowm
	WithConcurrency(limit int) Flowm
	Validate() error
	Start() error
	StartWithResult() ([]interface{}, error)
}

type flowm struct {
	stubStorage []stub
	ctx         context.Context
	hooks       *Hooks
	lastIndex   int
	maxJumps    int
	// Execution-only state is populated on a copy for each run.
	targets    map[string]int
	inParallel bool
}

func New() Flowm {
	return &flowm{
		stubStorage: make([]stub, 0),
		ctx:         context.Background(),
		hooks:       &Hooks{},
		lastIndex:   -1,
		maxJumps:    defaultMaxJumps,
	}
}

func (f *flowm) AddAction(act Action) {
	f.stubStorage = append(f.stubStorage, stub{Name: actionName(act), Action: act})
	f.lastIndex = len(f.stubStorage) - 1
}

func (f *flowm) Start() error {
	_, err := f.StartWithResult()
	return err
}

// StartWithResult runs the flow and returns the last executed action's
// non-error return values alongside the flow error.
func (f *flowm) StartWithResult() ([]interface{}, error) {
	targets, err := f.validate()
	if err != nil {
		return nil, err
	}
	run := *f
	run.targets = targets
	var pipedValues []interface{}
	hasPiped := false
	var lastResult []interface{}
	jumps := 0
	for i := 0; i < len(f.stubStorage); {
		st := f.stubStorage[i]
		if err := f.ctx.Err(); err != nil {
			failure := &ActionError{Name: st.Name, Err: err}
			if f.hooks != nil && f.hooks.OnError != nil {
				f.hooks.OnError(st.Name, failure)
			}
			return nil, failure
		}
		if hasPiped {
			if st.Action.parallel == nil {
				params := make([]interface{}, 0, len(st.Action.Params)+len(pipedValues))
				params = append(params, st.Action.Params...)
				st.Action.Params = append(params, pipedValues...)
			}
			hasPiped = false
			pipedValues = nil
		}
		result, pipeNext, pipeVals := run.runSingleAction(st)
		lastResult = result.Values
		if pipeNext {
			pipedValues = pipeVals
			hasPiped = true
		}
		if err := result.failure(); err != nil {
			return nil, err
		}
		if result.jump != nil {
			if err := f.ctx.Err(); err != nil {
				result.Err = err
			} else if jumps >= f.maxJumps {
				result.Err = fmt.Errorf("%w: limit %d, target %q", ErrJumpLimitExceeded, f.maxJumps, result.jump.name)
			}
			if result.Err != nil {
				f.reportOutcome(result.Outcome)
				return nil, result.failure()
			}
			jumps++
			i = targets[result.jump.name]
		} else {
			i++
		}
	}
	return filterErrorValues(lastResult), nil
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
// chaining With, Named, and When. Intended for
// AddParallel and anywhere an Action value is needed.
func Do(function interface{}, params ...interface{}) Action {
	return Action{Func: function, Params: params, defined: true}
}

// NoAction keeps the source outcome unchanged when returned from a When
// callback and allows later callbacks to decide. It preserves both values
// and errors; an error still stops the flow if no later callback handles it.
func NoAction() Action {
	return Action{}
}

// Named returns an action with a name used by errors, hooks, and jump targets.
// An empty name uses the function's name.
func (a Action) Named(name string) Action {
	a.Name = name
	return a
}

// With returns a copy of the action with the policies attached.
func (a Action) With(policies ...Policy) Action {
	combined := make([]Policy, 0, len(a.Policies)+len(policies))
	combined = append(combined, a.Policies...)
	combined = append(combined, policies...)
	a.Policies = combined
	return a
}

// When appends a callback that chooses an action after this action completes.
// Return Do(...) to run a branch, Jump(name) to transfer control, or NoAction()
// to keep the outcome and try the next callback. The first choice wins.
func (a Action) When(decide func(Outcome) Action) Action {
	rules := make([]route, 0, len(a.routes)+1)
	rules = append(rules, a.routes...)
	a.routes = append(rules, decide)
	return a
}

func (f *flowm) callFunc(function interface{}, params ...interface{}) ([]interface{}, error) {
	args, err := prepareArguments(function, params, f.ctx, false)
	if err != nil {
		return nil, err
	}
	return callValues(function, args)
}

func callReflect(function interface{}, params ...interface{}) ([]interface{}, error) {
	args, err := prepareArguments(function, params, nil, false)
	if err != nil {
		return nil, err
	}
	return callValues(function, args)
}

func callValues(function interface{}, args []reflect.Value) ([]interface{}, error) {
	var result []interface{}
	var errs []error
	for _, value := range reflect.ValueOf(function).Call(args) {
		if value.Type().Implements(errorType) {
			if !nilValue(value) {
				errs = append(errs, value.Interface().(error))
			}
			continue
		}
		result = append(result, value.Interface())
	}
	if len(errs) == 1 {
		return result, errs[0]
	}
	return result, errors.Join(errs...)
}

// executeAction completes the primary call and its policies before routing.
func (f *flowm) executeAction(st stub) (result []interface{}, err error, attempts int) {
	defer func() {
		if recovered := recover(); recovered != nil {
			result = nil
			err = fmt.Errorf("panic: %v", recovered)
		}
	}()
	if st.Action.parallel != nil {
		limit, limitErr := parallelLimit(st.Action)
		if limitErr != nil {
			err = limitErr
			return
		}
		members := st.Action.parallel.actions
		st.Action.Func = func(ctx context.Context) ([]interface{}, error) {
			return f.runParallel(ctx, members, limit)
		}
		st.Action.Params = nil
	}
	if _, argErr := prepareArguments(st.Action.Func, st.Action.Params, f.ctx, false); argErr != nil {
		err = argErr
		return
	}
	// Execution callbacks belong to this invocation, not shared configuration.
	st.Action.Policies = append([]Policy(nil), st.Action.Policies...)
	for i, p := range st.Action.Policies {
		switch p := p.(type) {
		case *RetryPolicy:
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
			if f.hooks != nil && f.hooks.OnRetry != nil {
				onRetry = func(_ string, attempt int, err error) {
					f.hooks.OnRetry(st.Name, attempt, err)
				}
			}
			rh.SetOnRetry(onRetry)
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
		attempts++
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
				// Routing sees the final attempt's values even when it fails.
				result = res
				return res, execErr
			})
		}
	}
	if f.hooks != nil && f.hooks.OnStart != nil {
		f.hooks.OnStart(st.Name)
	}
	result, err = executeOnce()
	for _, policy := range st.Action.Policies {
		err = policy.Do(st.Action, err)
	}
	return
}

// Piping on the originating flow step applies to the selected branch's values.
func (f *flowm) runSingleAction(st stub) (result actionResult, pipeNext bool, pipeVals []interface{}) {
	result = f.runAction(st)
	defer func() {
		if recovered := recover(); recovered != nil {
			result = actionResult{Outcome: Outcome{Name: st.Name, Err: fmt.Errorf("pipe panic: %v", recovered)}}
			pipeNext, pipeVals = false, nil
			f.reportOutcome(result.Outcome)
		}
	}()
	if result.Err == nil && len(result.Values) > 0 {
		// prepare values for piping: drop error-typed returns and expand nested []interface{}
		filtered := filterErrorValues(result.Values)
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

// WorkerPoolPolicy sets a parallel group limit. Prefer Flowm.WithConcurrency.
// Placement on a member is deprecated; a group limit takes precedence.
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
	OnSkip    func(actionName string)
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

// When appends a decision callback to the last added action. Callbacks run
// after its attempts and policies. Return Do(...), Jump(name), or NoAction()
// to keep the original outcome. The first non-empty decision wins.
func (f *flowm) When(decide func(Outcome) Action) Flowm {
	if f.lastIndex >= 0 {
		st := &f.stubStorage[f.lastIndex]
		st.Action = st.Action.When(decide)
	}
	return f
}

func (f *flowm) AddParallel(actions ...Action) Flowm {
	par := Action{Name: "parallel", parallel: &parallelGroup{actions: append([]Action(nil), actions...)}}
	f.AddAction(par)
	return f
}

// Named sets the last added action's name for errors, hooks, and jump targets.
func (f *flowm) Named(name string) Flowm {
	if f.lastIndex >= 0 {
		st := &f.stubStorage[f.lastIndex]
		st.Action.Name = name
		st.Name = actionName(st.Action)
	}
	return f
}

// WithConcurrency limits the last added parallel group to limit active members.
// The limit must be positive. Validate reports use on a non-parallel action.
func (f *flowm) WithConcurrency(limit int) Flowm {
	return f.With(&WorkerPoolPolicy{Size: limit})
}

func actionName(action Action) string {
	if action.Name != "" {
		return action.Name
	}
	if action.parallel != nil {
		return "parallel"
	}
	if action.jump != nil {
		return "jump:" + action.jump.name
	}
	return functionNameOf(action.Func)
}
