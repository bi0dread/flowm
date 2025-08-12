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
	Name  string
	Order int
}

type Flowm interface {
	AddAction(action Action)
	Start() error
}

type flowm struct {
	stubStorage map[stub]Action
}

func New() Flowm {
	return &flowm{
		stubStorage: make(map[stub]Action),
	}
}

func (f *flowm) AddAction(act Action) {
	name, _ := f.getFunctionName(act.Func)
	f.stubStorage[stub{Name: name, Order: len(f.stubStorage)}] = act
}

func (f *flowm) Start() error {
	for stu, act := range f.stubStorage {
		_, err := f.call(stu.Name, stu.Order, act.Params...)

		for _, policy := range act.Policies {
			_ = policy.Do(act, err)
		}

		if err != nil {
			return err
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

func (f *flowm) call(funcName string, order int, params ...interface{}) (result []interface{}, err error) {

	return callReflect(f.stubStorage[stub{
		Name:  funcName,
		Order: order,
	}].Func, params...)

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
	Do(action Action, err error) error
}

type RetryPolicy struct {
	Attempts      int
	Delay         time.Duration
	MaxDelay      time.Duration
	BackoffFactor float64
	Jitter        bool
	Fn            func() error
}

func (r *RetryPolicy) Do(action Action, mainErr error) error {

	if mainErr != nil {

		if _, ok := mainErr.(DoNotRetry); ok {
			fmt.Println("Do not retry due to DoNotRetry error")
			return nil
		}

		var err error
		delay := r.Delay

		for attempt := 1; attempt <= r.Attempts; attempt++ {
			if r.Fn != nil {
				err = r.Fn()
				if err != nil {
					return nil
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
				return nil
			}

			if attempt == r.Attempts {
				break
			}

			select {
			case <-time.After(delay):
			}

		}

		return err
	}

	return mainErr

}

type DoNotRetry interface {
	DoNotRetry()
}
