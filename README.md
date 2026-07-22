# Flowm – Go Function Flow Manager with Policies

Flowm is a simple and extensible Go package for managing sequences of function calls ("actions") with optional policies such as retries, backoff, timeouts, rate limiting, circuit breakers, and conditional execution.

---

## ✨ Features

- **Sequential Action Execution** – Run functions in order.
- **Context Propagation** – `WithContext(ctx)` injects `context.Context` into actions that accept it.
- **Policy System** – Attach policies (retries, timeouts, rate limits, circuit breakers, etc.).
- **Dynamic Function Calling** – Use reflection to call arbitrary functions with parameters.
- **Error Control** – Stop on error by default, or continue via `ContinueOnErrorPolicy`.
- **Error Fallbacks** – `OnError(fn)` chains an alternative function that runs when an action fails; its results pipe onward.
- **Retry Support** – Attempts (total), delay, max delay, backoff, jitter, predicate (`RetryIf`), max elapsed.
- **Timeouts** – `TimeoutPolicy{PerAttempt}` to bound each call.
- **Piping Outputs** – `PipeAll()`, `Pipe(indices...)`, `Map(fn)` to pass outputs to next action.
- **Parallel Groups** – `AddParallel(actions...)` with optional `WorkerPoolPolicy{Size}`.
- **Rate Limiting** – `RateLimitPolicy{Per, Burst}` token-bucket style.
- **Circuit Breaker** – `CircuitBreakerPolicy{Failures, Window, Cooldown}`.
- **Observability Hooks** – `WithHooks(Hooks{OnStart, OnSuccess, OnError, OnRetry})`.
- **Fluent Builder API** – `New().Do(...).With(...).PipeAll().ContinueOnError().Start()`.

---

## 📦 Installation

```bash
go get github.com/bi0dread/flowm
```

---

## 🚀 Quick Start

```go
f := flowm.New().WithContext(context.Background())

retry := &flowm.RetryPolicy{
    Attempts:      3,              // total attempts including the original call
    Delay:         500 * time.Millisecond,
    BackoffFactor: 2.0,
    Jitter:        true,
}

f.Do(step1, "Hello").With(retry).PipeAll().Do(step2)

if err := f.Start(); err != nil {
    log.Println("flow ended:", err)
}
```

---

## 📚 API Reference

### Types

#### `type Flowm interface`

```go
type Flowm interface {
    AddAction(action Action)
    WithContext(ctx context.Context) Flowm
    WithHooks(h Hooks) Flowm
    Do(function interface{}, params ...interface{}) Flowm
    With(policy Policy) Flowm
    PipeAll() Flowm
    Pipe(indices ...int) Flowm
    Map(mapper func([]any) []any) Flowm
    ContinueOnError() Flowm
    OnError(function interface{}, params ...interface{}) Flowm
    AddParallel(actions ...Action) Flowm
    Start() error
}
```

#### `type Action struct`

```go
type Action struct {
    Func     interface{}
    Params   []interface{}
    Policies []Policy
}
```

#### `type Policy interface`

```go
type Policy interface {
    Do(action Action, err error) error
}
```

Policies may also implement:
- `PreExecutionPolicy{ Before(ctx, action) error }`
- `ContextAwarePolicy{ SetContext(ctx) }`
- `RetryHookAwarePolicy{ SetOnRetry(func(name string, attempt int, err error)) }`

#### `type RetryPolicy struct`

```go
type RetryPolicy struct {
    Attempts      int               // total attempts INCLUDING the initial call
    Delay         time.Duration
    MaxDelay      time.Duration
    BackoffFactor float64
    Jitter        bool
    Fn            func() error      // optional pre-check before each retry
    RetryIf       func(error) bool  // retry predicate
    MaxElapsed    time.Duration     // limit total retry time
}
```

Behavior:
- Swallows errors implementing `DoNotRetry`.
- Respects `RetryIf`; if provided and returns false, stops retrying.
- Emits OnRetry hook if configured via `WithHooks`.
- Retries run through the same execution path as the original call: context injection, per-attempt `TimeoutPolicy`, pre-execution policies (rate limiting, circuit breaker), and panic recovery apply to every attempt.
- If `Fn` (pre-check before each retry) returns an error, retrying stops and the last call error is returned.
- When a retry succeeds, its return values are the ones piped to the next action.

#### Piping
- `PipeAll()` – pipe all return values to the next action.
- `Pipe(indices...)` – pipe selected return indices.
- `Map(func([]any) []any)` – transform outputs before passing on.

#### Timeouts
- `TimeoutPolicy{PerAttempt}` – bounds a single call duration; returns `context.DeadlineExceeded` on timeout.

#### Continue-on-error
- `ContinueOnErrorPolicy{Continue: true}` – continue the flow and swallow the error.
- `TerminatePolicy` is deprecated; behaves like continue-on-error for backward-compat.

#### Error fallback (catch)
- `OnError(fn, params...)` – run `fn` when the action fails. If the fallback succeeds, the flow continues and the fallback's return values replace the failed action's outputs for piping.
- The action's error is passed to `fn` if it declares a leading `error` parameter (after an optional `context.Context`).
- Chain multiple `OnError` calls: the next fallback runs only if the previous one also failed.
- Combine with retry: `.With(retry).OnError(fn)` runs the fallback only after retries are exhausted.

```go
fetchLive := func() (string, error) { return "", errors.New("service down") }
fetchCache := func(err error) (string, error) { return "cached-value", nil }

f := flowm.New()
f.Do(fetchLive).OnError(fetchCache).PipeAll()
f.Do(func(v string) { fmt.Println("got:", v) }) // got: cached-value
_ = f.Start()
```

#### Concurrency
- `AddParallel(actions...)` – execute actions concurrently; results are flattened in declaration order and can be piped.
- `flowm.Do(fn, params...)` – package-level action builder with the same signature as `Flowm.Do`; chain `.With(policy)` / `.OnError(fn)` on it. (`NewAction(fn, []interface{}{...}, policies...)` still works.)
- `WorkerPoolPolicy{Size}` – optional semaphore to limit concurrency.

```go
f.AddParallel(
    flowm.Do(fetchName),
    flowm.Do(fetchRole).With(rateLimiter),
    flowm.Do(fetchTeam, teamID).OnError(fetchTeamFromCache),
).PipeAll()
f.Do(func(name, role, team string) { ... })
```

#### Rate limiting and circuit breaker
- `RateLimitPolicy{Per, Burst}`
- `CircuitBreakerPolicy{Failures, Window, Cooldown}`

#### Hooks
```go
type Hooks struct {
    OnStart   func(actionName string)
    OnSuccess func(actionName string, result []any)
    OnError   func(actionName string, err error)
    OnRetry   func(actionName string, attempt int, err error)
}
```

---

## 🛠 Custom Policies

```go
type LogPolicy struct{}

func (l LogPolicy) Do(action flowm.Action, err error) error {
    fmt.Println("Action finished with error:", err)
    return err
}
```

---

## 📜 License

MIT License. See LICENSE.

## Rate limiting

`RateLimitPolicy` uses a simple token-bucket:
- **Per**: time to generate one token (e.g., `time.Second/5` ≈ 5 req/s)
- **Burst**: max tokens available at once (initially full)

Attach it to actions to throttle calls. If you reuse the same policy instance across actions, they share the limiter (global rate); separate instances rate-limit independently.

### Examples

```go
// 5 req/s, burst 2; shared across two actions (global limiter)
rl := &flowm.RateLimitPolicy{Per: time.Second / 5, Burst: 2}

f := flowm.New()

f.AddAction(flowm.NewAction(callA, nil, rl))
f.AddAction(flowm.NewAction(callB, nil, rl))
_ = f.Start()
```

```go
// Per-action limiters (independent)
rlA := &flowm.RateLimitPolicy{Per: 200 * time.Millisecond, Burst: 1}
rlB := &flowm.RateLimitPolicy{Per: 1 * time.Second, Burst: 1}

f := flowm.New()
f.AddAction(flowm.NewAction(callA, nil, rlA))
f.AddAction(flowm.NewAction(callB, nil, rlB))
_ = f.Start()
```

```go
// Parallel with shared limiter + worker pool
rl := &flowm.RateLimitPolicy{Per: time.Second / 10, Burst: 3}
wp := &flowm.WorkerPoolPolicy{Size: 4}

f := flowm.New()
f.AddParallel(
    flowm.NewAction(fetchUser, nil, rl, wp),
    flowm.NewAction(fetchOrders, nil, rl, wp),
    flowm.NewAction(fetchInventory, nil, rl, wp),
).PipeAll()
_ = f.Start()
```

### Context and timeouts
- `RateLimitPolicy` waits for tokens but respects `ctx.Done()`. Provide `WithContext(ctx)` on the flow to enable cancellation.
- If combined with `TimeoutPolicy{PerAttempt}`, the attempt can time out while waiting for a token, returning `context.DeadlineExceeded`.

```go
ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
defer cancel()
rl := &flowm.RateLimitPolicy{Per: 200 * time.Millisecond, Burst: 1}

f := flowm.New().WithContext(ctx)
f.AddAction(flowm.NewAction(callSlowAPI, nil, rl, &flowm.TimeoutPolicy{PerAttempt: 100 * time.Millisecond}))
err := f.Start() // likely deadline exceeded due to rate-limit wait
```

### Tips
- For N QPS: set `Per = time.Second / N` and choose an appropriate `Burst`.
- Ensure `Per > 0` and `Burst >= 1`.
- Reuse the same `RateLimitPolicy` instance to enforce a shared/global limit across actions.
- For strict fairness across many goroutines/flows, consider a centralized limiter you pass around (this policy is per-process, in-memory).
