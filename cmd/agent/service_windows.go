//go:build windows

package main

import (
	"fmt"
	"os"
)

func runService(action string, args []string) {
	_ = args
	fmt.Fprintf(os.Stderr, "service action %q is only supported on linux agents\n", action)
	os.Exit(1)
}
