package flowm

import (
	"context"
	"errors"
	"fmt"
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
