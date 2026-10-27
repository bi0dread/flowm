# flowm

[![Go Reference](https://pkg.go.dev/badge/github.com/bi0dread/flowm.svg)](https://pkg.go.dev/github.com/bi0dread/flowm)
[![Go Report Card](https://goreportcard.com/badge/github.com/bi0dread/flowm)](https://goreportcard.com/report/github.com/bi0dread/flowm)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

**flowm** is a lightweight, zero-dependency Go library for orchestrating sequences of function calls with composable resilience policies — retries, timeouts, circuit breakers, rate limits, outcome routing, and parallel execution — behind a fluent builder API.

```go
f := flowm.New().WithContext(ctx)
f.Do(fetchLive).With(retry).When(func(out flowm.Outcome) flowm.Action {
	if out.Err != nil {
		return flowm.Do(fetchCache)
	}
	return flowm.NoAction()
}).PipeAll()
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
  - [Jumping to named steps](#jumping-to-named-steps)
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
| **Flow** | An ordered list of actions, executed sequentially by `Start()` unless a `When` selects a named jump target. Stops at the first unhandled error. |
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

Validation checks registered functions and policies, explicit argument types and counts, non-nil decision callbacks, and built-in policy settings. It also checks every registered parallel member. Invalid registered definitions prevent earlier steps from causing side effects. Validation never invokes actions, decision callbacks, maps, custom policies, or hooks. Actions and jump destinations returned by a callback are checked at runtime before that action executes or the jump occurs; the source has already run at that point.

Piped values can change through mapping and outcome routing. Their exact types and counts are checked before the receiving action or its policies run. A callback can choose any named jump target, so named steps may need runtime argument checks when a routing source pipes values. Decision callbacks have the fixed signature `func(Outcome) Action`; returned actions can optionally request the source `Outcome` as a parameter. Function arguments remain checked at runtime. Custom policy configuration remains the policy author's responsibility.

Negative durations and retry counts are rejected. Rate limits require `Per > 0` and `Burst >= 1`; circuit breakers require positive thresholds and durations. A concurrency limit must be positive and belong to a parallel group. Duplicate timeout policies and concurrency settings on one action are rejected. Multiple `When` callbacks are allowed and evaluated in registration order until one returns a non-empty action. Zero retry attempts retain the existing behavior of making only the initial call.

### Action names and errors

Name steps and parallel members so logs and failures identify meaningful operations:

```go
f.Do(fetchUser, userID).Named("fetch-user").With(retry)
f.AddParallel(
	flowm.Do(fetchProfile).Named("profile"),
	flowm.Do(fetchPermissions).Named("permissions"),
).Named("load-details").WithConcurrency(2)
```

An empty name uses the function name; unnamed parallel groups use `parallel`. `Action{Name: "...", Func: fn}` is also supported. Jump targets require explicit names on top-level steps. A referenced name must be unique; duplicate names used only for diagnostics remain valid.

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

`Attempts` counts actual calls to that action, including retries. Each selected branch has its own name and attempt count. Validation and decision callback failures, cancellation before execution, and attempts rejected by pre-execution policies count as zero calls. A failed parallel group wraps joined, individually named member errors, preserving their causes through `errors.Is` and `errors.As`. If a branch fails, the returned error identifies that branch. `Outcome.Err` contains the underlying error; error hooks and the flow's returned errors add the `ActionError` wrapper.

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

`When` accepts **one function**: `func(Outcome) Action`. It runs after the source action and its policies finish, including retries. The callback chooses what happens next:

```go
f.Do(fetchUser, userID).Named("fetch-user").With(retry).
	When(func(out flowm.Outcome) flowm.Action {
		if out.Err != nil {
			return flowm.Do(loadCachedUser, userID).Named("load-cache")
		}
		user := out.Values[0].(User)
		if user.Active {
			return flowm.Do(sendWelcome, user).Named("send-welcome")
		}
		return flowm.NoAction()
	})
```

The outcome describes the completed source:

```go
type Outcome struct {
	Name     string
	Values   []interface{} // non-error values from the final attempt
	Err      error         // underlying error, nil on success
	Attempts int           // actual calls to this action, including retries
}
```

| Callback return | Behavior |
|---|---|
| `flowm.Do(fn, args...)` | Run the chosen action. Its values and error replace the source outcome. |
| `flowm.Jump("step-name")` | Handle the source error, retain its values, and continue from the named step. |
| `flowm.NoAction()` | Keep the source outcome and try the next callback, if any. |

`NoAction()` returns the empty `Action` value; `Action{}` remains supported. Returning `NoAction()` does **not** ignore an error. If every callback returns it, success continues with the source values and failure stops the flow with the source error. `Do(nil)` is invalid; it does not mean no decision.

Callbacks run in registration order. The first non-empty decision wins, so later callbacks are not called. A returned action's failure does not fall through to later callbacks on its source; handle it with that action's own `When`. Each callback runs at most once per source visit, after retries, regardless of where `When` appears relative to `With(retry)`.

Returned actions have their own names, retry and timeout policies, and nested decision callbacks. They use the parent flow context and do not inherit the source's policies or expired action deadline. They may request a leading `Outcome` parameter after an optional `context.Context`; both are injected. Other arguments must be supplied to `Do`, and may be selected from the callback's outcome.

Source `PipeAll`, `Pipe`, and `Map` apply to the final selected outcome. Piping policies belong on the source step. A returned action with no values clears the source values. To explicitly ignore an error and continue, return a successful action:

```go
f.Do(sendNotification).When(func(out flowm.Outcome) flowm.Action {
	if out.Err != nil {
		return flowm.Do(func() {})
	}
	return flowm.NoAction()
})
f.Do(nextTask)
```

Use nested callbacks for recovery chains:

```go
cache := flowm.Do(fetchCache).Named("cache").
	When(func(out flowm.Outcome) flowm.Action {
		if out.Err != nil {
			return flowm.Do(fetchDefault).Named("default")
		}
		return flowm.NoAction()
	})

f.Do(fetchLive).With(retry).When(func(out flowm.Outcome) flowm.Action {
	if out.Err != nil {
		return cache
	}
	return flowm.NoAction()
}).PipeAll()
f.Do(process) // receives live, cached, or default results
```

Callbacks work on ordinary actions, parallel members, and completed parallel groups. A group's merged slice is one value in `Outcome.Values`, matching `StartWithResult`. Group callbacks run after members finish or after the group's own deadline expires.

Treat outcomes as read-only in callbacks. A callback panic stops routing, preserves the source error when present, and identifies the source action with zero attempts for the failed decision. Returned definitions are validated before execution; invalid decisions also preserve the source error. A flow cancellation stops routing. An action timeout, `Permanent(err)`, or `DoNotRetry` can still be handled by a callback while the parent context is active.

### Jumping to named steps

Return `Jump(name)` from a `When` callback to choose an earlier step, later step, or the current step. The destination may be computed from the outcome:

```go
f := flowm.New().WithContext(ctx).WithMaxJumps(20)
f.Do(step1).Named("step-1")
f.Do(step2).Named("step-2")
f.Do(step3).Named("step-3")
f.Do(step4).Named("step-4")
f.Do(step5).Named("step-5")
f.Do(step6).Named("step-6").When(func(out flowm.Outcome) flowm.Action {
	if errors.Is(out.Err, ErrNeedsRefresh) {
		return flowm.Jump("step-2")
	}
	if out.Err != nil {
		return flowm.Do(handleError, out.Err)
	}
	return flowm.NoAction()
})
f.Do(step7).Named("step-7")
err := f.Start()
```

On `ErrNeedsRefresh`, execution resumes at step 2 and proceeds through steps 3–6 again. Other errors run `handleError`; if it succeeds, execution reaches step 7. A successful step 6 keeps its outcome and continues normally. `Jump` is optional; the callback can choose only ordinary actions if that is all the flow needs.

- A jump destination must identify exactly one explicitly named top-level action or parallel group. Missing, empty, ambiguous, and nested-only targets are rejected when the callback returns the jump, before control transfers.
- The destination uses its own arguments and policies. Each visit starts a fresh attempt count and retry budget; shared circuit-breaker and rate-limiter state is retained.
- Source piping sends values to the jump destination. Without piping, the target uses only its configured arguments. Previous inputs are never replayed automatically.
- A returned function action can itself return a jump through its `When` callback. The originating top-level step's piping applies to that action's values.
- Completed parallel groups may jump. A jump returned inside a parallel member or its nested callbacks fails with an error; individual workers cannot move the flow's execution position.
- `Jump` is only valid as a callback return. It cannot carry its own function, arguments, policies, or callbacks. It invokes no function and emits no action start/success hooks. Source outcome hooks remain observable, including handled errors; target hooks run when the target executes. Steps passed over by a jump emit no hooks.
- Each run allows **1000 jumps by default**. `WithMaxJumps(n)` sets a positive limit for forward, backward, and self jumps, resetting for each `Start` or `StartWithResult`. Exceeding it returns an `ActionError` wrapping `ErrJumpLimitExceeded`, attributed to the action that requested the jump. Use `errors.Is(err, flowm.ErrJumpLimitExceeded)` to detect it. Context cancellation also stops routing.

This loop sends the latest value back to its target:

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
f.Do(func(n int) { fmt.Println(n) }) // prints 3
err := f.Start()
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
	flowm.Do(fetchTeam, teamID).Named("team").When(func(out flowm.Outcome) flowm.Action {
		if out.Err != nil {
			return flowm.Do(fetchTeamFromCache, teamID)
		}
		return flowm.NoAction()
	}),
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

Hooks use the explicit action name when provided, otherwise the function name. Each source emits its success or error hook before routing; a selected branch emits its own hooks. A recovered source failure therefore remains observable. `OnSkip` names the source whose callback returned `NoAction()`; there is no selected branch to name. Callbacks after the first non-empty decision are not evaluated and emit no hooks. Preflight failures occur before execution hooks run. Error hooks receive structured `ActionError` values. `Hooks.OnError` remains an observation callback; the old `OnError` recovery method has been removed.

Hooks observe execution; use decision callbacks or policies to change outcomes. Hooks invoked by parallel members must be safe for concurrent calls. Use `StartWithResult()` to retrieve the final result directly.

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
	When(decide func(Outcome) Action) Flowm
	WithConcurrency(limit int) Flowm // last added parallel group

	// configuring the flow
	WithContext(ctx context.Context) Flowm
	WithHooks(h Hooks) Flowm
	WithMaxJumps(limit int) Flowm // positive, default 1000 per run

	// running
	Validate() error
	Start() error
	StartWithResult() ([]interface{}, error)
}

// Action builders
func Do(function interface{}, params ...interface{}) Action // chainable: .Named, .With, .When
func Jump(name string) Action // return from When to target a named top-level step
func NoAction() Action // preserve the source outcome and try the next callback

// Errors
func Permanent(err error) error // mark non-retryable; unwrappable via errors.Is/As
var ErrJumpLimitExceeded error // inspect with errors.Is
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

- **Policy order matters within an action.** Policies run in attachment order. Outcome callbacks always run afterward, with the first non-empty decision winning.
- **Shared policy instances share state.** One `RateLimitPolicy` across actions = one global bucket; one `CircuitBreakerPolicy` = one shared breaker. Use separate instances for independent behavior. Built-in retry configuration is copied per invocation. Route definitions can be reused; branch execution has independent invocation state. Custom policies and hooks used by parallel members must be safe for concurrent calls.
- **Reflection trade-off.** Definitions and explicit arguments are validated before execution; dynamic piped arguments are validated when available. Nil is accepted for nil-capable parameters, and variadic functions accept individual arguments. This does not provide compile-time checking.
- **Panics become errors.** A panic in an action or branch is recovered and treated as an error that can be retried or handled by a route. A decision callback panic terminates routing.
- **Non-retryable errors remain errors.** `Permanent` and `DoNotRetry` stop retries; handle or explicitly ignore the error with an error-matching `When`, or let `Start()` return it.
- **Results come from `StartWithResult`, piping, or hooks.** `Start()` returns only `error`; use `StartWithResult()`, pipe into a consumer function, or the `OnSuccess` hook.

## Migration from the previous API

This cleanup intentionally breaks compatibility with the removed APIs. The package keeps function sequencing, outcome routing, named jumps, and resilience. Collection iteration and rollback stay in ordinary Go code.

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
| `f.Do(consumer).When(predicate)` | Attach a decision callback to the producer; return `Do(consumer, args...)` when needed, otherwise `NoAction()`. |
| `Action{}` as a callback return | Prefer `NoAction()` to explicitly preserve the original outcome. |
| `When(predicate, action)` | `When(func(out Outcome) Action { if predicate(out) { return action }; return NoAction() })`. Construct the action or jump inside the callback. |
| `OnError(fn, params...)` / `Action.OnError` | Return `Do(fn, params...)` when `out.Err != nil`. Supply `out.Err` explicitly or request an injected `Outcome`. |
| `ContinueOnError()` / `ContinueOnErrorPolicy` | On error, return a successful action such as `Do(func() {})`. This clears source values; return any values the next task needs explicitly. `NoAction()` preserves the error. |
| Chained `OnError` calls | Nested decision callbacks on returned actions. Later callbacks on the source are not recovery handlers for a returned action's failure. |
| `ConditionPolicy`, `FallbackPolicy`, `FallbackAwarePolicy` | Removed. Return ordinary actions with their own policies from `When`. |
| Boolean or slice-based `When` predicates | Use `func(Outcome) Action`, inspect `out.Values` and `out.Err`, then return a decision. Nil callbacks are invalid. |
| `OnSkip` naming an unselected branch | `OnSkip` identifies the source whose callback returned `NoAction()`. |

Custom `Flowm` implementations must accept the single decision callback in `When` and implement `WithMaxJumps`. `OnError` and `ContinueOnError` are no longer interface methods; `Hooks.OnError` remains available for observation. Use keyed `Action` literals. Preflight checks registered definitions without invoking callbacks; returned actions and jump destinations are validated at runtime. Inspect error causes with `errors.Is`/`errors.As` instead of equality or string matching.

## Testing

```bash
go test -race ./...
```

## License

MIT
