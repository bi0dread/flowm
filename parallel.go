package flowm

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

type parallelGroup struct {
	actions []Action
}

func parallelLimit(action Action) (int, error) {
	for _, policy := range action.Policies {
		if limit, ok := policy.(*WorkerPoolPolicy); ok && limit != nil {
			return limit.Size, nil
		}
	}
	// Preserve unambiguous legacy member limits while encouraging group settings.
	limit := 0
	for _, member := range action.parallel.actions {
		for _, policy := range member.Policies {
			if pool, ok := policy.(*WorkerPoolPolicy); ok && pool != nil {
				if limit != 0 && limit != pool.Size {
					return 0, fmt.Errorf("conflicting member concurrency limits; set WithConcurrency on the group")
				}
				limit = pool.Size
			}
		}
	}
	return limit, nil
}

func (f *flowm) runParallel(ctx context.Context, actions []Action, limit int) ([]interface{}, error) {
	group := *f
	group.ctx = ctx
	group.inParallel = true
	type outcome struct {
		values []interface{}
		err    error
	}
	out := make([]outcome, len(actions))
	if len(actions) == 0 {
		return nil, nil
	}
	if limit == 0 || limit > len(actions) {
		limit = len(actions)
	}
	// Allocate only as many workers as the group can run at once.
	var mu sync.Mutex
	next := 0
	var wg sync.WaitGroup
	for range limit {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				mu.Lock()
				if next == len(actions) {
					mu.Unlock()
					return
				}
				i := next
				next++
				mu.Unlock()
				name := actionName(actions[i])
				if err := ctx.Err(); err != nil {
					out[i].err = &ActionError{Name: name, Err: err}
					continue
				}
				result, _, _ := group.runSingleAction(stub{Name: name, Action: actions[i]})
				out[i] = outcome{values: result.Values, err: result.failure()}
			}
		}()
	}
	wg.Wait()
	var errs []error
	var results []interface{}
	for _, member := range out {
		if member.err != nil {
			errs = append(errs, member.err)
		}
		results = append(results, filterErrorValues(member.values)...)
	}
	return results, errors.Join(errs...)
}
