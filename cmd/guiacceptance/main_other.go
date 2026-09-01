//go:build !windows || !amd64

package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "guiacceptance is supported only on windows/amd64")
	os.Exit(2)
}
