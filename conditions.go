package flowm

import (
	"fmt"
	"reflect"
)

func disabledPredicate(predicate interface{}) bool {
	return predicate == nil || (reflect.TypeOf(predicate).Kind() == reflect.Func && reflect.ValueOf(predicate).IsNil())
}

func validatePredicate(predicate interface{}) error {
	if disabledPredicate(predicate) {
		return nil
	}
	t, err := functionType(predicate)
	if err != nil {
		return fmt.Errorf("When predicate: %w", err)
	}
	if t.NumOut() != 1 || t.Out(0).Kind() != reflect.Bool {
		return fmt.Errorf("When predicate must return exactly one bool")
	}
	return nil
}

func (f *flowm) evaluateCondition(predicate interface{}, values []interface{}) (run bool, err error) {
	if disabledPredicate(predicate) {
		return true, nil
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("When predicate panic: %v", recovered)
		}
	}()
	args, err := prepareArguments(predicate, predicateArguments(predicate, values), f.ctx, false)
	if err != nil {
		return false, fmt.Errorf("When predicate: %w", err)
	}
	return reflect.ValueOf(predicate).Call(args)[0].Bool(), nil
}

func predicateArguments(predicate interface{}, values []interface{}) []interface{} {
	t := reflect.TypeOf(predicate)
	first := 0
	if t.NumIn() > 0 && t.In(0) == contextType {
		first = 1
	}
	if t.NumIn() == first {
		return nil // predicates over captured state can ignore incoming values
	}
	if !t.IsVariadic() && t.NumIn() == first+1 && t.In(first) == valuesType {
		return []interface{}{values} // preserve the original slice-based API
	}
	return values
}
