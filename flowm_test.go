package flowm

import (
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
	r.count++
	if r.count < threshold {
		return errors.New("still failing")
	}
	return nil
}

// mock error type implementing DoNotRetry
type noRetryError struct{}

func (noRetryError) Error() string          { return "no retry error" }
func (noRetryError) DoNotRetry()            {}
func newNoRetryError() error                { return noRetryError{} }
func newNoRetryErrorPointer() *noRetryError { return &noRetryError{} }

// custom policy for testing
type logPolicy struct {
	logs *[]string
}

func (l logPolicy) Do(action Action, err error) error {
	*l.logs = append(*l.logs, fmt.Sprintf("Policy executed for %T with err: %v", action.Func, err))
	return err
}

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
		Delay:         10 * time.Millisecond,
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

func TestFlowm_CustomPolicy(t *testing.T) {
	f := New()
	logs := []string{}

	lp := logPolicy{logs: &logs}

	f.AddAction(NewAction(mockError, []interface{}{}, lp))
	_ = f.Start()

	if len(logs) == 0 {
		t.Error("Expected custom policy to log execution, but got none")
	}
	if logs[0] == "" {
		t.Error("Expected a non-empty log message from policy")
	}
}
