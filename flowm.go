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
	AddParallel(actions ...Action) Flowm
	Start() error
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
	var pipedValues []interface{}
	hasPiped := false
	for i := range f.stubStorage {
		st := f.stubStorage[i]
		if hasPiped {
			if st.Action.Params == nil {
				st.Action.Params = []interface{}{}
			}
			st.Action.Params = append(st.Action.Params, pipedValues...)
			hasPiped = false
			pipedValues = nil
		}
		result, err, pipeNext, pipeVals, continueOnErr := f.runSingleAction(st)
		_ = result
		if pipeNext {
			pipedValues = pipeVals
			hasPiped = true
		}
		if err != nil {
			if continueOnErr {
				// swallow error and continue
				continue
			}
			return err
		}
	}
	return nil
}

func NewAction(function interface{}, Params []interface{}, Policy ...Policy) Action {
	return Action{
		Func:     function,
		Params:   Params,
		Policies: Policy,
	}
}

func (f *flowm) call(funcName int, params ...interface{}) (result []interface{}, err error) {
	return f.callFunc(f.stubStorage[funcName].Action.Func, params...)
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
	}
	// pre-execution checks
	for _, p := range st.Action.Policies {
		if pre, ok := p.(PreExecutionPolicy); ok {
			if perr := pre.Before(f.ctx, st.Action); perr != nil {
				err = perr
				break
			}
		}
	}
	if f.hooks != nil && f.hooks.OnStart != nil {
		f.hooks.OnStart(st.Name)
	}
	if err == nil {
		var timeout time.Duration
		var hasTimeout bool
		for _, p := range st.Action.Policies {
			if t, ok := p.(*TimeoutPolicy); ok && t.PerAttempt > 0 {
				timeout = t.PerAttempt
				hasTimeout = true
				break
			}
		}
		if hasTimeout {
			result, err = f.callWithTimeout(st.Action, timeout)
		} else {
			defer func() {
				if r := recover(); r != nil {
					err = fmt.Errorf("panic: %v", r)
				}
			}()
			result, err = f.callFunc(st.Action.Func, st.Action.Params...)
		}
	}
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
		filtered := make([]interface{}, 0, len(result))
		for _, v := range result {
			if _, isErr := v.(error); isErr {
				continue
			}
			filtered = append(filtered, v)
		}
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
}

type PassValuesPolicy struct {
	Value interface{}
}

func (r *PassValuesPolicy) Do(action Action, mainErr error) error {
	return mainErr
}

type TerminatePolicy struct {
	Terminate bool
}

func (r *TerminatePolicy) Do(action Action, mainErr error) error {
	return mainErr
}

type ContinueOnErrorPolicy struct{ Continue bool }

func (c *ContinueOnErrorPolicy) Do(action Action, mainErr error) error { return mainErr }

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

func (c *CircuitBreakerPolicy) Do(action Action, mainErr error) error {
	if mainErr == nil {
		return nil
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
	return mainErr
}

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

func (r *RetryPolicy) Do(action Action, mainErr error) error {
	if mainErr == nil {
		return nil
	}
	if _, ok := mainErr.(DoNotRetry); ok {
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
				return nil
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
		_, callErr := callReflect(action.Func, action.Params...)
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

func (f *flowm) AddParallel(actions ...Action) Flowm {
	par := f.newParallelAction(actions, nil)
	f.AddAction(par)
	return f
}
