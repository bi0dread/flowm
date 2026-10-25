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
		{"nil fallback", func(f Flowm) { f.Do(func() {}).OnError(nil) }},
		{"fallback arguments", func(f Flowm) { f.Do(func() {}).OnError(func(error, int) {}) }},
		{"predicate not function", func(f Flowm) { f.Do(func() {}).When(true) }},
		{"predicate result", func(f Flowm) { f.Do(func() {}).When(func() string { return "yes" }) }},
		{"predicate missing input", func(f Flowm) { f.Do(func() {}).When(func(int) bool { return true }) }},
		{"predicate multiple results", func(f Flowm) { f.Do(func() {}).When(func() (bool, error) { return true, nil }) }},
		{"duplicate predicates", func(f Flowm) { f.Do(func() {}).When(func() bool { return true }).When(func() bool { return true }) }},
		{"negative pipe index", func(f Flowm) { f.Do(func() {}).Pipe(-1) }},
		{"nil mapper", func(f Flowm) { f.Do(func() {}).Map(nil) }},
		{"concurrency on action", func(f Flowm) { f.Do(func() {}).WithConcurrency(2) }},
		{"zero concurrency", func(f Flowm) { f.AddParallel(Do(func() {})).WithConcurrency(0) }},
		{"negative concurrency", func(f Flowm) { f.AddParallel(Do(func() {})).WithConcurrency(-1) }},
		{"invalid member", func(f Flowm) { f.AddParallel(Do(func(int) {}).Named("bad-member")) }},
		{"member predicate", func(f Flowm) { f.AddParallel(Do(func() {}).With(&ConditionPolicy{If: func() bool { return true }})) }},
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
		OnError(func(error) int { t.Fatal("fallback called"); return 2 }).
		When(func() bool { t.Fatal("condition called"); return true }).
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

func TestFallback_ValidationAndInjectionAgree(t *testing.T) {
	cause := errors.New("failed")
	ctx := context.Background()
	for _, explicit := range []bool{false, true} {
		t.Run(fmt.Sprint(explicit), func(t *testing.T) {
			params := []interface{}{1, 2}
			if explicit {
				params = append([]interface{}{ctx}, params...)
			}
			f := New().WithContext(ctx)
			f.Do(func() error { return cause }).OnError(func(c context.Context, err error, values ...int) int {
				if c != ctx || err != cause {
					t.Errorf("context=%v error=%v", c, err)
				}
				return values[0] + values[1]
			}, params...)
			result, err := f.StartWithResult()
			if err != nil || !reflect.DeepEqual(result, []interface{}{3}) {
				t.Fatalf("result=%v error=%v", result, err)
			}
		})
	}
}

func TestWhen_TypedPredicates(t *testing.T) {
	type order struct{ Paid bool }
	cases := []struct {
		name      string
		produce   interface{}
		predicate interface{}
		consume   interface{}
	}{
		{"struct", func() order { return order{Paid: true} }, func(o order) bool { return o.Paid }, func(order) {}},
		{"multiple", func() (int, string) { return 7, "paid" }, func(id int, status string) bool { return id == 7 && status == "paid" }, func(int, string) {}},
		{"nil pointer", func() *order { return nil }, func(o *order) bool { return o == nil }, func(*order) {}},
		{"context", func() int { return 7 }, func(ctx context.Context, n int) bool { return ctx != nil && n == 7 }, func(int) {}},
		{"captured state", func() int { return 7 }, func() bool { return true }, func(int) {}},
		{"variadic", func() (int, int) { return 1, 2 }, func(ns ...int) bool { return len(ns) == 2 }, func(int, int) {}},
		{"legacy", func() int { return 7 }, func(values []interface{}) bool { return values[0] == 7 }, func(int) {}},
		{"nil predicate", func() int { return 7 }, nil, func(int) {}},
		{"typed nil predicate", func() int { return 7 }, (func(int) bool)(nil), func(int) {}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ran := false
			f := New().WithHooks(Hooks{OnSuccess: func(name string, _ []interface{}) {
				if name == "consumer" {
					ran = true
				}
			}})
			f.Do(tc.produce).PipeAll()
			f.Do(tc.consume).Named("consumer").When(tc.predicate)
			if err := f.Start(); err != nil || !ran {
				t.Fatalf("error=%v consumer ran=%v", err, ran)
			}
		})
	}
}

func TestWhen_OnSkipReportsOnlySkippedActions(t *testing.T) {
	var events []string
	f := New().WithHooks(Hooks{
		OnStart:   func(name string) { events = append(events, "start:"+name) },
		OnSuccess: func(name string, _ []interface{}) { events = append(events, "success:"+name) },
		OnSkip:    func(name string) { events = append(events, "skip:"+name) },
		OnError:   func(string, error) { t.Error("unexpected error hook") },
		OnRetry:   func(string, int, error) { t.Error("skipped action retried") },
	})
	f.Do(func() int { return 5 }).Named("producer").PipeAll()
	f.Do(func(int) { t.Fatal("skipped action ran") }).Named("skipped").
		When(func(n int) bool { return n > 10 }).With(&RetryPolicy{Attempts: 3}).
		OnError(func(error) { t.Fatal("skipped fallback ran") })
	f.Do(func(n int) int { return n }).Named("consumer")
	result, err := f.StartWithResult()
	want := []string{"start:producer", "success:producer", "skip:skipped", "start:consumer", "success:consumer"}
	if err != nil || !reflect.DeepEqual(result, []interface{}{5}) || !reflect.DeepEqual(events, want) {
		t.Fatalf("result=%v error=%v events=%v", result, err, events)
	}
}

func TestWhen_PredicateFailuresAreNamedErrors(t *testing.T) {
	for _, predicate := range []interface{}{func(string) bool { return true }, func(int) bool { panic("predicate failure") }} {
		t.Run(fmt.Sprintf("%T", predicate), func(t *testing.T) {
			var reported error
			f := New().WithHooks(Hooks{
				OnSkip:  func(string) { t.Error("failed predicate reported as skipped") },
				OnError: func(_ string, err error) { reported = err },
			})
			f.Do(func() int { return 7 }).PipeAll()
			f.Do(func(int) { t.Fatal("invalid predicate allowed execution") }).Named("conditional").When(predicate)
			err := f.Start()
			var failure *ActionError
			if !errors.As(err, &failure) || failure.Name != "conditional" || failure.Attempts != 0 || reported != err {
				t.Fatalf("error=%v reported=%v", err, reported)
			}
		})
	}
}

func TestWhen_CancellationDoesNotEmitSkip(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f := New().WithContext(ctx).WithHooks(Hooks{OnSkip: func(string) { t.Error("canceled action reported as skipped") }})
	f.Do(func() {}).When(func() bool { t.Error("canceled flow evaluated predicate"); return false })
	if err := f.Start(); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation, got %v", err)
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
	if !errors.As(err, &failure) || failure.Name != "fetch-user" || failure.Attempts != 3 || !errors.Is(err, cause) || !errors.As(err, &original) || original != cause || reported != err {
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

func TestParallel_TypedGroupCondition(t *testing.T) {
	for _, allowed := range []bool{false, true} {
		t.Run(fmt.Sprint(allowed), func(t *testing.T) {
			var ran atomic.Bool
			var skipped []string
			f := New().WithHooks(Hooks{OnSkip: func(name string) { skipped = append(skipped, name) }})
			f.Do(func() bool { return allowed }).PipeAll()
			f.AddParallel(Do(func(n int) int { ran.Store(true); return n }, 7)).
				Named("conditional-group").WithConcurrency(1).When(func(value bool) bool { return value })
			_, err := f.StartWithResult()
			if err != nil || ran.Load() != allowed || len(skipped) != map[bool]int{true: 0, false: 1}[allowed] {
				t.Fatalf("error=%v ran=%v skipped=%v", err, ran.Load(), skipped)
			}
		})
	}
}
