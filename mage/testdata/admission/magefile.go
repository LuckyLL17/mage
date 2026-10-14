//go:build mage
// +build mage

package main

import (
	"context"
	"fmt"

	"github.com/magefile/mage/mg"
)

// Aliases exercise alias-based admission rules.
var Aliases = map[string]interface{}{
	"b": Build,
}

// Build is an allowed build target.
func Build() {
	fmt.Println("build ran")
}

// Danger is a target that policy may forbid.
func Danger() {
	fmt.Println("danger ran")
}

// Greet takes an argument so argument rules can be exercised.
func Greet(name string) {
	fmt.Println("greet ran", name)
}

// Chain schedules Mid, which schedules Leaf. It lets the denial explanation
// span multiple hops.
func Chain() {
	mg.Deps(Mid)
	fmt.Println("chain ran")
}

// Mid is an admitted transitive node.
func Mid() {
	mg.Deps(Leaf)
	fmt.Println("mid ran")
}

// Leaf may be forbidden even though nothing on the command line names it.
func Leaf() {
	fmt.Println("leaf ran")
}

// Parallel schedules two independent dependencies at once.
func Parallel() {
	mg.Deps(Pa, Pb)
	fmt.Println("parallel ran")
}

// Pa is an admitted parallel branch.
func Pa() {
	fmt.Println("pa ran")
}

// Pb may be forbidden; when it is, Pa must not start either.
func Pb() {
	fmt.Println("pb ran")
}

// Shared is a dependency of more than one root target.
func Shared() {
	fmt.Println("shared ran")
}

// Share1 is a root that schedules Shared.
func Share1() {
	mg.Deps(Shared)
	fmt.Println("share1 ran")
}

// Share2 is another root scheduling the same Shared.
func Share2() {
	mg.Deps(Shared)
	fmt.Println("share2 ran")
}

// CtxChain uses CtxDeps so cancellation can be observed.
func CtxChain(ctx context.Context) {
	mg.CtxDeps(ctx, Leaf)
	fmt.Println("ctxchain ran")
}

type NS mg.Namespace

// Task lives in a namespace.
func (NS) Task() {
	fmt.Println("ns task ran")
}

// Nested is a nested namespace target.
func (NS) Nested() {
	fmt.Println("ns nested ran")
}
