package flowm_test

import (
	"errors"
	"fmt"

	"github.com/bi0dread/flowm"
)

func Example_outcomeRouting() {
	f := flowm.New().WithHooks(flowm.Hooks{
		OnSkip: func(name string) { fmt.Println("skipped:", name) },
	})
	f.Do(func() int { return 50 }).Named("fetch-score").When(
		func(out flowm.Outcome) flowm.Action {
			if out.Err == nil && out.Values[0].(int) > 90 {
				return flowm.Do(func(out flowm.Outcome) int {
					score := out.Values[0].(int)
					fmt.Println("alert:", score)
					return score
				}).Named("high-score-alert")
			}
			return flowm.NoAction()
		},
	).PipeAll()
	f.Do(func(score int) { fmt.Println("score:", score) })
	if err := f.Start(); err != nil {
		panic(err)
	}
	// Output:
	// skipped: fetch-score
	// score: 50
}

func ExampleJump() {
	f := flowm.New().WithMaxJumps(10)
	f.Do(func() int { return 0 }).PipeAll()
	f.Do(func(n int) int { return n + 1 }).Named("increment").When(
		func(out flowm.Outcome) flowm.Action {
			if out.Err == nil && out.Values[0].(int) < 3 {
				return flowm.Jump("increment")
			}
			return flowm.NoAction()
		},
	).PipeAll()
	f.Do(func(n int) { fmt.Println(n) })
	if err := f.Start(); err != nil {
		panic(err)
	}
	// Output: 3
}

func Example_validation() {
	f := flowm.New()
	f.Do(func() { fmt.Println("side effect") })
	f.Do(func(int) {}, "wrong type").Named("save-count")
	var failure *flowm.ActionError
	if err := f.Validate(); errors.As(err, &failure) {
		fmt.Printf("invalid: %s, attempts: %d\n", failure.Name, failure.Attempts)
	}
	// Output: invalid: save-count, attempts: 0
}

func ExampleActionError() {
	unavailable := errors.New("unavailable")
	f := flowm.New()
	f.Do(func() error { return unavailable }).Named("fetch-user").
		With(&flowm.RetryPolicy{Attempts: 3})
	err := f.Start()
	var failure *flowm.ActionError
	if errors.As(err, &failure) {
		fmt.Printf("%s failed after %d attempts\n", failure.Name, failure.Attempts)
	}
	fmt.Println("original cause:", errors.Is(err, unavailable))
	// Output:
	// fetch-user failed after 3 attempts
	// original cause: true
}

func Example_parallelLimit() {
	f := flowm.New()
	f.AddParallel(
		flowm.Do(func() string { return "profile" }).Named("profile"),
		flowm.Do(func() string { return "permissions" }).Named("permissions"),
	).Named("load-user").WithConcurrency(2).PipeAll()
	f.Do(func(first, second string) { fmt.Println(first, second) })
	if err := f.Start(); err != nil {
		panic(err)
	}
	// Output: profile permissions
}
