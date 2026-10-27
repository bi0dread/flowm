# flowm

[![Go Reference](https://pkg.go.dev/badge/github.com/bi0dread/flowm.svg)](https://pkg.go.dev/github.com/bi0dread/flowm)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

Build Go workflows from ordinary functions. Run steps in order, choose what happens after each result, and add retries, timeouts, or parallel execution.

**Go 1.23+ · Standard library only**

[Quick start](#quick-start) · [Decisions](#choose-what-happens-next) · [Jumps](#jump-to-a-named-step) · [Usage guide](docs/guide.md) · [Migration](docs/migration.md)

## Install

```sh
go get github.com/bi0dread/flowm
```

## Quick start

```go
package main

import (
	"fmt"
	"log"

	"github.com/bi0dread/flowm"
)

func main() {
	f := flowm.New()
	f.Do(func(name string) string {
		return "Hello, " + name
	}, "Go").PipeAll()
	f.Do(func(message string) {
		fmt.Println(message)
	})

	if err := f.Start(); err != nil {
		log.Fatal(err)
	}
}
// Output: Hello, Go
```

- `Do` adds a step. Steps run in order and stop on an unhandled error.
- `PipeAll()` passes a step's non-error return values to the next step.
- `Start()` runs the flow. Use `StartWithResult()` to also get the last executed action's return values.

`When` is optional. A flow can use only `Do`.

## Choose what happens next

`When` takes one callback. It receives the completed action's `Outcome` after retries and policies finish, then returns a decision:

| Return | What happens |
|---|---|
| `flowm.Do(fn, args...)` | Run another action; its result replaces the source result. |
| `flowm.Jump("name")` | Continue from a named step, retaining source values and handling its error. |
| `flowm.NoAction()` | Keep the original values and error. |

For example, recover a failed request with a cached value:

```go
f := flowm.New()
f.Do(func() (string, error) {
	return "", errors.New("service unavailable")
}).When(func(out flowm.Outcome) flowm.Action {
	if out.Err != nil {
		return flowm.Do(func() string { return "cached value" })
	}
	return flowm.NoAction()
}).PipeAll()
f.Do(func(value string) { fmt.Println(value) })

if err := f.Start(); err != nil {
	log.Fatal(err)
}
// Output: cached value
```

The callback can inspect `out.Err`, `out.Values`, `out.Name`, and `out.Attempts`. `NoAction()` preserves errors: the flow still stops unless a later callback handles the failure.

With multiple `When` callbacks, the first decision other than `NoAction()` wins. Returned actions can have their own policies and nested `When` callbacks. See [decision behavior](docs/guide.md#outcome-routing).

## Jump to a named step

Name a step with `Named`, then return `Jump` from a callback. Jumps can move backward, forward, or back to the current step:

```go
f := flowm.New().WithMaxJumps(10)
f.Do(func() int { return 0 }).PipeAll()
f.Do(func(n int) int { return n + 1 }).Named("increment").
	When(func(out flowm.Outcome) flowm.Action {
		if out.Err == nil && out.Values[0].(int) < 3 {
			return flowm.Jump("increment")
		}
		return flowm.NoAction()
	}).PipeAll()
f.Do(func(n int) { fmt.Println(n) })

if err := f.Start(); err != nil {
	log.Fatal(err)
}
// Output: 3
```

A target must be a uniquely named top-level step. Source piping sends values to that target; previous inputs are never replayed automatically.

Each run allows **1,000 jumps by default**. Set a positive limit with `WithMaxJumps(n)`. Exceeding it returns `ErrJumpLimitExceeded`; context cancellation also stops the loop. Parallel groups may jump after completion, but individual workers cannot. See [jump behavior](docs/guide.md#jumping-to-named-steps).

## Add retries and timeouts

Attach policies to the action they should affect:

```go
f.Do(fetchUser, 42).
	With(&flowm.RetryPolicy{
		Attempts: 3,
		Delay:    100 * time.Millisecond,
	}).
	With(&flowm.TimeoutPolicy{
		PerAttempt: time.Second,
		Overall:    5 * time.Second,
	})
```

`Attempts` includes the first call. Each retry gets the same policy checks and its own attempt deadline. Returned actions use their own policies.

Use `WithContext(ctx)` for flow cancellation. Functions with a leading `context.Context` parameter receive the execution context automatically; they must honor it to stop underlying work when a timeout expires.

[Retries](docs/guide.md#retries), [timeouts](docs/guide.md#timeouts), [circuit breakers](docs/guide.md#circuit-breaker), and [rate limiting](docs/guide.md#rate-limiting) are covered in the usage guide.

## Run actions in parallel

Use `flowm.Do` to build actions for a group. Limit active workers with `WithConcurrency`:

```go
f := flowm.New()
f.AddParallel(
	flowm.Do(func() string { return "profile" }),
	flowm.Do(func() string { return "permissions" }),
).WithConcurrency(2).PipeAll()
f.Do(func(profile, permissions string) {
	fmt.Println(profile, permissions)
})

if err := f.Start(); err != nil {
	log.Fatal(err)
}
// Output: profile permissions
```

Results follow declaration order. Member errors are combined and remain inspectable with `errors.Is` and `errors.As`. See [parallel execution](docs/guide.md#parallel-execution).

## Reference

| API | Purpose |
|---|---|
| `f.Do(fn, args...)` | Append a step to a flow. |
| `flowm.Do(fn, args...)` | Build an `Action` to return from `When` or use in a parallel group. |
| `Named(name)` | Name a step for errors, hooks, and jumps. |
| `PipeAll()` / `Pipe(indices...)` / `Map(fn)` | Pass all, selected, or transformed values to the next executed step. |
| `With(policy)` | Attach a policy to the current action. |
| `WithHooks(hooks)` | Observe starts, successes, errors, retries, and skipped decisions. |
| `Validate()` | Check registered definitions without running them. |
| `Start()` / `StartWithResult()` | Run the flow, optionally returning final values. |

Registered definitions are validated before execution. Actions and jump targets chosen inside `When`, along with dynamic piped arguments, are validated at runtime. Function arguments are checked using reflection.

Errors carry an `ActionError` with the action name, attempt count, and underlying cause. Use `errors.Is` and `errors.As` to inspect them.

For details, see the [usage guide](docs/guide.md), [API reference](docs/guide.md#api-reference), [executable examples](example_test.go), and [migration guide](docs/migration.md).

## Development

```sh
go test -race ./...
go vet ./...
```

## License

[MIT](LICENSE)
