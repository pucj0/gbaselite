// Historical cold-read runtime was removed. Storage-format migration tests live
// in storage and executor; this entry point fails explicitly for old scripts.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "paged/cold runtime probe was removed; use mvccprobe for runtime tests and migrate-legacy for old data")
	os.Exit(1)
}
