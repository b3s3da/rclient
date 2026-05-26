// Package web exposes the embedded panel UI assets.
package web

import "embed"

//go:embed all:static
var Files embed.FS
