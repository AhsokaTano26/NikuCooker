// Package web carries the built Vue application into the Go binary.
//
// Embedding means a release is a single executable with no asset directory to
// lose, no path to misconfigure and no separate deployment step. See
// docs/deployment.md §7.
//
// dist/ holds a committed placeholder index.html so that `go build` succeeds on
// a fresh clone with no Node toolchain installed — a requirement of every phase
// being buildable. `make web-build` overwrites it with the real application.
package web

import "embed"

// Dist is the web application's document root.
//
// The all: prefix includes files whose names begin with an underscore or a dot,
// which Vite emits for some assets and which the default embed rules would
// silently drop.
//
//go:embed all:dist
var Dist embed.FS

// DistDir is the directory within Dist that serves as the document root.
const DistDir = "dist"
