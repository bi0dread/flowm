package flowm

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// mock function for successful execution
func mockSuccess(s string) string {
	return "Hello " + s
}

// mock function for error
func mockError() error {
	return errors.New("mock error")
}

// mock function with retry counter
type retryCounter struct {
	count int
}

func doFail() error {

	fmt.Println("do fail")
	return errors.New("this is test error")
}

func (r *retryCounter) failUntil(threshold int) error {
	fmt.Println("failUntil")
	r.count++
	if r.count < threshold {
		return errors.New("still failing")
	}
	return nil
}

// mock error type implementing DoNotRetry
type noRetryError struct{}

func (noRetryError) Error() string { return "no retry error" }
func (noRetryError) DoNotRetry()   {}
func newNoRetryError() error       { return noRetryError{} }

// custom policy for testing
type logPolicy struct {
	logs *[]string
}

func (l logPolicy) Do(action Action, err error) error {
	*l.logs = append(*l.logs, fmt.Sprintf("Policy executed for %T with err: %v", action.Func, err))
	return err
}

var _ Policy = logPolicy{}

func TestFlowm_BasicExecution(t *testing.T) {
	f := New()

	f.AddAction(NewAction(mockSuccess, []interface{}{"World"}))
	f.AddAction(NewAction(mockError, []interface{}{}))

	err := f.Start()
	if err == nil {
		t.Error("Expected error but got nil")
	}
}

func TestFlowm_RetryPolicy_SuccessAfterRetries(t *testing.T) {
	f := New()
	rc := &retryCounter{}

	retry := &RetryPolicy{
		Attempts:      3,
		Delay:         1000 * time.Millisecond,
		BackoffFactor: 1.0,
		Jitter:        false,
	}

	f.AddAction(NewAction(rc.failUntil, []interface{}{3}, retry))

	err := f.Start()
	if err != nil {
		t.Errorf("Expected success after retries, got error: %v", err)
	}
	if rc.count != 3 {
		t.Errorf("Expected 3 attempts, got %d", rc.count)
	}
}

func TestFlowm_RetryPolicy_Failure(t *testing.T) {
	f := New()
	rc := &retryCounter{}

	retry := &RetryPolicy{
		Attempts: 2,
		Delay:    5 * time.Millisecond,
	}

	f.AddAction(NewAction(rc.failUntil, []interface{}{5}, retry))

	err := f.Start()
	if err == nil {
		t.Error("Expected error after retries but got nil")
	}
	if rc.count != 2 {
		t.Errorf("Expected 2 attempts, got %d", rc.count)
	}
}

func TestFlowm_TerminatePolicy_Skip(t *testing.T) {
	f := New()

	retry := &RetryPolicy{
		Attempts: 2,
		Delay:    5 * time.Millisecond,
	}

	terminate := &TerminatePolicy{Terminate: true}

	f.AddAction(NewAction(doFail, nil, retry, terminate))

	err := f.Start()
	if err == nil {
		fmt.Println("terminate policy should skip execution")
	} else {
		t.Errorf("Expected error due to termination, got: %v", err)
	}

}

func TestFlowm_PassValuePolicy_Skip(t *testing.T) {
	f := New()

	pass := &PassValuesPolicy{}

	f.AddAction(NewAction(FunctionWithReturnValue, nil, pass))
	f.AddAction(NewAction(FunctionWithParams, nil))

	err := f.Start()
	if err == nil {
		fmt.Println("terminate policy should skip execution")
	} else {
		t.Errorf("Expected error due to termination, got: %v", err)
	}

}

func FunctionWithReturnValue() string {
	return "Hello World"
}

func FunctionWithParams(value string) {
	fmt.Println("i got :" + value)
}

func TestFlowm_DoNotRetry(t *testing.T) {
	f := New()

	retry := &RetryPolicy{
		Attempts: 3,
		Delay:    5 * time.Millisecond,
	}

	// function returning a DoNotRetry error
	fn := func() error {
		return newNoRetryError()
	}

	f.AddAction(NewAction(fn, []interface{}{}, retry))
	err := f.Start()
	if err != nil {
		// RetryPolicy.Do() ignores DoNotRetry errors and returns nil
		t.Errorf("Expected nil error due to DoNotRetry, got %v", err)
	}
}

func TestFlowm_TimeoutPolicy_PerAttempt(t *testing.T) {
	f := New().WithContext(context.Background())
	slow := func() error {
		time.Sleep(20 * time.Millisecond)
		return nil
	}
	timeout := &TimeoutPolicy{PerAttempt: 5 * time.Millisecond}
	f.AddAction(NewAction(slow, nil, timeout))
	err := f.Start()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline exceeded, got %v", err)
	}
}

func TestFlowm_PipeAllAndMap(t *testing.T) {
	f := New()
	produce := func() (int, string) { return 2, "a" }
	var got []string
	consume := func(dst *[]string, i int, s string) { *dst = append(*dst, fmt.Sprintf("%d-%s", i, s)) }
	f.Do(produce).PipeAll()
	f.AddAction(NewAction(consume, []interface{}{&got}))
	if err := f.Start(); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "2-a" {
		t.Fatalf("unexpected piped result: %+v", got)
	}
	// Map example: transform outputs into a single string param
	got = nil
	f = New()
	mapper := func(vals []interface{}) []interface{} { return []interface{}{fmt.Sprintf("%v-%v", vals[0], vals[1])} }
	consumeStr := func(dst *[]string, s string) { *dst = append(*dst, s) }
	f.Do(produce).Map(mapper)
	f.AddAction(NewAction(consumeStr, []interface{}{&got}))
	if err := f.Start(); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "2-a" {
		t.Fatalf("unexpected mapped result: %+v", got)
	}
}

func TestFlowm_ParallelGroup_PipeAll(t *testing.T) {
	f := New()
	f1 := func() string { time.Sleep(5 * time.Millisecond); return "A" }
	f2 := func() string { time.Sleep(5 * time.Millisecond); return "B" }
	var out []string
	collect := func(dst *[]string, a string, b string) {
		*dst = append(*dst, a, b)
		fmt.Println(a, b)
	}
	f.AddParallel(NewAction(f1, nil), NewAction(f2, nil)).PipeAll()
	f.AddAction(NewAction(collect, []interface{}{&out}))
	if err := f.Start(); err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 {
		t.Fatalf("expected 2 outputs, got %v", out)
	}
	if !(out[0] == "A" && out[1] == "B" || out[0] == "B" && out[1] == "A") {
		t.Fatalf("unexpected outputs: %v", out)
	}
}

func TestFlowm_RateLimitPolicy_Waits(t *testing.T) {
	rl := &RateLimitPolicy{Per: 1 * time.Second, Burst: 1}
	times := []time.Time{}
	record := func(dst *[]time.Time) {
		fmt.Println("record_called")
	}
	f := New()
	f.AddAction(NewAction(record, []interface{}{&times}, rl))
	f.AddAction(NewAction(record, []interface{}{&times}, rl))
	if err := f.Start(); err != nil {
		t.Fatal(err)
	}

}

func TestFlowm_CircuitBreaker_Open(t *testing.T) {
	cb := &CircuitBreakerPolicy{Failures: 2, Window: 1 * time.Second, Cooldown: 1 * time.Second}
	bad := func() error {
		fmt.Println("bad_called")
		return errors.New("fail")
	}
	f := New()
	f.AddAction(NewAction(bad, nil, cb, &ContinueOnErrorPolicy{Continue: true}))
	f.AddAction(NewAction(bad, nil, cb, &ContinueOnErrorPolicy{Continue: true}))
	f.AddAction(NewAction(bad, nil, cb))
	err := f.Start()
	if err == nil {
		t.Fatal("expected error from circuit breaker")
	}
	if err.Error() != "circuit breaker open" {
		t.Fatalf("expected breaker open, got %v", err)
	}
}

func TestFlowm_RetryPolicy_FnPreCheckFailureReturnsError(t *testing.T) {
	f := New()
	calls := 0
	bad := func() error { calls++; return errors.New("boom") }
	retry := &RetryPolicy{
		Attempts: 3,
		Delay:    time.Millisecond,
		Fn:       func() error { return errors.New("pre-check failed") },
	}
	f.AddAction(NewAction(bad, nil, retry))
	err := f.Start()
	if err == nil {
		t.Fatal("expected error when retry pre-check fails, got nil")
	}
	if calls != 1 {
		t.Fatalf("expected no retries after failed pre-check, got %d calls", calls)
	}
}

func TestFlowm_RetryPolicy_ContextInjectionOnRetry(t *testing.T) {
	f := New().WithContext(context.Background())
	calls := 0
	fn := func(ctx context.Context) error {
		calls++
		if calls < 3 {
			return errors.New("not yet")
		}
		return nil
	}
	retry := &RetryPolicy{Attempts: 3, Delay: time.Millisecond}
	f.AddAction(NewAction(fn, nil, retry))
	if err := f.Start(); err != nil {
		t.Fatalf("expected success after retries with context injection, got %v", err)
	}
	if calls != 3 {
		t.Fatalf("expected 3 calls, got %d", calls)
	}
}

func TestFlowm_RetryPolicy_TimeoutAppliesToEachAttempt(t *testing.T) {
	f := New().WithContext(context.Background())
	var calls atomic.Int32
	fn := func() error {
		if calls.Add(1) == 1 {
			time.Sleep(100 * time.Millisecond) // first attempt exceeds the per-attempt timeout
		}
		return nil
	}
	retry := &RetryPolicy{Attempts: 2, Delay: time.Millisecond}
	timeout := &TimeoutPolicy{PerAttempt: 20 * time.Millisecond}
	f.AddAction(NewAction(fn, nil, timeout, retry))
	if err := f.Start(); err != nil {
		t.Fatalf("expected retry after timeout to succeed, got %v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("expected 2 attempts, got %d", calls.Load())
	}
}

func TestFlowm_RetryPolicy_RespectsOpenCircuitBreaker(t *testing.T) {
	cb := &CircuitBreakerPolicy{Failures: 1, Window: time.Second, Cooldown: time.Second}
	calls := 0
	bad := func() error { calls++; return errors.New("boom") }
	retry := &RetryPolicy{Attempts: 3, Delay: time.Millisecond}
	f := New()
	f.AddAction(NewAction(bad, nil, cb, retry))
	err := f.Start()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if calls != 1 {
		t.Fatalf("expected retries to be blocked by open breaker, got %d calls", calls)
	}
	if err.Error() != "circuit breaker open" {
		t.Fatalf("expected breaker-open error from retries, got %v", err)
	}
}

func TestFlowm_RetryPolicy_RecoversAndRetriesPanic(t *testing.T) {
	f := New()
	calls := 0
	fn := func() error {
		calls++
		if calls < 2 {
			panic("transient panic")
		}
		return nil
	}
	retry := &RetryPolicy{Attempts: 3, Delay: time.Millisecond}
	f.AddAction(NewAction(fn, nil, retry))
	if err := f.Start(); err != nil {
		t.Fatalf("expected panic to be recovered and retried, got %v", err)
	}
	if calls != 2 {
		t.Fatalf("expected 2 attempts, got %d", calls)
	}
}

func TestFlowm_RetryPolicy_PipesResultFromSuccessfulRetry(t *testing.T) {
	f := New()
	calls := 0
	produce := func() (string, error) {
		calls++
		if calls < 2 {
			return "", errors.New("not yet")
		}
		return "value-from-retry", nil
	}
	var got string
	consume := func(dst *string, s string) { *dst = s }
	retry := &RetryPolicy{Attempts: 2, Delay: time.Millisecond}
	f.Do(produce).With(retry).PipeAll()
	f.AddAction(NewAction(consume, []interface{}{&got}))
	if err := f.Start(); err != nil {
		t.Fatal(err)
	}
	if got != "value-from-retry" {
		t.Fatalf("expected piped value from successful retry, got %q", got)
	}
}

func TestFlowm_OnError_RecoversAndContinues(t *testing.T) {
	f := New()
	var recovered, ran bool
	primary := func() error { return errors.New("primary down") }
	fallback := func(err error) { recovered = err.Error() == "primary down" }
	f.Do(primary).OnError(fallback)
	f.Do(func() { ran = true })
	if err := f.Start(); err != nil {
		t.Fatalf("expected fallback to recover the flow, got %v", err)
	}
	if !recovered {
		t.Fatal("expected fallback to receive the primary error")
	}
	if !ran {
		t.Fatal("expected next action to run after recovery")
	}
}

func TestFlowm_OnError_ContextAndErrorInjection(t *testing.T) {
	type ctxKey struct{}
	ctx := context.WithValue(context.Background(), ctxKey{}, "ctx-ok")
	f := New().WithContext(ctx)
	var gotCtx, gotErr string
	fallback := func(c context.Context, err error) {
		gotCtx, _ = c.Value(ctxKey{}).(string)
		gotErr = err.Error()
	}
	f.Do(func() error { return errors.New("boom") }).OnError(fallback)
	if err := f.Start(); err != nil {
		t.Fatal(err)
	}
	if gotCtx != "ctx-ok" {
		t.Fatalf("expected context injection into fallback, got %q", gotCtx)
	}
	if gotErr != "boom" {
		t.Fatalf("expected error injection into fallback, got %q", gotErr)
	}
}

func TestFlowm_OnError_FallbackResultsArePiped(t *testing.T) {
	f := New()
	primary := func() (string, error) { return "", errors.New("no data") }
	fallback := func(err error) (string, error) { return "cached-value", nil }
	var got string
	consume := func(dst *string, s string) { *dst = s }
	f.Do(primary).OnError(fallback).PipeAll()
	f.AddAction(NewAction(consume, []interface{}{&got}))
	if err := f.Start(); err != nil {
		t.Fatal(err)
	}
	if got != "cached-value" {
		t.Fatalf("expected fallback value to be piped, got %q", got)
	}
}

func TestFlowm_OnError_ChainedFallbacks(t *testing.T) {
	f := New()
	primary := func() error { return errors.New("primary down") }
	fb1 := func(err error) error { return errors.New("fallback1 down") }
	var got string
	fb2 := func(err error) { got = err.Error() }
	f.Do(primary).OnError(fb1).OnError(fb2)
	if err := f.Start(); err != nil {
		t.Fatal(err)
	}
	if got != "fallback1 down" {
		t.Fatalf("expected second fallback to receive first fallback's error, got %q", got)
	}
}

func TestFlowm_OnError_AfterRetryExhausted(t *testing.T) {
	calls := 0
	primary := func() error { calls++; return errors.New("down") }
	var recovered bool
	retry := &RetryPolicy{Attempts: 3, Delay: time.Millisecond}
	f := New()
	f.Do(primary).With(retry).OnError(func(err error) { recovered = true })
	if err := f.Start(); err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Fatalf("expected fallback only after 3 attempts, got %d", calls)
	}
	if !recovered {
		t.Fatal("expected fallback to run after retries were exhausted")
	}
}

func TestFlowm_OnError_FallbackFailurePropagates(t *testing.T) {
	f := New()
	f.Do(func() error { return errors.New("primary down") }).
		OnError(func(err error) error { return errors.New("fallback down") })
	err := f.Start()
	if err == nil || err.Error() != "fallback down" {
		t.Fatalf("expected fallback error, got %v", err)
	}
}

func TestFlowm_OnError_FallbackPanicBecomesError(t *testing.T) {
	f := New()
	f.Do(func() error { return errors.New("x") }).
		OnError(func(err error) { panic("fallback exploded") })
	err := f.Start()
	if err == nil || !strings.Contains(err.Error(), "fallback exploded") {
		t.Fatalf("expected recovered panic error, got %v", err)
	}
}

func TestFlowm_DoStyleAction_InParallel(t *testing.T) {
	f := New()
	fa := func() string { return "A" }
	fb := func(suffix string) string { return "B" + suffix }
	var got []string
	collect := func(dst *[]string, a, b string) { *dst = append(*dst, a, b) }
	f.AddParallel(
		Do(fa),
		Do(fb, "!"),
	).PipeAll()
	f.AddAction(NewAction(collect, []interface{}{&got}))
	if err := f.Start(); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "A" || got[1] != "B!" {
		t.Fatalf("unexpected parallel results: %v", got)
	}
}

func TestFlowm_DoStyleAction_PoliciesAndFallback(t *testing.T) {
	f := New()
	good := func() string { return "A" }
	bad := func() (string, error) { return "", errors.New("b down") }
	fallback := func(err error) (string, error) { return "B-fallback", nil }
	rc := &retryCounter{}
	retry := &RetryPolicy{Attempts: 3, Delay: time.Millisecond}
	var got []string
	collect := func(dst *[]string, a, b string) { *dst = append(*dst, a, b) }
	f.AddParallel(
		Do(good),
		Do(bad).OnError(fallback),
	).PipeAll()
	f.AddAction(NewAction(collect, []interface{}{&got}))
	f.Do(rc.failUntil, 2).With(retry) // chained policy on the flow builder still works
	if err := f.Start(); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "A" || got[1] != "B-fallback" {
		t.Fatalf("expected fallback result inside parallel group, got %v", got)
	}
	if rc.count != 2 {
		t.Fatalf("expected retry policy on Do-style chain to run, got %d attempts", rc.count)
	}
}

func TestFlowm_DoStyleAction_WithDoesNotShareBackingArray(t *testing.T) {
	base := Do(func() {}).With(&ContinueOnErrorPolicy{Continue: true})
	a := base.With(&PipeAllPolicy{})
	b := base.With(&TimeoutPolicy{PerAttempt: time.Second})
	if len(a.Policies) != 2 || len(b.Policies) != 2 {
		t.Fatalf("expected independent policy lists, got %d and %d", len(a.Policies), len(b.Policies))
	}
	if _, ok := a.Policies[1].(*PipeAllPolicy); !ok {
		t.Fatalf("branch a lost its policy: %T", a.Policies[1])
	}
	if _, ok := b.Policies[1].(*TimeoutPolicy); !ok {
		t.Fatalf("branch b lost its policy: %T", b.Policies[1])
	}
}

func TestFlowm_ContinueOnError_Continues(t *testing.T) {
	f := New()
	var ran bool
	bad := func() error { return errors.New("x") }
	good := func() { ran = true }
	f.AddAction(NewAction(bad, nil, &ContinueOnErrorPolicy{Continue: true}))
	f.AddAction(NewAction(good, nil))
	if err := f.Start(); err != nil {
		t.Fatal(err)
	}
	if !ran {
		t.Fatal("expected to continue after error")
	}
}
