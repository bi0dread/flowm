package flowm

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestJump_BackwardFromStepSixToStepTwo(t *testing.T) {
	var visited []int
	passes := 0
	f := New().WithMaxJumps(2)
	for step := 1; step <= 7; step++ {
		f.Do(func(n int) {
			visited = append(visited, n)
			if n == 6 {
				passes++
			}
		}, step).Named(fmt.Sprintf("step-%d", step))
		if step == 6 {
			f.When(func(out Outcome) Action {
				if out.Err == nil && passes < 2 {
					return Jump("step-2")
				}
				return NoAction()
			})
		}
	}
	want := []int{1, 2, 3, 4, 5, 6, 2, 3, 4, 5, 6, 7}
	if err := f.Start(); err != nil || !reflect.DeepEqual(visited, want) {
		t.Fatalf("error=%v visited=%v, want %v", err, visited, want)
	}
}

func TestJump_FiveStepInfiniteCycleStopsAtJumpLimit(t *testing.T) {
	var visited []int
	record := func(step int) {
		visited = append(visited, step)
		fmt.Println(fmt.Sprintf("step=%d", step))
		time.Sleep(time.Second)
	}
	f := New().WithMaxJumps(3)
	f.Do(record, 1).Named("step-1")
	f.Do(record, 2).Named("step-2")
	f.Do(record, 3).Named("step-3")
	f.Do(record, 4).Named("step-4").When(func(Outcome) Action {
		// Always jump back: the flow's limit is the only exit from this cycle.
		return Jump("step-2")
	})
	f.Do(record, 5).Named("step-5")

	err := f.Start()
	if !errors.Is(err, ErrJumpLimitExceeded) {
		t.Fatalf("expected the jump limit to stop the cycle, got %v", err)
	}
	var failure *ActionError
	if !errors.As(err, &failure) || failure.Name != "step-4" || failure.Attempts != 1 {
		t.Fatalf("expected the rejected jump to identify step 4, got %v", err)
	}
	// Step 1 runs once; steps 2–4 repeat; step 5 is never reached.
	want := []int{1, 2, 3, 4, 2, 3, 4, 2, 3, 4, 2, 3, 4}
	if !reflect.DeepEqual(visited, want) {
		t.Fatalf("visited=%v, want %v", visited, want)
	}
}

func TestJump_ForwardUsesFirstMatchAndContinuesFromTarget(t *testing.T) {
	var events []string
	f := New().WithHooks(Hooks{
		OnStart: func(name string) { events = append(events, name) },
		OnSkip:  func(name string) { events = append(events, "skip:"+name) },
	})
	f.Do(func() {}).Named("source").
		When(func(Outcome) Action { return NoAction() }).
		When(func(Outcome) Action { return Jump("target") }).
		When(func(Outcome) Action { t.Fatal("later rule evaluated"); return Jump("skipped") })
	f.Do(func() { t.Fatal("jumped-over step ran") }).Named("skipped")
	f.Do(func() string { return "target" }).Named("target").PipeAll()
	f.Do(func(s string) string { return s + "-next" }).Named("next")
	result, err := f.StartWithResult()
	wantEvents := []string{"source", "skip:source", "target", "next"}
	if err != nil || !reflect.DeepEqual(result, []interface{}{"target-next"}) || !reflect.DeepEqual(events, wantEvents) {
		t.Fatalf("result=%v error=%v events=%v", result, err, events)
	}
}

func TestJump_ErrorAfterRetriesCanSelectTarget(t *testing.T) {
	cause := errors.New("unavailable")
	calls, checks, reports := 0, 0, 0
	f := New().WithHooks(Hooks{OnError: func(name string, err error) {
		if name != "source" || !errors.Is(err, cause) {
			t.Errorf("unexpected error report: %s %v", name, err)
		}
		reports++
	}})
	f.Do(func() (int, error) { calls++; return calls, cause }).Named("source").
		When(func(out Outcome) Action {
			checks++
			if out.Attempts != 3 || out.Values[0] != 3 {
				t.Fatalf("jump saw intermediate outcome: %+v", out)
			}
			if errors.Is(out.Err, cause) {
				return Jump("recover")
			}
			return NoAction()
		}).With(&RetryPolicy{Attempts: 3}).PipeAll()
	f.Do(func() { t.Fatal("normal successor ran") })
	f.Do(func(n int) int { return n + 10 }).Named("recover")
	result, err := f.StartWithResult()
	if err != nil || !reflect.DeepEqual(result, []interface{}{13}) || calls != 3 || checks != 1 || reports != 1 {
		t.Fatalf("result=%v error=%v calls=%d checks=%d reports=%d", result, err, calls, checks, reports)
	}
}

func TestJump_PipingTargetsActualNextStep(t *testing.T) {
	cases := []struct {
		name string
		pipe func(Flowm)
		want int
	}{
		{"all", func(f Flowm) { f.PipeAll() }, 3},
		{"index", func(f Flowm) { f.Pipe(1) }, 2},
		{"map", func(f Flowm) { f.Map(func(v []interface{}) []interface{} { return []interface{}{v[0].(int) * 10} }) }, 10},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := New()
			f.Do(func() (int, int) { return 1, 2 }).When(func(Outcome) Action { return Jump("target") })
			tc.pipe(f)
			f.Do(func() { t.Fatal("skipped action ran") })
			f.Do(func(prefix string, ns ...int) string {
				sum := 0
				for _, n := range ns {
					sum += n
				}
				return fmt.Sprintf("%s%d", prefix, sum)
			}, "sum:").Named("target")
			result, err := f.StartWithResult()
			if err != nil || !reflect.DeepEqual(result, []interface{}{fmt.Sprintf("sum:%d", tc.want)}) {
				t.Fatalf("result=%v error=%v", result, err)
			}
		})
	}
}

func TestJump_BackwardPipingUsesFreshArgumentsOnEveryVisit(t *testing.T) {
	f := New()
	f.Do(func() int { return 0 }).PipeAll()
	f.Do(func(prefix string, n int) (string, int) { return prefix, n + 1 }, "value").Named("increment").
		When(func(out Outcome) Action {
			if out.Values[1].(int) < 3 {
				return Jump("increment")
			}
			return NoAction()
		}).Pipe(1)
	f.Do(func(n int) int { return n })
	for range 2 {
		result, err := f.StartWithResult()
		if err != nil || !reflect.DeepEqual(result, []interface{}{3}) {
			t.Fatalf("parameters accumulated across visits or runs: result=%v error=%v", result, err)
		}
	}
}

func TestJump_WithoutPipingDoesNotReplayPreviousInputs(t *testing.T) {
	calls := 0
	f := New()
	f.Do(func() int { return 7 }).PipeAll()
	f.Do(func(n int) int { calls++; return n }).Named("target")
	f.Do(func() {}).When(func(Outcome) Action { return Jump("target") })
	err := f.Start()
	var failure *ActionError
	if !errors.As(err, &failure) || failure.Name != "target" || failure.Attempts != 0 || calls != 1 {
		t.Fatalf("previous inputs were replayed: error=%v calls=%d", err, calls)
	}
}

func TestJump_NestedBranchCanTransferControl(t *testing.T) {
	f := New()
	f.Do(func() error { return errors.New("source") }).When(func(out Outcome) Action {
		if failed(out) {
			return Do(func() (string, error) { return "branch-value", errors.New("branch") }).When(func(out Outcome) Action {
				if failed(out) {
					return Jump("target")
				}
				return NoAction()
			})
		}
		return NoAction()
	}).PipeAll()
	f.Do(func() { t.Fatal("normal successor ran") })
	f.Do(func(s string) string { return s }).Named("target")
	result, err := f.StartWithResult()
	if err != nil || !reflect.DeepEqual(result, []interface{}{"branch-value"}) {
		t.Fatalf("result=%v error=%v", result, err)
	}
}

func TestJump_LimitStopsCyclesAndResetsForEachRun(t *testing.T) {
	for _, limit := range []int{2, defaultMaxJumps} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			f := New()
			if limit != defaultMaxJumps {
				f.WithMaxJumps(limit)
			}
			calls, reports := 0, 0
			f.WithHooks(Hooks{OnError: func(_ string, err error) {
				if errors.Is(err, ErrJumpLimitExceeded) {
					reports++
				}
			}})
			f.Do(func() { calls++ }).Named("loop").When(func(Outcome) Action { return Jump("loop") })
			for run := 1; run <= 2; run++ {
				err := f.Start()
				var failure *ActionError
				if !errors.Is(err, ErrJumpLimitExceeded) || !errors.As(err, &failure) || failure.Name != "loop" || failure.Attempts != 1 || calls != run*(limit+1) || reports != run {
					t.Fatalf("error=%v calls=%d reports=%d", err, calls, reports)
				}
			}
		})
	}
}

func TestJump_TargetGetsFreshRetryBudget(t *testing.T) {
	calls, checks := 0, 0
	f := New()
	f.Do(func() error {
		calls++
		if calls%2 == 1 {
			return errors.New("retry")
		}
		return nil
	}).Named("target").With(&RetryPolicy{Attempts: 2}).When(func(out Outcome) Action {
		checks++
		if out.Err != nil || out.Attempts != 2 {
			t.Fatalf("retry state leaked across visits: %+v", out)
		}
		if checks == 1 {
			return Jump("target")
		}
		return NoAction()
	})
	if err := f.Start(); err != nil || calls != 4 || checks != 2 {
		t.Fatalf("error=%v calls=%d checks=%d", err, calls, checks)
	}
}

func TestJump_CancellationStopsBackwardRouting(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls atomic.Int32
	checks := 0
	f := New().WithContext(ctx)
	f.Do(func() {
		if calls.Add(1) == 3 {
			cancel()
		}
	}).Named("loop").When(func(Outcome) Action { checks++; return Jump("loop") })
	if err := f.Start(); !errors.Is(err, context.Canceled) || calls.Load() != 3 || checks != 2 {
		t.Fatalf("error=%v calls=%d checks=%d", err, calls.Load(), checks)
	}
}

func TestJump_CompletedParallelGroupCanSelectTarget(t *testing.T) {
	var completed atomic.Int32
	f := New()
	f.AddParallel(
		Do(func() int { completed.Add(1); return 3 }),
		Do(func() int { completed.Add(1); return 4 }),
	).Named("group").WithConcurrency(2).When(func(Outcome) Action {
		if completed.Load() != 2 {
			t.Fatal("group routed before members finished")
		}
		return Jump("target")
	}).PipeAll()
	f.Do(func() { t.Fatal("skipped step ran") })
	f.Do(func(a, b int) int { return a + b }).Named("target")
	result, err := f.StartWithResult()
	if err != nil || !reflect.DeepEqual(result, []interface{}{7}) {
		t.Fatalf("result=%v error=%v", result, err)
	}
}

func TestJump_ValidatesReturnedDestinationsBeforeTransfer(t *testing.T) {
	cases := []struct {
		name    string
		build   func(Flowm)
		message string
	}{
		{"empty", func(f Flowm) { f.When(func(Outcome) Action { return Jump("") }) }, "must not be empty"},
		{"whitespace", func(f Flowm) { f.When(func(Outcome) Action { return Jump(" ") }) }, "must not be empty"},
		{"missing", func(f Flowm) { f.When(func(Outcome) Action { return Jump("missing") }) }, "explicitly Named"},
		{"implicit name", func(f Flowm) { f.Do(jumpUnnamed).When(func(Outcome) Action { return Jump("jumpUnnamed") }) }, "explicitly Named"},
		{"ambiguous", func(f Flowm) { f.When(func(Outcome) Action { return Jump("target") }); f.Do(func() {}).Named("target") }, "ambiguous"},
		{"member jump", func(f Flowm) { f.AddParallel(Do(func() {}).When(func(Outcome) Action { return Jump("target") })) }, "parallel member"},
		{"nested member jump", func(f Flowm) {
			f.AddParallel(Do(func() {}).When(func(Outcome) Action { return Do(func() {}).When(func(Outcome) Action { return Jump("target") }) }))
		}, "parallel member"},
		{"branch target", func(f Flowm) {
			f.When(func(Outcome) Action {
				return Do(func() {}).Named("branch").When(func(Outcome) Action { return Jump("branch") })
			})
		}, "explicitly Named"},
		{"member target", func(f Flowm) {
			f.When(func(Outcome) Action { return Jump("member") })
			f.AddParallel(Do(func() {}).Named("member"))
		}, "explicitly Named"},
		{"jump policy", func(f Flowm) { f.When(func(Outcome) Action { return Jump("target").With(&RetryPolicy{Attempts: 2}) }) }, "Jump cannot have"},
		{"jump routes", func(f Flowm) {
			f.When(func(Outcome) Action { return Jump("target").When(func(Outcome) Action { return Do(func() {}) }) })
		}, "Jump cannot have"},
		{"jump function", func(f Flowm) { a := Jump("target"); a.Func = func() {}; f.When(func(Outcome) Action { return a }) }, "Jump cannot have"},
		{"jump arguments", func(f Flowm) {
			a := Jump("target")
			a.Params = []interface{}{1}
			f.When(func(Outcome) Action { return a })
		}, "Jump cannot have"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ran, targetRan := false, false
			f := New()
			f.Do(func() { ran = true })
			tc.build(f)
			f.Do(func() { targetRan = true }).Named("target")
			if err := f.Validate(); err != nil || ran {
				t.Fatalf("preflight invoked a callback or guessed its decision: %v", err)
			}
			err := f.Start()
			var failure *ActionError
			if !errors.As(err, &failure) || !strings.Contains(err.Error(), tc.message) || !ran || targetRan {
				t.Fatalf("error=%v source=%v target=%v, want %q", err, ran, targetRan, tc.message)
			}
		})
	}
}

func TestJump_StaticConfigurationFailsBeforeExecution(t *testing.T) {
	for _, build := range []func(Flowm){
		func(f Flowm) { f.AddAction(Jump("target")) },
		func(f Flowm) { f.WithMaxJumps(0) },
		func(f Flowm) { f.WithMaxJumps(-1) },
	} {
		f := New()
		f.Do(func() { t.Fatal("invalid flow executed") }).Named("target")
		build(f)
		var failure *ActionError
		if err := f.Start(); !errors.As(err, &failure) || failure.Attempts != 0 {
			t.Fatalf("expected validation failure, got %v", err)
		}
	}
}

func TestJump_DestinationChosenFromOutcome(t *testing.T) {
	for _, destination := range []string{"left", "right"} {
		t.Run(destination, func(t *testing.T) {
			visited := ""
			f := New()
			f.Do(func() string { return destination }).When(func(out Outcome) Action {
				return Jump(out.Values[0].(string))
			})
			f.Do(func() { visited += "left" }).Named("left").When(func(Outcome) Action { return Jump("done") })
			f.Do(func() { visited += "right" }).Named("right")
			f.Do(func() string { return visited }).Named("done")
			result, err := f.StartWithResult()
			if err != nil || !reflect.DeepEqual(result, []interface{}{destination}) {
				t.Fatalf("result=%v error=%v", result, err)
			}
		})
	}
}

func jumpUnnamed() {}

func TestJump_UnreferencedDuplicateNamesRemainValid(t *testing.T) {
	f := New()
	f.Do(func() {}).Named("same")
	f.Do(func() {}).Named("same")
	if err := f.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestJump_DynamicArgumentsValidatedBeforeTargetRuns(t *testing.T) {
	f := New()
	f.Do(func() string { return "wrong-type" }).When(func(Outcome) Action { return Jump("target") }).PipeAll()
	f.Do(func() {})
	f.Do(func(int) { t.Fatal("invalid target ran") }).Named("target")
	err := f.Start()
	var failure *ActionError
	if !errors.As(err, &failure) || failure.Name != "target" || failure.Attempts != 0 || !strings.Contains(err.Error(), "not assignable") {
		t.Fatalf("unexpected argument failure: %v", err)
	}
}

func TestJump_MappingFailurePreventsTransfer(t *testing.T) {
	f := New()
	f.Do(func() int { return 1 }).Named("source").
		When(func(Outcome) Action { return Jump("target") }).
		Map(func([]interface{}) []interface{} { panic("bad mapping") })
	f.Do(func() { t.Fatal("target ran after mapping failure") }).Named("target")
	err := f.Start()
	var failure *ActionError
	if !errors.As(err, &failure) || failure.Name != "source" || !strings.Contains(err.Error(), "bad mapping") {
		t.Fatalf("unexpected mapping failure: %v", err)
	}
}
