# flowm

[![Go Reference](https://pkg.go.dev/badge/github.com/bi0dread/flowm.svg)](https://pkg.go.dev/github.com/bi0dread/flowm)
[![Go Report Card](https://goreportcard.com/badge/github.com/bi0dread/flowm)](https://goreportcard.com/report/github.com/bi0dread/flowm)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

**flowm** is a lightweight, zero-dependency Go library for orchestrating sequences of function calls with composable resilience policies — retries, timeouts, circuit breakers, rate limits, fallbacks, and parallel execution — behind a fluent builder API.

```go
f := flowm.New().WithContext(ctx)
f.Do(fetchLive).With(retry).OnError(fetchCache).PipeAll()
f.Do(process)
err := f.Start()
```

## Contents

- [Why flowm](#why-flowm)
- [Installation](#installation)
- [Quick start](#quick-start)
- [Core concepts](#core-concepts)
- [Usage](#usage)
  - [Passing data between actions](#passing-data-between-actions)
  - [Getting results out](#getting-results-out)
  - [Error handling and fallbacks](#error-handling-and-fallbacks)
  - [Compensation (saga rollback)](#compensation-saga-rollback)
  - [Conditional actions](#conditional-actions)
  - [Retries](#retries)
  - [Timeouts](#timeouts)
  - [Circuit breaker](#circuit-breaker)
  - [Rate limiting](#rate-limiting)
  - [Parallel execution](#parallel-execution)
  - [ForEach fan-out](#foreach-fan-out)
  - [Subflows](#subflows)
  - [Observability hooks](#observability-hooks)
  - [Context propagation](#context-propagation)
  - [Custom policies](#custom-policies)
- [API reference](#api-reference)
- [Behavior notes](#behavior-notes)
- [Testing](#testing)
- [License](#license)

## Why flowm

- **One execution path, many policies.** Every attempt — first call, retry, or parallel branch — runs through the same pipeline: pre-execution checks, context injection, timeouts, and panic recovery. Policies compose correctly because they all see the same path.
- **Catch-style fallbacks and saga rollback.** Chain `OnError` handlers like nested `catch` blocks, and register `Compensate` functions that undo completed steps when a later one fails.
- **Zero dependencies.** Single package, standard library only.

## Installation

```bash
go get github.com/bi0dread/flowm
```

Requires Go 1.23+.

## Quick start

```go
package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/bi0dread/flowm"
)

func fetchUser(ctx context.Context, id int) (string, error) {
	return fmt.Sprintf("user-%d", id), nil // ctx is injected by the flow
}

func greet(name string) string { return "Hello, " + name }

func main() {
	retry := &flowm.RetryPolicy{
		Attempts:      3, // total attempts, including the first call
		Delay:         200 * time.Millisecond,
		BackoffFactor: 2.0,
		Jitter:        true,
	}

	f := flowm.New().WithContext(context.Background())
	f.Do(fetchUser, 42).With(retry).PipeAll() // outputs become the next action's inputs
	f.Do(greet).PipeAll()
	f.Do(func(msg string) { fmt.Println(msg) })

	if err := f.Start(); err != nil {
		log.Fatal(err)
	}
	// Output: Hello, user-42
}
```

## Core concepts

| Concept | What it is |
|---|---|
| **Flow** | An ordered list of actions, executed sequentially by `Start()`. Stops at the first unhandled error. |
| **Action** | A function plus its parameters and attached policies. Any signature works; calls go through reflection. |
| **Policy** | A behavior attached to an action: retry, timeout, fallback, rate limit, … Policies are plain interfaces, so you can write your own. |
| **Piping** | Opt-in forwarding of an action's return values into the next action's parameters. |

Every attempt of every action runs through the same execution path: pre-execution policy checks → context injection → the call (bounded by a per-attempt timeout if configured) → panic recovery. Retries and fallbacks reuse this path, so all policies apply consistently.

## Usage

### Passing data between actions

By default an action's return values are discarded. Three chainable methods forward them to the **next** action:

```go
f.Do(produce).PipeAll()   // all non-error return values
f.Do(produce).Pipe(0, 2)  // only return values at indices 0 and 2
f.Do(produce).Map(func(vals []interface{}) []interface{} { // transform first
	return []interface{}{fmt.Sprintf("%v-%v", vals[0], vals[1])}
})
```

Rules:

- Error-typed return values are filtered out — `func() (string, error)` pipes only the string.
- Piped values are appended **after** the next action's explicit params.
- If an action returns exactly one `[]interface{}`, it is expanded into individual values (this is how parallel groups spread their merged results).
- Piping is consumed by exactly one action; chain `PipeAll()` again to keep forwarding.

### Getting results out

`StartWithResult` returns the last executed action's non-error return values:

```go
f := flowm.New()
f.Do(compute)
results, err := f.StartWithResult() // []interface{} of compute's outputs
```

### Error handling and fallbacks

By default the first error aborts the flow and `Start()` returns it.

```go
// Swallow the error and keep going (the error is discarded):
f.Do(risky).ContinueOnError()

// Catch-style fallback chain — each handler runs only if everything before it failed:
f.Do(fetchLive).
	OnError(fetchCache).   // receives fetchLive's error
	OnError(fetchDefault). // receives fetchCache's error
	PipeAll()              // whichever succeeded, its values pipe onward
f.Do(process)
```

Fallback semantics:

- A fallback receives the previous error if it declares a leading `error` parameter (after an optional `context.Context`): `func(err error)`, `func(ctx context.Context, err error) (string, error)`, ….
- If a fallback succeeds, the flow recovers and the fallback's return values **replace** the failed action's outputs for piping.
- If the last fallback also fails, its error is what `Start()` returns.
- Combine with retry: `.With(retry).OnError(fb)` runs the fallback only after retries are exhausted.
- Errors don't survive piping (see above) — to forward error details downstream, convert them to a value: `func(err error) (string, string) { return "fallback", err.Error() }`.

To mark an error as non-retryable, wrap it with `flowm.Permanent(err)` (or implement the `DoNotRetry` interface). `RetryPolicy` stops retrying when it sees one — even wrapped, via `errors.As` — and treats the action as handled. `errors.Is`/`errors.As` keep working through the wrapper.

### Compensation (saga rollback)

Register an undo function per action; when a **later** action fails terminally, compensations of all previously succeeded actions run in **reverse order**:

```go
f := flowm.New()
f.Do(createOrder).Compensate(cancelOrder)
f.Do(chargeCard).Compensate(refundCard)
f.Do(shipOrder) // fails → refundCard runs, then cancelOrder
err := f.Start() // the original failure; compensation errors are errors.Join-ed onto it
```

- Only actions that **succeeded** are compensated — the failed action itself is not.
- Compensations receive context injection like any function; a panicking compensation is recovered and reported.
- Compensations are tracked for top-level flow actions (not individual parallel members).

### Conditional actions

`When` skips an action unless a predicate over its incoming piped values returns true. Skipped actions are transparent: piped values stay available for the next action.

```go
f.Do(fetchScore).PipeAll()
f.Do(sendAlert).When(func(piped []interface{}) bool {
	return piped[0].(int) > 90 // alert only on high scores
})
f.Do(logScore) // receives the piped score whether or not the alert ran
```

### Retries

```go
retry := &flowm.RetryPolicy{
	Attempts:      5,                       // total attempts INCLUDING the initial call
	Delay:         100 * time.Millisecond,  // base delay between attempts
	BackoffFactor: 2.0,                     // exponential backoff multiplier
	MaxDelay:      2 * time.Second,         // backoff ceiling
	Jitter:        true,                    // randomize each delay by ±50%
	RetryIf:       func(err error) bool { return isTransient(err) },
	Fn:            func() error { return nil }, // pre-check before each retry; error = stop retrying
	MaxElapsed:    10 * time.Second,        // total time budget for all retries
}
f.Do(callAPI).With(retry)
```

Behavior:

- Retries run through the full execution path: context injection, per-attempt `TimeoutPolicy`, pre-execution policies (rate limit, circuit breaker), and panic recovery apply to **every** attempt.
- A successful retry's return values are the ones piped to the next action.
- If `Fn` returns an error, retrying stops and the last call error is returned.
- `RetryIf` returning false stops further retries.
- `Permanent`/`DoNotRetry` errors stop retrying immediately — on the initial call or mid-retry.
- The `OnRetry` hook (see [hooks](#observability-hooks)) fires on every retry attempt.

### Timeouts

```go
f.Do(slowCall).With(&flowm.TimeoutPolicy{
	PerAttempt: 500 * time.Millisecond, // bounds each attempt (including each retry)
	Overall:    5 * time.Second,        // total budget across all attempts of this action
})
```

On expiry an attempt fails with `context.DeadlineExceeded`; once the `Overall` budget is spent, remaining retry attempts fail immediately. The flow's context cancels waits as well.

> Note: the underlying goroutine is not forcibly stopped — pass a context and honor it in your function for cooperative cancellation.

### Circuit breaker

```go
cb := &flowm.CircuitBreakerPolicy{
	Failures: 5,                // open after 5 failures...
	Window:   time.Minute,      // ...within this window
	Cooldown: 30 * time.Second, // stay open for this long
}
f.Do(callService).With(cb)
```

While open, attempts (including retries) fail immediately with `circuit breaker open` — the function is not called. Every executed attempt counts toward the threshold, retries included; rejected attempts do not. Share one instance across actions to share the breaker state.

### Rate limiting

Token-bucket limiter; `Per` is the time to generate one token, `Burst` the bucket size:

```go
rl := &flowm.RateLimitPolicy{Per: time.Second / 5, Burst: 2} // ~5 calls/s, burst of 2

// Shared instance = shared bucket (global limit across these actions):
f.Do(callA).With(rl)
f.Do(callB).With(rl)
```

- For N calls/sec: `Per = time.Second / N`. Ensure `Per > 0` and `Burst >= 1`.
- Separate instances rate-limit independently; one shared instance enforces a global limit.
- Waiting respects the flow context (`WithContext`) and counts against a `TimeoutPolicy{PerAttempt}` if one is attached.
- The limiter is in-process and in-memory; for cross-process fairness use a centralized limiter.

### Parallel execution

`AddParallel` runs a group of actions concurrently as a single flow step. Build the actions with the package-level `flowm.Do` — same signature as the method:

```go
f.AddParallel(
	flowm.Do(fetchName),
	flowm.Do(fetchRole).With(rateLimiter),
	flowm.Do(fetchTeam, teamID).OnError(fetchTeamFromCache),
).PipeAll()
f.Do(func(name, role, team string) { ... })
```

- Results are flattened in **declaration order** (not completion order) and can be piped.
- Per-action policies — including `OnError` fallbacks — apply inside the group.
- Cap concurrency with `flowm.WorkerPoolPolicy{Size: 4}` on any member action.
- If members fail, their errors are collected into a single `MultiError`; `errors.Is`/`errors.As` see through it to the individual errors.
- `NewAction(fn, []interface{}{...}, policies...)` is the equivalent non-chaining constructor.

### ForEach fan-out

`ForEach` runs a function once per piped element, concurrently, and merges the results in element order for piping onward. A single piped slice is expanded into its elements:

```go
f.Do(listUserIDs).PipeAll()                  // returns []int
f.ForEach(fetchUser, &flowm.WorkerPoolPolicy{Size: 4}).PipeAll()
f.Do(renderUsers)                            // receives one value per user
```

Policies passed to `ForEach` apply per element (retry, rate limit, worker pool, …). An empty piped collection is a no-op.

### Subflows

`DoFlow` embeds a whole flow as a single action of another. The subflow runs with the parent's context, and its final results become the step's pipeable outputs:

```go
enrichment := flowm.New()
enrichment.Do(fetchProfile).With(retry).PipeAll()
enrichment.Do(mergeProfile)

f := flowm.New().WithContext(ctx)
f.DoFlow(enrichment).PipeAll()
f.Do(save)
```

This lets you build a library of reusable, pre-policied flows. A subflow instance holds state (context), so don't share one across concurrently running parents.

### Observability hooks

```go
f := flowm.New().WithHooks(flowm.Hooks{
	OnStart:   func(name string) {},
	OnSuccess: func(name string, result []interface{}) {},
	OnError:   func(name string, err error) {},
	OnRetry:   func(name string, attempt int, err error) {},
})
```

Hooks observe every action in the flow (`name` is the function name). They cannot change outcomes — use policies for that. `OnSuccess` is also the way to read a flow's final results, since `Start()` returns only an error.

### Context propagation

```go
ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
defer cancel()
f := flowm.New().WithContext(ctx)
```

- If a function's first parameter is `context.Context` and you supplied one fewer argument, the flow's context is injected automatically — on the initial call and on every retry.
- Cancellation is honored while waiting on retry delays, rate-limit tokens, and per-attempt timeouts.

### Custom policies

Any type implementing `Policy` can be attached with `.With(...)`:

```go
type Policy interface {
	Do(action Action, err error) error // runs after every attempt cycle
}
```

Return `nil` to swallow the error (recover the flow), return a different error to replace it:

```go
type MetricsPolicy struct{ client *statsd.Client }

func (m *MetricsPolicy) Do(a flowm.Action, err error) error {
	if err != nil {
		m.client.Incr("flow.action.failure", nil, 1)
	}
	return err
}
```

Optional capability interfaces — implement any of these and the flow wires them up automatically:

| Interface | Implement to… | Used by |
|---|---|---|
| `PreExecutionPolicy` | veto/gate before each attempt | circuit breaker, rate limiter |
| `ContextAwarePolicy` | receive the flow's context | retry |
| `RetryHookAwarePolicy` | receive the `OnRetry` hook | retry |
| `ExecutorAwarePolicy` | re-run the action through the full execution path | retry |
| `FallbackAwarePolicy` | invoke alternative functions and replace the piped result | fallback |
| `AttemptObserverPolicy` | observe the outcome of every executed attempt | circuit breaker |

## API reference

```go
type Flowm interface {
	// building
	Do(function interface{}, params ...interface{}) Flowm
	AddAction(action Action)
	AddParallel(actions ...Action) Flowm
	DoFlow(sub Flowm) Flowm
	ForEach(function interface{}, policies ...Policy) Flowm

	// configuring the last added action
	With(policy Policy) Flowm
	PipeAll() Flowm
	Pipe(indices ...int) Flowm
	Map(mapper func([]interface{}) []interface{}) Flowm
	ContinueOnError() Flowm
	OnError(function interface{}, params ...interface{}) Flowm
	When(pred func(piped []interface{}) bool) Flowm
	Compensate(function interface{}, params ...interface{}) Flowm

	// configuring the flow
	WithContext(ctx context.Context) Flowm
	WithHooks(h Hooks) Flowm

	// running
	Start() error
	StartWithResult() ([]interface{}, error)
}

// Action builders
func NewAction(function interface{}, params []interface{}, policies ...Policy) Action
func Do(function interface{}, params ...interface{}) Action // chainable: .With, .OnError, .Compensate

// Errors
func Permanent(err error) error // mark non-retryable; unwrappable via errors.Is/As
```

Built-in policies:

| Policy | Effect |
|---|---|
| `RetryPolicy{Attempts, Delay, BackoffFactor, MaxDelay, Jitter, RetryIf, Fn, MaxElapsed}` | retry failed attempts with backoff |
| `TimeoutPolicy{PerAttempt}` | bound each attempt's duration |
| `CircuitBreakerPolicy{Failures, Window, Cooldown}` | fail fast after repeated failures |
| `RateLimitPolicy{Per, Burst}` | token-bucket throttling |
| `FallbackPolicy{Func, Params}` | run an alternative function on failure (sugar: `OnError`) |
| `ContinueOnErrorPolicy{Continue: true}` | swallow errors, keep the flow going (sugar: `ContinueOnError`) |
| `ConditionPolicy{If}` | skip the action unless the predicate passes (sugar: `When`) |
| `CompensatePolicy{Func, Params}` | rollback function for saga-style flows (sugar: `Compensate`) |
| `WorkerPoolPolicy{Size}` | cap concurrency inside `AddParallel` and `ForEach` |
| `PipeAllPolicy` / `PipeIndicesPolicy` / `PipeMapPolicy` | piping (sugar: `PipeAll` / `Pipe` / `Map`) |

Deprecated, kept for backward compatibility: `TerminatePolicy` (behaves like continue-on-error), `PassValuesPolicy` (behaves like `PipeAllPolicy`).

## Behavior notes

- **Policy order matters.** Policies run in attachment order: `.With(retry).OnError(fb)` retries first and falls back only when retries are exhausted; reversed, the fallback would run before the retry sees the error.
- **Shared policy instances share state.** One `RateLimitPolicy` across actions = one global bucket; one `CircuitBreakerPolicy` = one shared breaker. Use separate instances for independent behavior.
- **Reflection trade-off.** Parameter types and counts are checked at runtime, not compile time. A mismatch (or an untyped `nil` argument) surfaces as an error on `Start()`.
- **Panics become errors.** A panic in an action, fallback, or compensation is recovered and treated as a normal error — it can be retried, caught by `OnError`, or continued past.
- **`Permanent`/`DoNotRetry` errors are swallowed.** When retrying stops on a non-retryable error, `RetryPolicy` treats the action as handled and the flow continues without an error — attach `OnError` if you need to react to it.
- **Results come from `StartWithResult`, piping, or hooks.** `Start()` returns only `error`; use `StartWithResult()`, pipe into a consumer function, or the `OnSuccess` hook.

## Testing

```bash
go test -race ./...
```

## License

MIT
