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

func failed(out Outcome) bool { return out.Err != nil }

func TestWhen_CallbackChoosesActionJumpOrOriginalOutcome(t *testing.T) {
	cases := []struct {
		name      string
		value     int
		err       error
		want      int
		nextCalls int
	}{
		{"action", 1, nil, 111, 1},
		{"jump", 2, errors.New("recover"), 2, 0},
		{"original", 5, nil, 105, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			checks, nextCalls := 0, 0
			f := New()
			f.Do(func() (int, error) { return tc.value, tc.err }).When(func(out Outcome) Action {
				checks++
				if out.Err != nil {
					return Jump("target")
				}
				if out.Values[0].(int) == 1 {
					return Do(func(n int) int { return n + 10 }, out.Values[0])
				}
				return NoAction()
			}).PipeAll()
			f.Do(func(n int) int { nextCalls++; return n + 100 }).PipeAll()
			f.Do(func(n int) int { return n }).Named("target")
			result, err := f.StartWithResult()
			if err != nil || !reflect.DeepEqual(result, []interface{}{tc.want}) || checks != 1 || nextCalls != tc.nextCalls {
				t.Fatalf("result=%v error=%v checks=%d next=%d", result, err, checks, nextCalls)
			}
		})
	}
}

func TestWhen_ValidatesReturnedActionsBeforeRunningThem(t *testing.T) {
	cause := errors.New("source failure")
	for _, action := range []Action{
		Do(nil), Do((func())(nil)), Do("not a function"),
		Do(func(Outcome, int) { t.Fatal("invalid arguments ran") }),
		Do(func() { t.Fatal("invalid policy ran") }).With(&RetryPolicy{Attempts: -1}),
		Do(func() { t.Fatal("invalid piping ran") }).With(&PipeAllPolicy{}),
		Do(func() { t.Fatal("nil nested callback ran") }).When(nil),
		Action{Name: "missing-function"},
	} {
		checks := 0
		f := New().WithHooks(Hooks{OnStart: func(name string) {
			if name == "invalid" {
				t.Fatal("invalid returned action started")
			}
		}})
		f.Do(func() error { return cause }).When(func(Outcome) Action {
			checks++
			return action.Named("invalid")
		})
		if err := f.Validate(); err != nil || checks != 0 {
			t.Fatalf("preflight evaluated the callback: error=%v checks=%d", err, checks)
		}
		err := f.Start()
		var failure *ActionError
		if !errors.Is(err, cause) || !errors.As(err, &failure) || failure.Name != "invalid" || failure.Attempts != 0 || checks != 1 {
			t.Fatalf("invalid action was accepted or lost source cause: error=%v checks=%d", err, checks)
		}
	}
}

func TestWhen_OnlyReturnedDefinitionsAreValidated(t *testing.T) {
	invalid := Do(func(int) { t.Fatal("invalid branch ran") }).Named("invalid")
	selected := false
	f := New()
	f.Do(func() {}).When(func(Outcome) Action {
		if selected {
			return invalid
		}
		return NoAction()
	})
	if err := f.Start(); err != nil {
		t.Fatal(err)
	}
	selected = true
	var failure *ActionError
	if err := f.Start(); !errors.As(err, &failure) || failure.Name != "invalid" || failure.Attempts != 0 {
		t.Fatalf("returned definition was not validated: %v", err)
	}
}

func TestWhen_RoutesCompletedSuccessAndReplacesValues(t *testing.T) {
	for _, score := range []int{50, 95} {
		t.Run(fmt.Sprint(score), func(t *testing.T) {
			var sourceRan, branchRan bool
			f := New()
			f.Do(func() int { sourceRan = true; return score }).Named("score").When(
				func(out Outcome) Action {
					if !sourceRan || out.Name != "score" || out.Attempts != 1 || out.Err != nil {
						t.Fatalf("predicate ran before the source completed: %+v", out)
					}
					if out.Values[0].(int) > 90 {
						return Do(func(out Outcome) int { branchRan = true; return out.Values[0].(int) + 1 })
					}
					return NoAction()
				},
			).PipeAll()
			f.Do(func(n int) int { return n })
			result, err := f.StartWithResult()
			want := score
			if score > 90 {
				want++
			}
			if err != nil || !reflect.DeepEqual(result, []interface{}{want}) || branchRan != (score > 90) {
				t.Fatalf("result=%v error=%v branch=%v", result, err, branchRan)
			}
		})
	}
}

func TestWhen_OnlyFirstMatchingRuleRuns(t *testing.T) {
	var evaluated []int
	f := New()
	f.Do(func() int { return 1 }).
		When(func(Outcome) Action { evaluated = append(evaluated, 1); return NoAction() }).
		When(func(Outcome) Action { evaluated = append(evaluated, 2); return Do(func() string { return "selected" }) }).
		When(func(Outcome) Action {
			t.Fatal("later predicate evaluated")
			return Do(func() { t.Fatal("later branch ran") })
		})
	result, err := f.StartWithResult()
	if err != nil || !reflect.DeepEqual(evaluated, []int{1, 2}) || !reflect.DeepEqual(result, []interface{}{"selected"}) {
		t.Fatalf("result=%v error=%v predicates=%v", result, err, evaluated)
	}
}

func TestWhen_UnmatchedErrorStopsFlow(t *testing.T) {
	cause := errors.New("not recovered")
	f := New()
	f.Do(func() error { return cause }).Named("source").
		When(func(out Outcome) Action {
			if out.Err == nil {
				return Do(func() { t.Fatal("success branch ran") })
			}
			return NoAction()
		})
	f.Do(func() { t.Fatal("flow continued after unmatched failure") })
	err := f.Start()
	var failure *ActionError
	if !errors.Is(err, cause) || !errors.As(err, &failure) || failure.Name != "source" || failure.Attempts != 1 {
		t.Fatalf("source failure was lost: %v", err)
	}
}

func TestWhen_ErrorBranchRecoversAndPipesReplacement(t *testing.T) {
	cause := errors.New("service down")
	f := New()
	f.Do(func() (string, error) { return "partial", cause }).When(func(out Outcome) Action {
		if failed(out) {
			return Do(func(out Outcome, prefix string) string {
				if out.Err != cause || !reflect.DeepEqual(out.Values, []interface{}{"partial"}) {
					t.Fatalf("branch did not receive the completed outcome: %+v", out)
				}
				return prefix + "cached"
			}, "from-").Named("cache")
		}
		return NoAction()
	},
	).PipeAll()
	f.Do(func(value string) string { return value + "-processed" })
	result, err := f.StartWithResult()
	if err != nil || !reflect.DeepEqual(result, []interface{}{"from-cached-processed"}) {
		t.Fatalf("result=%v error=%v", result, err)
	}
}

func TestWhen_SelectedFailureDoesNotTrySibling(t *testing.T) {
	original, branchFailure := errors.New("original"), errors.New("branch failed")
	f := New()
	f.Do(func() error { return original }).Named("source").
		When(func(out Outcome) Action {
			if failed(out) {
				return Do(func() error { return branchFailure }).Named("selected")
			}
			return NoAction()
		}).
		When(func(out Outcome) Action {
			if failed(out) {
				return Do(func() { t.Fatal("sibling recovery ran") })
			}
			return NoAction()
		})
	err := f.Start()
	var failure *ActionError
	if !errors.Is(err, branchFailure) || errors.Is(err, original) || !errors.As(err, &failure) || failure.Name != "selected" || failure.Attempts != 1 {
		t.Fatalf("selected branch did not replace the error: %v", err)
	}
}

func TestWhen_NestedRecovery(t *testing.T) {
	original, cacheFailure := errors.New("service down"), errors.New("cache down")
	cache := Do(func(out Outcome) error {
		if out.Err != original {
			t.Fatalf("wrong original failure: %+v", out)
		}
		return cacheFailure
	}).Named("cache").When(func(out Outcome) Action {
		if failed(out) {
			return Do(func(out Outcome) string {
				if out.Err != cacheFailure || out.Name != "cache" {
					t.Fatalf("nested route saw wrong outcome: %+v", out)
				}
				return "default"
			})
		}
		return NoAction()
	})
	f := New()
	f.Do(func() error { return original }).When(func(out Outcome) Action {
		if failed(out) {
			return cache
		}
		return NoAction()
	})
	result, err := f.StartWithResult()
	if err != nil || !reflect.DeepEqual(result, []interface{}{"default"}) {
		t.Fatalf("result=%v error=%v", result, err)
	}
}

func TestWhen_RoutingFollowsRetriesRegardlessOfAttachmentOrder(t *testing.T) {
	for _, succeeds := range []bool{false, true} {
		t.Run(fmt.Sprint(succeeds), func(t *testing.T) {
			calls, checks := 0, 0
			failure := errors.New("failed")
			f := New()
			f.Do(func() (int, error) {
				calls++
				if succeeds && calls == 3 {
					return calls, nil
				}
				return calls, failure
			}).When(func(out Outcome) Action {
				checks++
				if calls != 3 || out.Attempts != 3 || out.Values[0] != 3 || (out.Err == nil) != succeeds {
					t.Fatalf("route did not see final retry outcome: %+v, calls=%d", out, calls)
				}
				return Do(func() string { return "routed" })
			}).With(&RetryPolicy{Attempts: 3})
			result, err := f.StartWithResult()
			if err != nil || checks != 1 || !reflect.DeepEqual(result, []interface{}{"routed"}) {
				t.Fatalf("result=%v error=%v checks=%d", result, err, checks)
			}
		})
	}
}

func TestWhen_BranchUsesItsOwnRetryPolicy(t *testing.T) {
	sourceCalls, branchCalls := 0, 0
	f := New()
	f.Do(func() error { sourceCalls++; return errors.New("source failure") }).
		When(func(out Outcome) Action {
			if failed(out) {
				return Do(func(out Outcome) error {
					branchCalls++
					if out.Attempts != 2 {
						t.Errorf("source attempt count changed: %+v", out)
					}
					if branchCalls < 3 {
						return errors.New("branch failure")
					}
					return nil
				}).With(&RetryPolicy{Attempts: 3})
			}
			return NoAction()
		}).With(&RetryPolicy{Attempts: 2})
	if err := f.Start(); err != nil || sourceCalls != 2 || branchCalls != 3 {
		t.Fatalf("error=%v source=%d branch=%d", err, sourceCalls, branchCalls)
	}
}

func TestWhen_ContextAndOutcomeInjection(t *testing.T) {
	type key struct{}
	ctx := context.WithValue(context.Background(), key{}, "parent")
	for _, explicit := range []bool{false, true} {
		t.Run(fmt.Sprint(explicit), func(t *testing.T) {
			params := []interface{}{1, 2}
			if explicit {
				params = append([]interface{}{ctx}, params...)
			}
			f := New().WithContext(ctx)
			f.Do(func() int { return 7 }).When(func(Outcome) Action {
				return Do(func(c context.Context, out Outcome, numbers ...int) int {
					if c.Value(key{}) != "parent" || out.Values[0] != 7 {
						t.Errorf("context or outcome changed: %+v", out)
					}
					return numbers[0] + numbers[1]
				}, params...)
			})
			result, err := f.StartWithResult()
			if err != nil || !reflect.DeepEqual(result, []interface{}{3}) {
				t.Fatalf("result=%v error=%v", result, err)
			}
		})
	}
}

func TestWhen_HooksObserveSourceAndEvaluatedBranches(t *testing.T) {
	var events []string
	f := New().WithHooks(Hooks{
		OnStart: func(name string) { events = append(events, "start:"+name) },
		OnError: func(name string, err error) {
			var failure *ActionError
			if !errors.As(err, &failure) || failure.Name != name {
				t.Errorf("error hook lost structured cause: %v", err)
			}
			events = append(events, "error:"+name)
		},
		OnSuccess: func(name string, _ []interface{}) { events = append(events, "success:"+name) },
		OnSkip:    func(name string) { events = append(events, "skip:"+name) },
	})
	f.Do(func() error { return errors.New("source failed") }).Named("source").
		When(func(out Outcome) Action {
			if out.Err == nil {
				return Do(func() {}).Named("success-branch")
			}
			return NoAction()
		}).
		When(func(out Outcome) Action {
			if failed(out) {
				return Do(func() {}).Named("recover")
			}
			return NoAction()
		}).
		When(func(Outcome) Action { t.Fatal("later rule evaluated"); return Do(func() {}).Named("later") })
	if err := f.Start(); err != nil {
		t.Fatal(err)
	}
	want := []string{"start:source", "error:source", "skip:source", "start:recover", "success:recover"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events=%v, want %v", events, want)
	}
}

func TestWhen_CallbackPanicPreservesOriginalFailure(t *testing.T) {
	original := errors.New("source failed")
	f := New()
	f.Do(func() error { return original }).Named("source").When(func(Outcome) Action { panic("bad callback") })
	err := f.Start()
	var failure *ActionError
	if !errors.Is(err, original) || !strings.Contains(err.Error(), "bad callback") || !errors.As(err, &failure) || failure.Name != "source" || failure.Attempts != 0 {
		t.Fatalf("callback failure lost context: %v", err)
	}
}

func TestWhen_BranchPanicIsAnError(t *testing.T) {
	f := New()
	f.Do(func() {}).When(func(Outcome) Action { return Do(func() { panic("branch panic") }).Named("branch") })
	err := f.Start()
	var failure *ActionError
	if !errors.As(err, &failure) || failure.Name != "branch" || failure.Attempts != 1 || !strings.Contains(err.Error(), "branch panic") {
		t.Fatalf("branch panic lost context: %v", err)
	}
}

func TestWhen_CancellationDoesNotRouteOrSkip(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := New().WithContext(ctx).WithHooks(Hooks{OnSkip: func(string) { t.Error("canceled route reported as skipped") }})
	f.Do(func() { cancel() }).When(func(Outcome) Action {
		t.Error("predicate ran after cancellation")
		return Do(func() { t.Error("branch ran after cancellation") })
	})
	if err := f.Start(); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation, got %v", err)
	}
}

func TestWhen_GroupRoutesAfterMembersComplete(t *testing.T) {
	cause := errors.New("member failure")
	f := New()
	f.AddParallel(Do(func() error { return cause }).Named("member"), Do(func() int { return 7 })).
		Named("group").WithConcurrency(2).
		When(func(out Outcome) Action {
			if errors.Is(out.Err, cause) {
				return Do(func(out Outcome) string {
					if out.Name != "group" || out.Attempts != 1 || !reflect.DeepEqual(out.Values, []interface{}{[]interface{}{7}}) {
						t.Errorf("wrong group outcome: %+v", out)
					}
					return "recovered-group"
				})
			}
			return NoAction()
		}).PipeAll()
	f.Do(func(value string) string { return value })
	result, err := f.StartWithResult()
	if err != nil || !reflect.DeepEqual(result, []interface{}{"recovered-group"}) {
		t.Fatalf("result=%v error=%v", result, err)
	}
}

func TestWhen_ActionRulesDoNotShareBackingArray(t *testing.T) {
	base := Do(func() {}).When(func(out Outcome) Action {
		if failed(out) {
			return Do(func() {})
		}
		return NoAction()
	})
	a := base.When(func(Outcome) Action { return Do(func() int { return 1 }) })
	b := base.When(func(Outcome) Action { return Do(func() int { return 2 }) })
	for i, action := range []Action{a, b} {
		f := New()
		f.AddAction(action)
		result, err := f.StartWithResult()
		if err != nil || !reflect.DeepEqual(result, []interface{}{i + 1}) {
			t.Fatalf("route definitions were shared: result=%v error=%v", result, err)
		}
	}
}

func TestWhen_BranchTimeoutUsesItsOwnPolicy(t *testing.T) {
	var calls atomic.Int32
	f := New()
	f.Do(func() int { return 1 }).When(func(Outcome) Action {
		return Do(func(ctx context.Context) error { calls.Add(1); <-ctx.Done(); return ctx.Err() }).
			Named("slow-branch").With(&TimeoutPolicy{PerAttempt: 10 * time.Millisecond})
	})
	err := f.Start()
	var failure *ActionError
	if !errors.Is(err, context.DeadlineExceeded) || !errors.As(err, &failure) || failure.Name != "slow-branch" || calls.Load() != 1 {
		t.Fatalf("error=%v calls=%d", err, calls.Load())
	}
}
