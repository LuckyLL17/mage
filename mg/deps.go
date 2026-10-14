package mg

import (
	"context"
	"fmt"
	"log"
	"os"
	"reflect"
	"runtime"
	"strings"
	"sync"
)

var logger = log.New(os.Stderr, "", 0)

type onceMap struct {
	mu *sync.Mutex
	m  map[onceKey]*onceFun
}

type onceKey struct {
	Name string
	ID   string
}

func (o *onceMap) LoadOrStore(f Fn) *onceFun {
	defer o.mu.Unlock()
	o.mu.Lock()

	key := onceKey{
		Name: f.Name(),
		ID:   f.ID(),
	}
	existing, ok := o.m[key]
	if ok {
		return existing
	}
	one := &onceFun{
		once:        &sync.Once{},
		fn:          f,
		displayName: displayName(f.Name()),
		executionID: dependencyExecutionID(f),
	}
	o.m[key] = one
	return one
}

func (o *onceMap) Reset() {
	o.mu.Lock()
	defer o.mu.Unlock()

	o.m = map[onceKey]*onceFun{}
}

var onces = &onceMap{
	mu: &sync.Mutex{},
	m:  map[onceKey]*onceFun{},
}

// ResetDeps clears the dependency cache used by Deps, CtxDeps, SerialDeps, and
// SerialCtxDeps. It is intended for tests that need to run the same dependencies
// more than once in a single process. Do not call ResetDeps while dependencies
// are running.
func ResetDeps() {
	onces.Reset()
}

// SerialDeps is like Deps except it runs each dependency serially, instead of
// in parallel. This can be useful for resource intensive dependencies that
// shouldn't be run at the same time.
func SerialDeps(fns ...interface{}) {
	funcs := checkFns(fns)
	ctx := ambientContext()
	for i := range fns {
		runDeps(ctx, funcs[i:i+1], false)
	}
}

// SerialCtxDeps is like CtxDeps except it runs each dependency serially,
// instead of in parallel. This can be useful for resource intensive
// dependencies that shouldn't be run at the same time.
func SerialCtxDeps(ctx context.Context, fns ...interface{}) {
	funcs := checkFns(fns)
	for i := range fns {
		runDeps(ctx, funcs[i:i+1], false)
	}
}

// CtxDeps runs the given functions as dependencies of the calling function.
// Dependencies must only be of type:
//
//	func()
//	func() error
//	func(context.Context)
//	func(context.Context) error
//
// Or a similar method on a mg.Namespace type.
// Or an mg.Fn interface.
//
// The function calling Deps is guaranteed that all dependent functions will be
// run exactly once when Deps returns.  Dependent functions may in turn declare
// their own dependencies using Deps. Each dependency is run in their own
// goroutines. Each function is given the context provided if the function
// prototype allows for it.
func CtxDeps(ctx context.Context, fns ...interface{}) {
	funcs := checkFns(fns)
	runDeps(ctx, funcs, true)
}

// runDeps assumes you've already called checkFns.
func runDeps(ctx context.Context, fns []Fn, parallel bool) {
	parent := resolveFrame(ctx)
	if parent != nil {
		dependencies := make([]TargetID, len(fns))
		for i, f := range fns {
			dependencies[i] = TargetRef(f)
		}
		parent.state.emit(Event{
			Kind:         EventDepsExpanded,
			ExecutionID:  parent.execution,
			Target:       parent.target,
			Dependencies: dependencies,
			Parallel:     parallel,
		})
	}
	mu := &sync.Mutex{}
	var errs []string
	var exit int
	wg := &sync.WaitGroup{}
	for _, f := range fns {
		fn := onces.LoadOrStore(f)
		ref := TargetRef(f)
		wg.Add(1)
		go func() {
			defer func() {
				if v := recover(); v != nil {
					mu.Lock()
					if err, ok := v.(error); ok {
						exit = changeExit(exit, ExitStatus(err))
					} else {
						exit = changeExit(exit, 1)
					}
					errs = append(errs, fmt.Sprint(v))
					mu.Unlock()
				}
				wg.Done()
			}()
			err, ran := fn.run(ctx, parent, ref)
			if parent != nil && !ran {
				parent.state.emit(Event{
					Kind:            EventTargetSkipped,
					ExecutionID:     fn.executionID,
					ParentExecution: parent.execution,
					Target:          ref,
					ReusesExecution: fn.executionID,
				})
			}
			if err != nil {
				mu.Lock()
				errs = append(errs, fmt.Sprint(err))
				exit = changeExit(exit, ExitStatus(err))
				mu.Unlock()
			}
		}()
	}

	wg.Wait()
	if len(errs) > 0 {
		panic(Fatal(exit, strings.Join(errs, "\n")))
	}
}

func checkFns(fns []interface{}) []Fn {
	funcs := make([]Fn, len(fns))
	for i, f := range fns {
		if fn, ok := f.(Fn); ok {
			funcs[i] = fn
			continue
		}

		// Check if the target provided is a not function so we can give a clear warning
		t := reflect.TypeOf(f)
		if t == nil || t.Kind() != reflect.Func {
			panic(fmt.Errorf("non-function used as a target dependency: %T. The mg.Deps, mg.SerialDeps and mg.CtxDeps functions accept function names, such as mg.Deps(TargetA, TargetB)", f))
		}

		funcs[i] = F(f)
	}
	return funcs
}

// Deps runs the given functions in parallel, exactly once. Dependencies must
// only be of type:
//
//	func()
//	func() error
//	func(context.Context)
//	func(context.Context) error
//
// Or a similar method on a mg.Namespace type.
// Or an mg.Fn interface.
//
// This is a way to build up a tree of dependencies with each dependency
// defining its own dependencies.  Functions must have the same signature as a
// Mage target, i.e. optional context argument, optional error return.
func Deps(fns ...interface{}) {
	CtxDeps(ambientContext(), fns...)
}

func changeExit(old, nw int) int {
	if nw == 0 {
		return old
	}
	if old == 0 {
		return nw
	}
	if old == nw {
		return old
	}
	// both different and both non-zero, just set
	// exit to 1. Nothing more we can do.
	return 1
}

// funcName returns the unique name for the function.
func funcName(i interface{}) string {
	return runtime.FuncForPC(reflect.ValueOf(i).Pointer()).Name()
}

func displayName(name string) string {
	splitByPackage := strings.Split(name, ".")
	if len(splitByPackage) == 2 && splitByPackage[0] == "main" {
		return splitByPackage[len(splitByPackage)-1]
	}
	return name
}

type onceFun struct {
	once *sync.Once
	fn   Fn
	err  error

	displayName string
	executionID string
}

// run will run the function exactly once and capture the error output. Further
// runs simply return the same error output. It reports whether this caller was
// the one that actually executed the function body; all other callers reuse
// the single execution.
func (o *onceFun) run(ctx context.Context, parent *eventFrame, ref TargetID) (err error, ran bool) {
	o.once.Do(func() {
		ran = true
		if Verbose() {
			logger.Println("Running dependency:", displayName(o.fn.Name()))
		}
		runCtx := ctx
		var state *eventState
		if parent != nil {
			state = parent.state
			state.emit(Event{
				Kind:            EventTargetStarted,
				ExecutionID:     o.executionID,
				ParentExecution: parent.execution,
				Target:          ref,
			})
			runCtx = withFrame(ctx, &eventFrame{
				state:     state,
				execution: o.executionID,
				target:    ref,
			})
			popAmbient := pushAmbient(eventFrame{
				state:     state,
				execution: o.executionID,
				target:    ref,
			})
			defer popAmbient()
		}
		func() {
			defer func() {
				if value := recover(); value != nil {
					if state != nil {
						state.emit(failureEvent(o.executionID, parentExecution(parent), ref, value))
					}
					panic(value)
				}
			}()
			o.err = o.fn.Run(runCtx)
		}()
		if state != nil {
			if o.err != nil {
				state.emit(failureEvent(o.executionID, parentExecution(parent), ref, o.err))
			} else {
				state.emit(Event{
					Kind:            EventTargetCompleted,
					ExecutionID:     o.executionID,
					ParentExecution: parentExecution(parent),
					Target:          ref,
				})
			}
		}
	})
	return o.err, ran
}

// parentExecution returns the frame's execution id or the empty string.
func parentExecution(frame *eventFrame) string {
	if frame == nil {
		return ""
	}
	return frame.execution
}
