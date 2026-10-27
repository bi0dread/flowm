package flowm

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestValidate_RejectsDefinitionsBeforeSideEffects(t *testing.T) {
	cases := []struct {
		name string
		add  func(Flowm)
	}{
		{"nil function", func(f Flowm) { f.Do(nil) }},
		{"non-function", func(f Flowm) { f.Do("invalid") }},
		{"nil function pointer", func(f Flowm) { f.Do((func())(nil)) }},
		{"argument type", func(f Flowm) { f.Do(func(int) {}, "wrong") }},
		{"missing argument", func(f Flowm) { f.Do(func(int) {}) }},
		{"extra argument", func(f Flowm) { f.Do(func() {}, 1) }},
		{"nil scalar", func(f Flowm) { f.Do(func(int) {}, nil) }},
		{"nil policy", func(f Flowm) { f.Do(func() {}).With(nil) }},
		{"typed nil policy", func(f Flowm) { f.Do(func() {}).With((*RetryPolicy)(nil)) }},
		{"retry attempts", func(f Flowm) { f.Do(func() {}).With(&RetryPolicy{Attempts: -1}) }},
		{"retry delay", func(f Flowm) { f.Do(func() {}).With(&RetryPolicy{Delay: -1}) }},
		{"retry max delay", func(f Flowm) { f.Do(func() {}).With(&RetryPolicy{MaxDelay: -1}) }},
		{"retry NaN", func(f Flowm) { f.Do(func() {}).With(&RetryPolicy{BackoffFactor: math.NaN()}) }},
		{"retry infinity", func(f Flowm) { f.Do(func() {}).With(&RetryPolicy{BackoffFactor: math.Inf(1)}) }},
		{"retry factor", func(f Flowm) { f.Do(func() {}).With(&RetryPolicy{BackoffFactor: 0.5}) }},
		{"timeout", func(f Flowm) { f.Do(func() {}).With(&TimeoutPolicy{Overall: -1}) }},
		{"duplicate timeout", func(f Flowm) { f.Do(func() {}).With(&TimeoutPolicy{}).With(&TimeoutPolicy{}) }},
		{"rate interval", func(f Flowm) { f.Do(func() {}).With(&RateLimitPolicy{Burst: 1}) }},
		{"rate burst", func(f Flowm) { f.Do(func() {}).With(&RateLimitPolicy{Per: time.Second}) }},
		{"breaker", func(f Flowm) { f.Do(func() {}).With(&CircuitBreakerPolicy{}) }},
		{"nil callback", func(f Flowm) { f.Do(func() {}).When(nil) }},
		{"typed nil callback", func(f Flowm) { f.Do(func() {}).When((func(Outcome) Action)(nil)) }},
		{"negative pipe index", func(f Flowm) { f.Do(func() {}).Pipe(-1) }},
		{"nil mapper", func(f Flowm) { f.Do(func() {}).Map(nil) }},
		{"concurrency on action", func(f Flowm) { f.Do(func() {}).WithConcurrency(2) }},
		{"zero concurrency", func(f Flowm) { f.AddParallel(Do(func() {})).WithConcurrency(0) }},
		{"negative concurrency", func(f Flowm) { f.AddParallel(Do(func() {})).WithConcurrency(-1) }},
		{"invalid member", func(f Flowm) { f.AddParallel(Do(func(int) {}).Named("bad-member")) }},
		{"conflicting legacy limits", func(f Flowm) {
			f.AddParallel(Do(func() {}).With(&WorkerPoolPolicy{Size: 1}), Do(func() {}).With(&WorkerPoolPolicy{Size: 2}))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ran := false
			f := New().WithHooks(Hooks{OnStart: func(string) { t.Error("validation invoked a hook") }})
			f.Do(func() { ran = true }).Named("side-effect")
			tc.add(f)
			f.Named("invalid")
			for _, err := range []error{f.Validate(), f.Start()} {
				var failure *ActionError
				if !errors.As(err, &failure) || failure.Name != "invalid" || failure.Attempts != 0 {
					t.Fatalf("expected named validation failure, got %v", err)
				}
			}
			if ran {
				t.Fatal("flow executed before all definitions were validated")
			}
		})
	}
}

func TestValidate_DoesNotInvokeUserCode(t *testing.T) {
	f := New()
	f.Do(func() int { t.Fatal("action called"); return 1 }).
		With(&RetryPolicy{Attempts: 2, RetryIf: func(error) bool { t.Fatal("retry predicate called"); return false }}).
		When(func(Outcome) Action {
			t.Fatal("route predicate called")
			return Do(func(Outcome) int { t.Fatal("branch called"); return 2 })
		}).
		Map(func([]interface{}) []interface{} { t.Fatal("mapper called"); return nil })
	f.Do(func(int) {})
	if err := f.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestValidate_ExplicitArgumentsCheckedEvenWithIncomingValues(t *testing.T) {
	ran := false
	f := New()
	f.Do(func() int { ran = true; return 1 }).PipeAll()
	f.Do(func(string, int) {}, false).Named("consumer")
	if err := f.Start(); err == nil || ran {
		t.Fatalf("error=%v producer ran=%v", err, ran)
	}
}

func TestValidate_DynamicArgumentsCheckedBeforeConsumer(t *testing.T) {
	f := New()
	producerRan, consumerRan := false, false
	f.Do(func() int { producerRan = true; return 1 }).Map(func([]interface{}) []interface{} {
		return []interface{}{"wrong runtime type"}
	})
	f.Do(func(int) { consumerRan = true }).Named("consumer")
	if err := f.Validate(); err != nil {
		t.Fatalf("dynamic values must not be guessed by preflight: %v", err)
	}
	var failure *ActionError
	err := f.Start()
	if !errors.As(err, &failure) || failure.Name != "consumer" || failure.Attempts != 0 || !producerRan || consumerRan {
		t.Fatalf("error=%v producer=%v consumer=%v", err, producerRan, consumerRan)
	}
}

func TestArguments_ContextNilAndVariadic(t *testing.T) {
	type key struct{}
	ctx := context.WithValue(context.Background(), key{}, "context")
	for _, explicit := range []bool{false, true} {
		t.Run(fmt.Sprint(explicit), func(t *testing.T) {
			f := New().WithContext(ctx)
			params := []interface{}{nil, 1, 2, 3}
			if explicit {
				params = append([]interface{}{ctx}, params...)
			}
			f.Do(func(c context.Context, pointer *int, values ...int) int {
				if c.Value(key{}) != "context" || pointer != nil {
					t.Error("context or nil argument changed")
				}
				return values[0] + values[1] + values[2]
			}, params...)
			result, err := f.StartWithResult()
			if err != nil || !reflect.DeepEqual(result, []interface{}{6}) {
				t.Fatalf("result=%v error=%v", result, err)
			}
		})
	}
}

func TestArguments_ContextAsOrdinaryValue(t *testing.T) {
	ctx := context.Background()
	f := New().WithContext(ctx)
	f.Do(func(injected, value context.Context) bool { return injected == ctx && value == ctx }, ctx)
	result, err := f.StartWithResult()
	if err != nil || !reflect.DeepEqual(result, []interface{}{true}) {
		t.Fatalf("result=%v error=%v", result, err)
	}
}

type detailedFailure struct{ message string }

func (e *detailedFailure) Error() string { return e.message }

func TestNamedAction_ErrorsAndHooksPreserveCause(t *testing.T) {
	cause := &detailedFailure{message: "service down"}
	var events []string
	var reported error
	f := New().WithHooks(Hooks{
		OnStart: func(name string) { events = append(events, "start:"+name) },
		OnRetry: func(name string, attempt int, err error) {
			events = append(events, fmt.Sprintf("retry:%s:%d", name, attempt))
			if !errors.Is(err, cause) {
				t.Errorf("retry lost original error: %v", err)
			}
		},
		OnError: func(name string, err error) { events = append(events, "error:"+name); reported = err },
	})
	f.Do(func() error { return cause }).Named("fetch-user").With(&RetryPolicy{Attempts: 3})
	err := f.Start()
	var failure *ActionError
	var original *detailedFailure
	if !errors.As(err, &failure) || failure.Name != "fetch-user" || failure.Attempts != 3 || !errors.Is(err, cause) || !errors.As(err, &original) || original != cause || !errors.Is(reported, cause) {
		t.Fatalf("structured error lost details: %v", err)
	}
	want := []string{"start:fetch-user", "retry:fetch-user:2", "retry:fetch-user:3", "error:fetch-user"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("hooks=%v, want %v", events, want)
	}
}

func TestNamedAction_ParallelErrorsIdentifyMembers(t *testing.T) {
	first, second := errors.New("first"), errors.New("second")
	f := New()
	f.AddParallel(
		Do(func() error { return first }).Named("profile"),
		Do(func() error { return second }).Named("permissions"),
	).Named("load-user").WithConcurrency(2)
	err := f.Start()
	var failure *ActionError
	if !errors.As(err, &failure) || failure.Name != "load-user" || !errors.Is(err, first) || !errors.Is(err, second) {
		t.Fatalf("parallel error lost context: %v", err)
	}
	for _, name := range []string{"profile", "permissions"} {
		if !strings.Contains(err.Error(), name) {
			t.Fatalf("parallel error omitted member %q: %v", name, err)
		}
	}
}

func TestParallel_GroupConcurrencyAndResultOrder(t *testing.T) {
	const count, limit = 6, 2
	var active, peak atomic.Int32
	entered := make(chan struct{}, count)
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	actions := make([]Action, count)
	want := make([]interface{}, count)
	for i := range actions {
		want[i] = i
		actions[i] = Do(func() int {
			n := active.Add(1)
			defer active.Add(-1)
			for old := peak.Load(); n > old && !peak.CompareAndSwap(old, n); old = peak.Load() {
			}
			entered <- struct{}{}
			<-release
			return i
		}).Named(fmt.Sprint(i)).With(&WorkerPoolPolicy{Size: 1})
	}
	f := New()
	f.AddParallel(actions...).WithConcurrency(limit) // overrides legacy member limits
	type outcome struct {
		values []interface{}
		err    error
	}
	done := make(chan outcome, 1)
	go func() { result, err := f.StartWithResult(); done <- outcome{result, err} }()
	for range limit {
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("group did not fill its concurrency limit")
		}
	}
	if active.Load() != limit {
		t.Fatalf("group started %d members, expected %d", active.Load(), limit)
	}
	unblock()
	select {
	case result := <-done:
		if result.err != nil || !reflect.DeepEqual(result.values, []interface{}{want}) || peak.Load() != limit {
			t.Fatalf("result=%v error=%v peak=%d", result.values, result.err, peak.Load())
		}
	case <-time.After(time.Second):
		t.Fatal("group did not finish")
	}
}
