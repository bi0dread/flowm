package flowm

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync/atomic"
	"testing"
	"time"
)

func TestFlowm_PermanentError_ReachesFallback(t *testing.T) {
	for _, permanentAt := range []int{1, 2} {
		t.Run(fmt.Sprint(permanentAt), func(t *testing.T) {
			failure := errors.New("permanent failure")
			calls := 0
			f := New()
			f.Do(func() error {
				calls++
				if calls == permanentAt {
					return fmt.Errorf("wrapped: %w", Permanent(failure))
				}
				return errors.New("transient")
			}).With(&RetryPolicy{Attempts: 5}).OnError(func(err error) (string, error) {
				if !errors.Is(err, failure) {
					return "", fmt.Errorf("unexpected fallback error: %w", err)
				}
				return "recovered", nil
			})
			result, err := f.StartWithResult()
			if err != nil || !reflect.DeepEqual(result, []interface{}{"recovered"}) || calls != permanentAt {
				t.Fatalf("result=%v error=%v calls=%d", result, err, calls)
			}
		})
	}
}

func TestFlowm_RetryPolicy_RetryIfRejectsLaterError(t *testing.T) {
	transient, terminal := errors.New("transient"), errors.New("terminal")
	calls := 0
	f := New()
	f.Do(func() error {
		calls++
		if calls == 1 {
			return transient
		}
		return terminal
	}).With(&RetryPolicy{Attempts: 5, RetryIf: func(err error) bool { return errors.Is(err, transient) }})
	if err := f.Start(); !errors.Is(err, terminal) || calls != 2 {
		t.Fatalf("error=%v calls=%d; expected terminal error after two calls", err, calls)
	}
}

func TestFlowm_RetryPolicy_DelayBeforeEveryRetry(t *testing.T) {
	var attempts []time.Time
	f := New()
	f.Do(func() error {
		attempts = append(attempts, time.Now())
		if len(attempts) < 3 {
			return errors.New("retry")
		}
		return nil
	}).With(&RetryPolicy{Attempts: 3, Delay: 5 * time.Millisecond, BackoffFactor: 2})
	if err := f.Start(); err != nil {
		t.Fatal(err)
	}
	for i, minimum := range []time.Duration{5 * time.Millisecond, 10 * time.Millisecond} {
		if elapsed := attempts[i+1].Sub(attempts[i]); elapsed < minimum {
			t.Fatalf("retry %d waited %v, expected at least %v", i+1, elapsed, minimum)
		}
	}
}

func TestFlowm_CanceledContext_DoesNotExecute(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var calls atomic.Int32
	f := New().WithContext(ctx)
	f.Do(func() { calls.Add(1) }).With(&RetryPolicy{Attempts: 3})
	if err := f.Start(); !errors.Is(err, context.Canceled) || calls.Load() != 0 {
		t.Fatalf("error=%v calls=%d; canceled flow must not execute", err, calls.Load())
	}
}

func TestFlowm_TimeoutPolicy_OverallBoundsRetryDelay(t *testing.T) {
	var calls atomic.Int32
	f := New()
	f.Do(func() error {
		calls.Add(1)
		return errors.New("retry")
	}).With(&RetryPolicy{Attempts: 5, Delay: time.Second}).With(&TimeoutPolicy{Overall: 15 * time.Millisecond})
	start := time.Now()
	err := f.Start()
	if !errors.Is(err, context.DeadlineExceeded) || calls.Load() != 1 {
		t.Fatalf("error=%v calls=%d; expected deadline during first retry delay", err, calls.Load())
	}
	if elapsed := time.Since(start); elapsed >= 500*time.Millisecond {
		t.Fatalf("retry delay exceeded overall budget: %v", elapsed)
	}
}

func TestFlowm_TimeoutPolicy_BoundsRateLimitWait(t *testing.T) {
	for _, timeout := range []TimeoutPolicy{{PerAttempt: 15 * time.Millisecond}, {Overall: 15 * time.Millisecond}} {
		t.Run(fmt.Sprintf("%+v", timeout), func(t *testing.T) {
			limiter := &RateLimitPolicy{Per: time.Second, Burst: 1}
			if err := limiter.Before(context.Background(), Action{}); err != nil {
				t.Fatal(err)
			}
			var calls atomic.Int32
			f := New()
			f.Do(func() { calls.Add(1) }).With(limiter).With(&timeout)
			start := time.Now()
			err := f.Start()
			if !errors.Is(err, context.DeadlineExceeded) || calls.Load() != 0 {
				t.Fatalf("error=%v calls=%d; rate-limit wait must consume timeout", err, calls.Load())
			}
			if elapsed := time.Since(start); elapsed >= 500*time.Millisecond {
				t.Fatalf("rate-limit wait exceeded budget: %v", elapsed)
			}
		})
	}
}

func TestFlowm_TimeoutPolicy_CancelsActionAndAllowsFallback(t *testing.T) {
	done := make(chan error, 1)
	f := New()
	f.Do(func(ctx context.Context) error {
		<-ctx.Done()
		done <- ctx.Err()
		return ctx.Err()
	}).With(&TimeoutPolicy{PerAttempt: 15 * time.Millisecond}).OnError(func(ctx context.Context, err error) (string, error) {
		if ctx.Err() != nil || !errors.Is(err, context.DeadlineExceeded) {
			return "", fmt.Errorf("fallback context=%v error=%v", ctx.Err(), err)
		}
		return "cached", nil
	})
	result, err := f.StartWithResult()
	if err != nil || !reflect.DeepEqual(result, []interface{}{"cached"}) {
		t.Fatalf("result=%v error=%v", result, err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("unexpected action cancellation: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("action did not receive its attempt deadline")
	}
}

func TestFlowm_Parallel_SharedRetryAndFallback(t *testing.T) {
	const count = 16
	retry := &RetryPolicy{Attempts: 2}
	fallback := &FallbackPolicy{Func: func(err error) string { return err.Error() }}
	var calls [count]atomic.Int32
	actions := make([]Action, count)
	want := make([]interface{}, count)
	for i := range actions {
		want[i] = fmt.Sprint(i)
		actions[i] = Do(func() error {
			calls[i].Add(1)
			return fmt.Errorf("%d", i)
		}).With(retry, fallback)
	}
	f := New()
	f.AddParallel(actions...)
	result, err := f.StartWithResult()
	if err != nil || !reflect.DeepEqual(result, []interface{}{want}) {
		t.Fatalf("shared fallback mixed results: got %v, want %v, error=%v", result, want, err)
	}
	for i := range calls {
		if calls[i].Load() != 2 {
			t.Fatalf("action %d ran %d times, expected 2", i, calls[i].Load())
		}
	}
}

func TestFlowm_Parallel_ContinueOnErrorFiltersPipedErrors(t *testing.T) {
	f := New()
	f.AddParallel(
		Do(func() (int, error) { return 7, errors.New("ignored") }).With(&ContinueOnErrorPolicy{Continue: true}),
		Do(func() int { return 9 }),
	).PipeAll()
	f.Do(func(a, b int) int { return a + b })
	result, err := f.StartWithResult()
	if err != nil || !reflect.DeepEqual(result, []interface{}{16}) {
		t.Fatalf("result=%v error=%v", result, err)
	}
}

func TestFlowm_ParallelErrors_PreserveAllErrors(t *testing.T) {
	first := errors.New("first")
	second := &noRetryError{}
	f := New()
	f.AddParallel(Do(func() error { return first }), Do(func() error { return second }))
	err := f.Start()
	var target *noRetryError
	if !errors.Is(err, first) || !errors.Is(err, second) || !errors.As(err, &target) || target != second {
		t.Fatalf("joined error lost a branch failure: %v", err)
	}
}

func TestFlowm_PipingDoesNotMutateActionParams(t *testing.T) {
	params := []interface{}{2, "preserve"}
	f := New()
	f.Do(func() int { return 3 }).PipeAll()
	f.AddAction(Action{Func: func(a, b int) int { return a + b }, Params: params[:1]})
	for range 2 {
		result, err := f.StartWithResult()
		if err != nil || !reflect.DeepEqual(result, []interface{}{5}) || params[1] != "preserve" {
			t.Fatalf("result=%v error=%v params=%v", result, err, params)
		}
	}
}

func TestFlowm_When_UsesPreviousResult(t *testing.T) {
	for _, score := range []int{50, 95} {
		t.Run(fmt.Sprint(score), func(t *testing.T) {
			f := New()
			var ran bool
			f.Do(func() int { return score }).PipeAll()
			f.Do(func(value int) int {
				ran = true
				return value + 1
			}).When(func(values []interface{}) bool {
				return values[0].(int) > 90
			}).PipeAll()
			f.Do(func(value int) int { return value })
			result, err := f.StartWithResult()
			want := score
			if score > 90 {
				want++
			}
			if err != nil || !reflect.DeepEqual(result, []interface{}{want}) || ran != (score > 90) {
				t.Fatalf("result=%v error=%v ran=%v; expected score %d", result, err, ran, want)
			}
		})
	}
}

func TestFlowm_When_SkippedActionDoesNotRunPoliciesOrReplaceResult(t *testing.T) {
	f := New()
	var logs []string
	f.Do(func() int { return 42 }).PipeAll()
	f.Do(func(int) { t.Fatal("skipped action ran") }).
		With(logPolicy{logs: &logs}).
		OnError(func(error) { t.Fatal("skipped fallback ran") }).
		When(func([]interface{}) bool { return false })
	result, err := f.StartWithResult()
	if err != nil || !reflect.DeepEqual(result, []interface{}{42}) || len(logs) != 0 {
		t.Fatalf("result=%v error=%v policies=%v", result, err, logs)
	}
}

func TestFlowm_When_WithoutPipedValues(t *testing.T) {
	f := New()
	var ran bool
	f.Do(func(value int) { ran = value == 7 }, 7).When(func(values []interface{}) bool {
		if values != nil {
			t.Fatalf("expected nil piped values, got %v", values)
		}
		return true
	})
	if err := f.Start(); err != nil || !ran {
		t.Fatalf("error=%v ran=%v", err, ran)
	}
}

func TestFlowm_When_EvaluatedOnceBeforeRetries(t *testing.T) {
	checks, calls := 0, 0
	f := New()
	f.Do(func() error {
		calls++
		if calls == 1 {
			return errors.New("retry")
		}
		return nil
	}).When(func([]interface{}) bool {
		checks++
		return true
	}).With(&RetryPolicy{Attempts: 2})
	if err := f.Start(); err != nil || checks != 1 || calls != 2 {
		t.Fatalf("error=%v checks=%d calls=%d", err, checks, calls)
	}
}

func TestFlowm_InvalidAction_ReturnsError(t *testing.T) {
	for _, fn := range []interface{}{nil, "not a function", 42, (func())(nil)} {
		t.Run(fmt.Sprintf("%T", fn), func(t *testing.T) {
			f := New()
			f.Do(fn)
			if err := f.Start(); err == nil {
				t.Fatal("expected an invalid action to fail")
			}
		})
	}
}

func TestFlowm_Parallel_GroupCancellationReachesMembers(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan context.Context, 1)
	finished := make(chan error, 1)
	var calls atomic.Int32
	actions := make([]Action, 8)
	for i := range actions {
		actions[i] = Do(func(ctx context.Context) error {
			if calls.Add(1) == 1 {
				started <- ctx
			}
			<-ctx.Done()
			finished <- ctx.Err()
			return ctx.Err()
		})
	}
	actions[0] = actions[0].With(&WorkerPoolPolicy{Size: 1})
	f := New().WithContext(ctx)
	f.AddParallel(actions...).With(&TimeoutPolicy{Overall: time.Second})
	result := make(chan error, 1)
	go func() { result <- f.Start() }()
	select {
	case memberCtx := <-started:
		if _, ok := memberCtx.Deadline(); !ok {
			t.Fatal("parallel member did not receive the group deadline")
		}
	case <-time.After(time.Second):
		t.Fatal("parallel group did not start")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected canceled group, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("parallel group did not stop after cancellation")
	}
	select {
	case err := <-finished:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected member to receive cancellation, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("parallel member did not receive cancellation")
	}
	if calls.Load() != 1 {
		t.Fatalf("queued members ran after cancellation: %d calls", calls.Load())
	}
}
