//go:build mage
// +build mage

package main

import (
	"fmt"

	//mage:import lockdep
	_ "github.com/magefile/mage/mage/testdata/importlock/basic/dep"
)

// Root is a target on the root magefile.
func Root() {
	fmt.Println("root")
}
