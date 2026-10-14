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
		req:         dependencyRequest(f),
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
	ctx := context.Background()
	for i := range fns {
		runDeps(ctx, funcs[i:i+1])
	}
}

// SerialCtxDeps is like CtxDeps except it runs each dependency serially,
// instead of in parallel. This can be useful for resource intensive
// dependencies that shouldn't be run at the same time.
func SerialCtxDeps(ctx context.Context, fns ...interface{}) {
	funcs := checkFns(fns)
	for i := range fns {
		runDeps(ctx, funcs[i:i+1])
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
	runDeps(ctx, funcs)
}

// runDeps assumes you've already called checkFns.
func runDeps(ctx context.Context, fns []Fn) {
	if activePolicy() == nil {
		runDepsUnchecked(ctx, fns)
		return
	}
	runDepsChecked(ctx, fns)
}

// runDepsUnchecked is the legacy scheduler used when no admission policy is
// configured. It must stay behaviorally identical to the original
// implementation.
func runDepsUnchecked(ctx context.Context, fns []Fn) {
	mu := &sync.Mutex{}
	var errs []string
	var exit int
	wg := &sync.WaitGroup{}
	for _, f := range fns {
		fn := onces.LoadOrStore(f)
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
			if err := fn.run(ctx); err != nil {
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

// runDepsChecked is the scheduler used when an admission policy is active.
// It enforces four deterministic guarantees:
//
//   - every direct dependency of this call is admitted before any of them
//     starts, so a rejected sibling never runs as a side effect;
//   - a rejected dependency (and the nodes it would have scheduled) never
//     starts, while the caller panics and its own continuation is skipped;
//   - after cancellation no new node starts;
//   - rejection explanations carry the chain from the requested root target
//     to each rejected node.
func runDepsChecked(ctx context.Context, fns []Fn) {
	if err := ctx.Err(); err != nil {
		panic(Fatal(ExitStatus(err), err.Error()))
	}

	entries := make([]*onceFun, len(fns))
	for i, f := range fns {
		entries[i] = onces.LoadOrStore(f)
	}

	parent := parentChain()

	// Preflight in source order so the rejection explanation is stable
	// regardless of goroutine scheduling.
	var denials []*Denial
	chains := make([][]ChainLink, len(entries))
	for i, fn := range entries {
		req := withWorkDir(fn.req)
		chains[i] = appendChainLink(parent, req)
		if d := admit(parent, req); d != nil {
			denials = append(denials, d)
		}
	}
	if len(denials) > 0 {
		panic(&admissionError{denials: denials})
	}

	mu := &sync.Mutex{}
	var errs []string
	var exit int
	wg := &sync.WaitGroup{}
	for i, fn := range entries {
		fn := fn
		chain := chains[i]
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
			if err := fn.runChecked(ctx, chain); err != nil {
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

func appendChainLink(parent []ChainLink, req TargetRequest) []ChainLink {
	chain := make([]ChainLink, 0, len(parent)+1)
	chain = append(chain, parent...)
	return append(chain, ChainLink{Request: req})
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
	CtxDeps(context.Background(), fns...)
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
	req         TargetRequest
}

// run will run the function exactly once and capture the error output. Further runs simply return
// the same error output.
func (o *onceFun) run(ctx context.Context) error {
	o.once.Do(func() {
		if Verbose() {
			logger.Println("Running dependency:", displayName(o.fn.Name()))
		}
		o.err = o.fn.Run(ctx)
	})
	return o.err
}

// runChecked is run when an admission policy is active. The caller already
// admitted this dependency for the current batch; the admission is evaluated
// again here so that the single winning goroutine uses the one shared
// decision and a shared dependency can never run merely because another
// caller reached it first.
func (o *onceFun) runChecked(ctx context.Context, chain []ChainLink) error {
	o.once.Do(func() {
		req := withWorkDir(o.req)
		// The last link of chain is this very dependency; rebuild the
		// denial on top of its admitted ancestors.
		if d := admit(chain[:len(chain)-1], req); d != nil {
			o.err = d
			return
		}
		gid := goroutineID()
		setAmbientChain(gid, chain)
		defer clearAmbientChain(gid)
		if Verbose() {
			logger.Println("Running dependency:", displayName(o.fn.Name()))
		}
		o.err = o.fn.Run(ctx)
	})
	return o.err
}
