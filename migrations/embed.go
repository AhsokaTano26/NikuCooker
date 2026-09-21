// Package migrations carries the schema into the binary.
//
// Embedding rather than reading from disk means a released binary always has
// its own schema, so "migrations directory not found" is not a failure mode a
// user can hit.
//
// It is a Go package at the repository root because //go:embed cannot reach
// outside its own directory, and internal/database needs these files.
package migrations

import "embed"

// FS holds the numbered SQL files, applied in ascending order.
//
//go:embed *.sql
var FS embed.FS
