package dep

import (
	"fmt"

	"github.com/magefile/mage/mg"

	//mage:import
	_ "github.com/magefile/mage/mage/testdata/importlock/basic/dep2"
)

// Build builds the dependency.
func Build() {
	fmt.Println("dep build")
}

// NS is a namespace.
type NS mg.Namespace

// Deploy deploys the dependency.
func (NS) Deploy() {
	fmt.Println("dep deploy")
}
