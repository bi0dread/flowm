# flowm

[![Go Reference](https://pkg.go.dev/badge/github.com/bi0dread/flowm.svg)](https://pkg.go.dev/github.com/bi0dread/flowm)
[![Go Report Card](https://goreportcard.com/badge/github.com/bi0dread/flowm)](https://goreportcard.com/report/github.com/bi0dread/flowm)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

**flowm** is a lightweight, zero-dependency Go library for orchestrating sequences of function calls with composable resilience policies — retries, timeouts, circuit breakers, rate limits, outcome routing, and parallel execution — behind a fluent builder API.

```go
f := flowm.New().WithContext(ctx)
f.Do(fetchLive).With(retry).When(
	func(out flowm.Outcome) bool { return out.Err != nil },
	flowm.Do(fetchCache),
).PipeAll()
f.Do(process)
err := f.Start()
```

## Contents

- [Why flowm](#why-flowm)
- [Installation](#installation)
- [Quick start](#quick-start)
- [Core concepts](#core-concepts)
- [Usage](#usage)
  - [Validation](#validation)
  - [Action names and errors](#action-names-and-errors)
  - [Passing data between actions](#passing-data-between-actions)
  - [Getting results out](#getting-results-out)
  - [Outcome routing](#outcome-routing)
  - [Retries](#retries)
  - [Timeouts](#timeouts)
  - [Circuit breaker](#circuit-breaker)
  - [Rate limiting](#rate-limiting)
  - [Parallel execution](#parallel-execution)
  - [Observability hooks](#observability-hooks)
  - [Context propagation](#context-propagation)
  - [Custom policies](#custom-policies)
- [API reference](#api-reference)
- [Behavior notes](#behavior-notes)
- [Migration from the previous API](#migration-from-the-previous-api)
- [Testing](#testing)
- [License](#license)

## Why flowm

- **One execution path, many policies.** Every attempt — first call, retry, or parallel branch — runs through the same pipeline: pre-execution checks, context injection, timeouts, and panic recovery. Policies compose correctly because they all see the same path.
- **Explicit outcome routing.** Use `When` to choose the next action after success or failure. Unmatched errors reach the caller unless explicitly ignored.
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
| **Action** | A function plus its parameters and attached policies. Calls go through reflection. |
| **Policy** | A behavior attached to an action: retry, timeout, rate limit, … Policies are plain interfaces, so you can write your own. |
| **Piping** | Opt-in forwarding of an action's return values into the next action's parameters. |

Every attempt of every action runs through the same execution path: pre-execution policy checks → context injection → the call (bounded by a per-attempt timeout if configured) → panic recovery. Retries reuse this path. After the action and its policies finish, `When` rules inspect its final outcome. A selected branch runs through the same execution path with its own policies.

## Usage

### Validation

`Start()` and `StartWithResult()` validate the whole flow before executing any action. You can check a flow separately without running it:

```go
if err := f.Validate(); err != nil {
	log.Fatal(err)
}
```

Validation checks non-nil functions and policies, explicit argument types and counts, non-nil route predicates, branch definitions, and built-in policy settings. It also checks every parallel member and every nested branch, including branches that will not be selected. Invalid later steps prevent earlier steps from causing side effects. Validation does not invoke actions, predicates, branches, maps, custom policies, or hooks.

Piped values can change through mapping and outcome routing. Their exact types and counts are checked before the receiving action or its policies run. Predicates have the fixed signature `func(Outcome) bool`; branch parameters can optionally request the source `Outcome`. This is runtime validation, not compile-time type safety. Custom policy configuration remains the policy author's responsibility.

Negative durations and retry counts are rejected. Rate limits require `Per > 0` and `Burst >= 1`; circuit breakers require positive thresholds and durations. A concurrency limit must be positive and belong to a parallel group. Duplicate timeout policies and concurrency settings on one action are rejected. Multiple `When` rules are allowed and evaluated in registration order. Zero retry attempts retain the existing behavior of making only the initial call.

### Action names and errors

Name steps and parallel members so logs and failures identify meaningful operations:

```go
f.Do(fetchUser, userID).Named("fetch-user").With(retry)
f.AddParallel(
	flowm.Do(fetchProfile).Named("profile"),
	flowm.Do(fetchPermissions).Named("permissions"),
).Named("load-details").WithConcurrency(2)
```

An empty name uses the function name; unnamed parallel groups use `parallel`. `Action{Name: "...", Func: fn}` is also supported.

Failures returned by the flow carry an `*ActionError`:

```go
err := f.Start()
var failure *flowm.ActionError
if errors.As(err, &failure) {
	log.Printf("action=%s attempts=%d cause=%v", failure.Name, failure.Attempts, failure.Err)
}
if errors.Is(err, context.DeadlineExceeded) {
	// Handle the underlying cause without matching error text.
}
```

`Attempts` counts actual calls to that action, including retries. Each selected branch has its own name and attempt count. Validation and predicate failures, cancellation before execution, and attempts rejected by pre-execution policies count as zero calls. A failed parallel group wraps joined, individually named member errors, preserving their causes through `errors.Is` and `errors.As`. If a branch fails, the returned error identifies that branch. `Outcome.Err` contains the underlying error; error hooks and the flow's returned errors add the `ActionError` wrapper.

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

### Outcome routing

`When(predicate, action)` runs **after the action it is attached to**, after all retries and policies have finished. Each predicate receives the completed outcome:

```go
type Outcome struct {
	Name     string
	Values   []interface{} // non-error return values from the final attempt
	Err      error         // nil on success
	Attempts int           // calls made to this action, including retries
}
```

Register ordered rules for success and failure:

```go
f.Do(fetchUser, userID).Named("fetch-user").With(retry).
	When(func(out flowm.Outcome) bool {
		return out.Err == nil && out.Values[0].(User).Active
	}, flowm.Do(func(out flowm.Outcome) error {
		return sendWelcome(out.Values[0].(User))
	}).Named("send-welcome")).
	When(func(out flowm.Outcome) bool {
		return errors.Is(out.Err, ErrServiceUnavailable)
	}, flowm.Do(loadCachedUser, userID).Named("load-cache"))
```

The rules are:

- The source action always runs first. Routing happens once, after its final attempt, regardless of where `When` appears relative to `With(retry)` in the builder chain.
- Only the **first matching rule** runs. Later predicates and branches are not evaluated.
- If no rule matches, a successful source continues with its original values; a failed source stops the flow with its original error.
- The selected branch's values and error replace the source outcome. A successful error branch recovers the flow. A failing branch propagates its own failure; the engine does not try sibling rules afterward.
- A branch may declare a leading `Outcome` parameter after an optional `context.Context`. These parameters are injected. All other branch arguments are explicit arguments supplied to `Do`; source values are not appended implicitly. Branches that do not request `Outcome` can be ordinary functions with only explicit arguments.
- Branches have their own names, retry/timeout policies, and nested `When` rules. They use the parent flow context and do not inherit the source action's policies or its expired per-action deadline.
- `PipeAll`, `Pipe`, and `Map` on the source step apply to the final selected outcome. Branches replace values automatically; piping policies belong on the source step, not on branch actions. A branch returning no values clears the source values.
- The flow's cancellation stops routing. A timeout on the source action can still be recovered if the parent flow context remains active.
- Treat outcomes as read-only in predicates. A predicate panic aborts routing, preserves the source failure when present, and is reported as an error for the target branch with zero attempts.

Use nested rules for a recovery chain:

```go
failed := func(out flowm.Outcome) bool { return out.Err != nil }

cache := flowm.Do(fetchCache).Named("cache").
	When(failed, flowm.Do(fetchDefault).Named("default"))

f.Do(fetchLive).With(retry).When(failed, cache).PipeAll()
f.Do(process) // receives live, cached, or default results
```

Rules work on ordinary actions, members of `AddParallel`, and completed parallel groups. Group outcomes contain the group's returned merged slice as one value in `Outcome.Values`, matching `StartWithResult`. Group routing occurs after the members finish, or after the group's own deadline expires.

`Permanent(err)` and `DoNotRetry` stop retries but remain failures that can be handled by an error-matching rule. To explicitly ignore an error and continue, select a branch that succeeds:

```go
f.Do(sendNotification).When(
	func(out flowm.Outcome) bool { return out.Err != nil },
	flowm.Do(func() {}),
)
f.Do(nextTask)
```

An empty recovery branch clears the source's returned values. If the next task needs values from the failed action, have the branch receive `Outcome` and return the specific values to keep. An unmatched error still stops the flow; a selected branch's failure requires its own nested recovery rule.

### Retries

```go
retry := &flowm.RetryPolicy{
	Attempts:      5,                       // total attempts INCLUDING the initial call
	Delay:         100 * time.Millisecond,  // base delay between attempts
	BackoffFactor: 2.0,                     // exponential backoff multiplier
	MaxDelay:      2 * time.Second,         // backoff ceiling
	Jitter:        true,                    // randomize each delay by ±50%
	RetryIf:       func(err error) bool { return isTransient(err) },
}
f.Do(callAPI).With(retry)
```

Behavior:

- Retries run through the full execution path: context injection, per-attempt `TimeoutPolicy`, pre-execution policies (rate limit, circuit breaker), and panic recovery apply to **every** attempt.
- A successful retry's return values are the ones piped to the next action.
- `RetryIf` is checked before every retry, including the first. Returning false preserves the last error.
- `Delay` applies before the first retry; `BackoffFactor` increases later delays. `MaxDelay` caps the wait, including jitter.
- Use `TimeoutPolicy.Overall` for an action budget or `WithContext` for a flow budget.
- `Permanent`/`DoNotRetry` errors stop retrying immediately — on the initial call or mid-retry.
- The `OnRetry` hook (see [hooks](#observability-hooks)) fires on every retry attempt.

### Timeouts

```go
f.Do(slowCall).With(&flowm.TimeoutPolicy{
	PerAttempt: 500 * time.Millisecond, // bounds each attempt (including each retry)
	Overall:    5 * time.Second,        // total budget across all attempts of this action
})
```

The attempt deadline is injected into context-aware functions and pre-execution policies. On expiry an attempt fails with `context.DeadlineExceeded`. `Overall` covers attempts, rate-limit waits, and retry delays; exhaustion stops further attempts. Selected branches use the parent flow context so they can still recover from an action timeout.

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
	flowm.Do(fetchName).Named("name"),
	flowm.Do(fetchRole).Named("role").With(rateLimiter),
	flowm.Do(fetchTeam, teamID).Named("team").When(
		func(out flowm.Outcome) bool { return out.Err != nil },
		flowm.Do(fetchTeamFromCache, teamID),
	),
).Named("fetch-details").WithConcurrency(2).PipeAll()
f.Do(func(name, role, team string) { ... })
```

- Results are flattened in **declaration order** (not completion order) and can be piped.
- Each member runs its own policies and outcome routes inside its worker slot.
- `WithConcurrency(n)` sets a positive limit on the group. It creates at most `n` workers and limits the number of active member executions. Without a limit, all members may run concurrently. Timed-out functions must honor their contexts to stop their underlying work.
- Legacy `WorkerPoolPolicy` values on members still work, but a group limit takes precedence. Conflicting member limits without a group limit fail validation; placing the limit on the group is preferred.
- Cancellation prevents queued members from starting. Empty groups succeed with no member results.
- If members fail, their named `ActionError` values are combined with `errors.Join` and wrapped in the group's `ActionError`; `errors.Is`/`errors.As` retain access to underlying failures.

### Observability hooks

```go
f := flowm.New().WithHooks(flowm.Hooks{
	OnStart:   func(name string) {},
	OnSkip:    func(name string) {},
	OnSuccess: func(name string, result []interface{}) {},
	OnError:   func(name string, err error) {},
	OnRetry:   func(name string, attempt int, err error) {},
})
```

Hooks use the explicit action name when provided, otherwise the function name. Each source emits its success or error hook before routing; a selected branch emits its own hooks. A recovered source failure therefore remains observable. `OnSkip` names the branch whose predicate returned false. Rules after the first match are not evaluated and emit no hooks. Preflight failures occur before execution hooks run. Error hooks receive structured `ActionError` values. `Hooks.OnError` remains an observation callback; the old `OnError` recovery method has been removed.

Hooks cannot change outcomes — use policies for that. Hooks invoked by parallel members must be safe for concurrent calls. Use `StartWithResult()` to retrieve the final result directly.

### Context propagation

```go
ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
defer cancel()
f := flowm.New().WithContext(ctx)
```

- If a function's first parameter is `context.Context` and you supplied one fewer argument, the flow's context is injected automatically — on the initial call and on every retry.
- Cancellation is checked before executing an action and honored while waiting on retry delays, rate-limit tokens, worker slots, and action calls.

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
| `AttemptObserverPolicy` | observe the outcome of every executed attempt | circuit breaker |

## API reference

```go
type Flowm interface {
	// building
	Do(function interface{}, params ...interface{}) Flowm
	AddAction(action Action)
	AddParallel(actions ...Action) Flowm

	// configuring the last added action
	Named(name string) Flowm
	With(policy Policy) Flowm
	PipeAll() Flowm
	Pipe(indices ...int) Flowm
	Map(mapper func([]interface{}) []interface{}) Flowm
	When(predicate func(Outcome) bool, action Action) Flowm
	WithConcurrency(limit int) Flowm // last added parallel group

	// configuring the flow
	WithContext(ctx context.Context) Flowm
	WithHooks(h Hooks) Flowm

	// running
	Validate() error
	Start() error
	StartWithResult() ([]interface{}, error)
}

// Action builders
func Do(function interface{}, params ...interface{}) Action // chainable: .Named, .With, .When

// Errors
func Permanent(err error) error // mark non-retryable; unwrappable via errors.Is/As
type ActionError struct {
	Name     string
	Attempts int
	Err      error
} // implements Error() and Unwrap()
```

Built-in policies:

| Policy | Effect |
|---|---|
| `RetryPolicy{Attempts, Delay, BackoffFactor, MaxDelay, Jitter, RetryIf}` | retry failed attempts with backoff |
| `TimeoutPolicy{PerAttempt, Overall}` | bound each attempt and the total action budget |
| `CircuitBreakerPolicy{Failures, Window, Cooldown}` | fail fast after repeated failures |
| `RateLimitPolicy{Per, Burst}` | token-bucket throttling |
| `WorkerPoolPolicy{Size}` | group concurrency limit (sugar: `WithConcurrency`); member placement is deprecated |
| `PipeAllPolicy` / `PipeIndicesPolicy` / `PipeMapPolicy` | piping (sugar: `PipeAll` / `Pipe` / `Map`) |

## Behavior notes

- **Policy order matters within an action.** Policies run in attachment order. Outcome routing always runs afterward, with the first matching `When` winning.
- **Shared policy instances share state.** One `RateLimitPolicy` across actions = one global bucket; one `CircuitBreakerPolicy` = one shared breaker. Use separate instances for independent behavior. Built-in retry configuration is copied per invocation. Route definitions can be reused; branch execution has independent invocation state. Custom policies and hooks used by parallel members must be safe for concurrent calls.
- **Reflection trade-off.** Definitions and explicit arguments are validated before execution; dynamic piped arguments are validated when available. Nil is accepted for nil-capable parameters, and variadic functions accept individual arguments. This does not provide compile-time checking.
- **Panics become errors.** A panic in an action or branch is recovered and treated as an error that can be retried or handled by a route. A predicate panic terminates routing.
- **Non-retryable errors remain errors.** `Permanent` and `DoNotRetry` stop retries; handle or explicitly ignore the error with an error-matching `When`, or let `Start()` return it.
- **Results come from `StartWithResult`, piping, or hooks.** `Start()` returns only `error`; use `StartWithResult()`, pipe into a consumer function, or the `OnSuccess` hook.

## Migration from the previous API

This cleanup intentionally breaks compatibility with the removed APIs. The package keeps function sequencing, conditional actions, and resilience; iteration and rollback stay in ordinary Go code.

| Removed API | Reason and replacement |
|---|---|
| `Compensate` / `CompensatePolicy` (including `Action.Compensate`) | Top-level-only rollback cannot safely promise transaction semantics. Handle cleanup explicitly in application code. |
| `ForEach` / `ForEachPolicy` | Implicit concurrent fan-out reused mutable policy instances. Use a loop or build explicit `Action` values for `AddParallel`. |
| `DoFlow` | Mutated the child flow's context. Compose ordinary functions, or use `Do(sub.StartWithResult)` with the child's context configured explicitly. |
| `NewAction(fn, params, policies...)` | Duplicate constructor. Use `Do(fn, params...).With(policies...)`. |
| `TerminatePolicy` | Its name suggested stopping, but it continued after errors. Use an error-matching `When` with a successful recovery branch. |
| `PassValuesPolicy` | Deprecated duplicate. Use `PipeAll()` or `PipeAllPolicy`. |
| `RetryPolicy.Fn` | Duplicate retry gate. Use `RetryIf`; use a `PreExecutionPolicy` for checks before every attempt. |
| `RetryPolicy.MaxElapsed` | Overlapping budget that did not cover the initial attempt or bound waits. Use `TimeoutPolicy.Overall` or a context deadline. |
| `MultiError` | Duplicated the standard library. Parallel failures now use `errors.Join`; inspect them with `errors.Is` / `errors.As` instead of asserting a concrete container type or matching its text. |

`Permanent` / `DoNotRetry` now preserve the failure instead of silently succeeding. `RetryIf` and `Delay` now apply before the first retry as well as later retries. Parallel members support recovery through `When`, and piped parallel results exclude error values.

The outcome-routing API intentionally replaces the previous conditional and fallback APIs:

| Previous API | Replacement |
|---|---|
| `f.Do(consumer).When(predicate)` | Attach `When(func(Outcome) bool, Do(consumer...))` to the **producer**. Adapt the branch to request `Outcome` if it needs the producer's values. |
| `OnError(fn, params...)` / `Action.OnError` | `When(func(out Outcome) bool { return out.Err != nil }, Do(fn, params...))`; use an `Outcome` parameter instead of an injected `error`. |
| `ContinueOnError()` / `ContinueOnErrorPolicy` | Use an error-matching `When` with a successful branch, such as `Do(func() {})`. An empty branch clears source values; explicitly return any values the next task needs. |
| Chained `OnError` calls | Nested `When` rules on branch actions. Sibling rules are first-match alternatives. |
| `ConditionPolicy`, `FallbackPolicy`, `FallbackAwarePolicy` | Removed. Attach routes with `When`; a branch is an ordinary `Action` with its own policies. |
| Single-argument or slice-based `When` predicates | Use `func(Outcome) bool` and inspect `out.Values` and `out.Err`. Nil predicates are invalid. |
| `OnSkip` for the source action | `OnSkip` now reports a branch whose rule was evaluated and did not match. The source action always executes. |

Custom `Flowm` implementations must use the new `When` signature; `OnError` and `ContinueOnError` are no longer required interface methods. `Hooks.OnError` remains available for observation. Use keyed `Action` literals. `Start` validates all branch definitions before execution, and returned error text includes action details; inspect causes with `errors.Is`/`errors.As` instead of equality or string matching.

## Testing

```bash
go test -race ./...
```

## License

MIT
