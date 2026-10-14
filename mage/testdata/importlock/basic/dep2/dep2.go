package dep2

import "fmt"

// Second is a target exposed by a transitively imported package.
func Second() {
	fmt.Println("dep2 second")
}
