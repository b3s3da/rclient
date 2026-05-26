//go:build windows

package main

import (
	"fmt"
	"os"
)

func runInstall(args []string) {
	_ = args
	fmt.Fprintln(os.Stderr, "install is only supported on linux agents")
	os.Exit(1)
}

func runUninstall(args []string) {
	_ = args
	fmt.Fprintln(os.Stderr, "uninstall is only supported on linux agents")
	os.Exit(1)
}
