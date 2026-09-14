// Command orc prints its version and nothing else yet; it exists so CI has a
// binary to vet, test and build.
package main

import (
	"fmt"
	"io"
	"os"
)

// version is replaced at release with -ldflags "-X main.version=<v>".
var version = "0.0.0-dev"

func printVersion(w io.Writer) error {
	_, err := fmt.Fprintf(w, "orc %s\n", version)
	return err
}

func main() {
	if err := printVersion(os.Stdout); err != nil {
		os.Exit(1)
	}
}
