# Flowm – Go Function Flow Manager with Policies

Flowm is a simple and extensible Go package for managing sequences of function calls ("actions") with optional **policies** such as retries, backoff, and conditional execution rules.  

It’s useful when you want to execute multiple steps in order, handle failures gracefully, and optionally retry failed actions with configurable policies.

---

## ✨ Features

- **Sequential Action Execution** – Run functions in the order you define.
- **Policy System** – Attach policies (e.g., retries with backoff) to actions.
- **Dynamic Function Calling** – Use reflection to call arbitrary functions with parameters.
- **Error-Aware Flow Control** – Stop execution if an error occurs (unless policies handle it).
- **Retry Support** – Configurable attempts, delay, max delay, backoff factor, and jitter.
- **Pluggable Policies** – Create your own policies by implementing the `Policy` interface.

---

## 📦 Installation

```bash
go get github.com/bi0dread/flowm
```

---

## 🚀 Quick Start

```go
package main

import (
    "errors"
    "fmt"
    "time"

    "github.com/bi0dread/flowm"
)

func step1(msg string) error {
    fmt.Println("Step 1:", msg)
    return errors.New("fail once")
}

func step2() {
    fmt.Println("Step 2: Success!")
}

func main() {
    f := flowm.New()

    retryPolicy := &flowm.RetryPolicy{
        Attempts:      3,
        Delay:         1 * time.Second,
        BackoffFactor: 2.0,
        Jitter:        true,
    }

    f.AddAction(flowm.NewAction(step1, []interface{}{"Hello"}, retryPolicy))
    f.AddAction(flowm.NewAction(step2, []interface{}{}))

    if err := f.Start(); err != nil {
        fmt.Println("Flow ended with error:", err)
    }
}
```

---

## 📚 API Reference

### **Types**

#### `type Flowm interface`
The core flow manager interface.

```go
type Flowm interface {
    AddAction(action Action)
    Start() error
}
```

---

#### `type flowm struct`
Internal implementation of `Flowm`.

---

#### `type Action struct`
Represents a function call with parameters and policies.

```go
type Action struct {
    Func     interface{}   // The function to call
    Params   []interface{} // Parameters to pass
    Policies []Policy      // Policies to apply after execution
}
```

---

#### `type Policy interface`
Any policy must implement the `Do` method.

```go
type Policy interface {
    Do(action Action, err error) error
}
```

Policies are executed after an action runs, receiving the action and any error it produced.

---

#### `type RetryPolicy struct`
Retries a failed action with optional backoff and jitter.

```go
type RetryPolicy struct {
    Attempts      int
    Delay         time.Duration
    MaxDelay      time.Duration
    BackoffFactor float64
    Jitter        bool
    Fn            func() error // Optional pre-check before retry
}
```

##### RetryPolicy Behavior:
- Retries the action if it fails, up to `Attempts` times.
- Waits `Delay` between retries, optionally increasing with `BackoffFactor`.
- Caps wait time at `MaxDelay` if set.
- Adds randomness to delay if `Jitter` is `true`.
- Stops retrying if error implements `DoNotRetry`.

---

#### `type DoNotRetry interface`
Marker interface to indicate that an error should not be retried.

```go
type DoNotRetry interface {
    DoNotRetry()
}
```

---

### **Functions**

#### `func New() Flowm`
Creates a new `Flowm` instance.

---

#### `func NewAction(function interface{}, params []interface{}, policy ...Policy) Action`
Creates a new `Action` from a function, its parameters, and optional policies.

---

#### `func (f *flowm) AddAction(act Action)`
Adds an `Action` to the flow.

---

#### `func (f *flowm) Start() error`
Executes all actions in order.  
Stops execution if an action returns an error not handled by its policies.

---

## ⚡ Example with Retry and Backoff

```go
func mightFail(count *int) error {
    *count++
    fmt.Println("Attempt", *count)
    if *count < 3 {
        return errors.New("still failing")
    }
    fmt.Println("Success on attempt", *count)
    return nil
}

func main() {
    count := 0
    f := flowm.New()

    retry := &flowm.RetryPolicy{
        Attempts:      5,
        Delay:         500 * time.Millisecond,
        BackoffFactor: 2.0,
        MaxDelay:      3 * time.Second,
        Jitter:        true,
    }

    f.AddAction(flowm.NewAction(mightFail, []interface{}{&count}, retry))
    f.Start()
}
```

---

## 🛠 Extending with Custom Policies

You can define your own policies by implementing `Policy`.

```go
type LogPolicy struct{}

func (l LogPolicy) Do(action flowm.Action, err error) error {
    fmt.Println("Action finished with error:", err)
    return err
}
```

Usage:

```go
f.AddAction(flowm.NewAction(myFunc, []interface{}{}, LogPolicy{}))
```

---

## 📜 License

MIT License. See [LICENSE](LICENSE) for details.
