// Package web carries the built Vue application into the Go binary.
//
// Embedding means a release is a single executable with no asset directory to
// lose, no path to misconfigure and no separate deployment step. See
// docs/deployment.md §5.
//
// dist/ holds a tracked .gitkeep so that `go build` succeeds on a fresh clone
// with no Node toolchain — a //go:embed directive fails to compile if its
// directory is absent, and an empty directory cannot be committed to git.
// `make web-build` fills dist/ with the real application.
//
// The fallback page deliberately does not live in dist/. If it did, every build
// would overwrite a tracked file and leave the working tree dirty.
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

//go:embed unbuilt.html
var unbuiltPage []byte

// UnbuiltPage returns the page served when the web application has not been
// built into this binary.
//
// It names the command that fixes the situation, because the alternative — a
// blank page or a bare 404 — gives a user nothing to act on.
func UnbuiltPage() []byte { return unbuiltPage }
