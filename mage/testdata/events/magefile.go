//go:build mage
// +build mage

package main

import (
	"fmt"

	"github.com/magefile/mage/mg"
)

// All expands a diamond of dependencies: Left and Right both depend on Common,
// so Common's body executes exactly once.
func All() {
	mg.Deps(Left, Right)
	fmt.Println("all")
}

// Left depends on Common.
func Left() {
	mg.Deps(Common)
	fmt.Println("left")
}

// Right depends on Common, exactly like Left.
func Right() {
	mg.Deps(Common)
	fmt.Println("right")
}

// Common is the shared dependency executed exactly once.
func Common() {
	fmt.Println("common")
}

// Serial runs its dependencies serially.
func Serial() {
	mg.SerialDeps(Left, Right)
}

// Fails returns an error carrying a custom exit code.
func Fails() error {
	return mg.Fatal(3, "nope")
}

// Panics panics with a non-error value.
func Panics() {
	panic("kaboom")
}
