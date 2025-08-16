package flowm

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"reflect"
	"runtime"
	"strings"
	"time"
)

type Action struct {
	Func     interface{}
	Params   []interface{}
	Policies []Policy
}

type stub struct {
	Name   string
	Action Action
}

type Flowm interface {
	AddAction(action Action)
	Start() error
}

type flowm struct {
	stubStorage []stub
}

func New() Flowm {
	return &flowm{
		stubStorage: make([]stub, 0),
	}
}

func (f *flowm) AddAction(act Action) {
	name, _ := f.getFunctionName(act.Func)
	f.stubStorage = append(f.stubStorage, stub{Name: name, Action: act})
}

func (f *flowm) Start() error {

	var res []interface{}
	res = nil
	keepValue := false

	for i, act := range f.stubStorage {

		if res != nil && keepValue {
			if act.Action.Params == nil {
				act.Action.Params = []interface{}{}
			}
			act.Action.Params = append(act.Action.Params, res...)
			keepValue = false
			res = nil
		}

		result, err := f.call(i, act.Action.Params...)

		skipErr := false

		for _, policy := range act.Action.Policies {
			_, _ = policy.Do(act.Action, err)
			if p, ok := policy.(*TerminatePolicy); ok && p.Terminate {
				skipErr = true
			}
			if _, ok := policy.(*PassValuesPolicy); ok {

				res = result
				keepValue = true
			}
		}

		if err != nil {
			if skipErr {
				fmt.Printf("Skipping error for %s: %v\n", act.Name, err)
				continue
			} else {
				return err

			}
		}
	}

	return nil
}

func NewAction(function interface{}, Params []interface{}, Policy ...Policy) Action {
	return Action{
		Func:     function,
		Params:   Params,
		Policies: Policy,
	}
}

func (f *flowm) call(funcName int, params ...interface{}) (result []interface{}, err error) {

	return callReflect(f.stubStorage[funcName].Action.Func, params...)

}

func callReflect(funcValue interface{}, params ...interface{}) (result []interface{}, err error) {

	function := reflect.ValueOf(funcValue)

	if len(params) != function.Type().NumIn() {
		err = errors.New("The number of params is out of index")
		return
	}
	in := make([]reflect.Value, len(params))
	for k, param := range params {
		in[k] = reflect.ValueOf(param)
	}
	var res []reflect.Value
	res = function.Call(in)

	var mainErr error

	for _, v := range res {
		if v.Type().ConvertibleTo(reflect.TypeOf((*error)(nil)).Elem()) {
			if v.IsNil() {
				continue
			}
			if err = v.Interface().(error); err != nil {
				mainErr = err
			}
		}

		result = append(result, v.Interface())

	}
	return result, mainErr
}

func (f *flowm) getFunctionName(i interface{}) (name string, isMethod bool) {
	if fullName, ok := i.(string); ok {
		return fullName, false
	}
	fullName := runtime.FuncForPC(reflect.ValueOf(i).Pointer()).Name()
	isMethod = strings.ContainsAny(fullName, "*")
	elements := strings.Split(fullName, ".")
	shortName := elements[len(elements)-1]

	return strings.TrimSuffix(shortName, "-fm"), isMethod
}

type Policy interface {
	Do(action Action, err error) (any, error)
}

type RetryPolicy struct {
	Attempts      int
	Delay         time.Duration
	MaxDelay      time.Duration
	BackoffFactor float64
	Jitter        bool
	Fn            func() error
}

type PassValuesPolicy struct {
	Value interface{}
}

func (r *PassValuesPolicy) Do(action Action, mainErr error) (any, error) {

	return nil, mainErr
}

type TerminatePolicy struct {
	Terminate bool
}

func (r *TerminatePolicy) Do(action Action, mainErr error) (any, error) {

	return nil, mainErr
}

func (r *RetryPolicy) Do(action Action, mainErr error) (any, error) {

	if mainErr != nil {

		if _, ok := mainErr.(DoNotRetry); ok {
			fmt.Println("Do not retry due to DoNotRetry error")
			return nil, nil
		}

		var err error
		delay := r.Delay

		for attempt := 1; attempt <= r.Attempts; attempt++ {
			if r.Fn != nil {
				err = r.Fn()
				if err != nil {
					return nil, nil
				}
			}

			if r.BackoffFactor > 1 {
				delay = time.Duration(float64(delay) * r.BackoffFactor)
			}

			if r.MaxDelay > 0 && delay > r.MaxDelay {
				delay = r.MaxDelay
			}

			if r.Jitter {
				delay = time.Duration(float64(delay) * (0.5 + rand.Float64()/2))
			}

			_, callErr := callReflect(action.Func, action.Params...)
			if callErr != nil {
				//fmt.Println(callErr.Error())
			} else {
				fmt.Println("Retry success")
				return nil, nil
			}

			if attempt == r.Attempts {
				break
			}

			select {
			case <-time.After(delay):
			}

		}

		return nil, err
	}

	return nil, mainErr

}

type DoNotRetry interface {
	DoNotRetry()
}
