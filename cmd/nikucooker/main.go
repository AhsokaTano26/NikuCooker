// Command nikucooker is the NikuCooker entry point.
//
// One binary serves the web UI, runs the subtitle pipeline, manages models and
// diagnoses the host. The CLI, the HTTP API and the web UI are all clients of
// the same business core; nothing is implemented in only one of them.
//
// Design documentation lives in ARCHITECTURE.md and docs/.
package main

import "os"

func main() {
	if err := newRootCmd().Execute(); err != nil {
		// Cobra has already written the error to stderr. A non-zero exit is the
		// only remaining signal for scripts and CI.
		os.Exit(1)
	}
}
