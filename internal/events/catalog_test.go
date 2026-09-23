package events

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Every event the server can emit must be one the browser listens for.
//
// This is not a style rule, it is a delivery rule. EventSource dispatches a
// named event only to listeners registered for that name, and the store
// registers one per entry in its EVENT_TYPES list — so an event whose name is
// not in that list is emitted into a stream nobody is reading. Nothing errors,
// nothing logs, and the symptom is a progress bar that never moves.
//
// That happened: the server emits "model.progress" and the store registered
// "model.download.progress", so a multi-gigabyte download reported nothing at
// all. A test comparing the two lists is the only thing that catches it, which
// is why this reads a TypeScript file from a Go test.
func TestEveryEmittedEventIsListenedFor(t *testing.T) {
	path := filepath.Join("..", "..", "web", "src", "stores", "events.ts")

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("cannot read the store's event list: %v", err)
	}

	registered := map[string]bool{}
	for _, match := range regexp.MustCompile(`'([a-z][a-z0-9.]*)'`).FindAllStringSubmatch(string(raw), -1) {
		registered[match[1]] = true
	}
	if len(registered) == 0 {
		t.Fatal("no event names were found in the store; the list moved or changed shape")
	}

	emitted := map[string]Type{
		"hello":             TypeHello,
		"resync.required":   TypeResyncRequired,
		"job.status":        TypeJobStatus,
		"job.progress":      TypeJobProgress,
		"stage.status":      TypeStageStatus,
		"stage.progress":    TypeStageProgress,
		"project.updated":   TypeProjectUpdated,
		"project.deleted":   TypeProjectDeleted,
		"segment.updated":   TypeSegmentUpdated,
		"segments.replaced": TypeSegmentsReplaced,
		"settings.changed":  TypeSettingsChanged,
		"model.progress":    TypeModelProgress,
		"runtime.provision": TypeRuntimeProvision,
	}

	for name := range emitted {
		if !registered[name] {
			t.Errorf("the server emits %q and the interface never subscribes to it; "+
				"add it to EVENT_TYPES in web/src/stores/events.ts", name)
		}
	}

	// And the reverse, for the entries that are not simply not yet emitted: a
	// name in the list that no constant produces is a listener for a message
	// that will never arrive.
	for name := range registered {
		if _, ok := emitted[name]; !ok && strings.Contains(name, ".") {
			t.Logf("note: the interface subscribes to %q, which no event in this "+
				"catalog produces", name)
		}
	}
}
