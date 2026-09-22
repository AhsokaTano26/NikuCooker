package settings

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/AhsokaTano26/NikuCooker/internal/database"
)

func newService(t *testing.T) *Service {
	t.Helper()

	db, err := database.Open(database.Options{Path: filepath.Join(t.TempDir(), "test.db")})
	if err != nil {
		t.Fatalf("database.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if _, err := database.Migrate(context.Background(), db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return NewService(db)
}

func TestSetAndReadBack(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	if err := service.Set(ctx, map[string]any{
		"asr.model":        "large-v3",
		"subtitle.max_cps": 22,
		"subtitle.formats": []any{"srt"},
		"vad.enabled":      false,
	}); err != nil {
		t.Fatalf("Set: %v", err)
	}

	document, err := service.All(ctx)
	if err != nil {
		t.Fatalf("All: %v", err)
	}

	// Nested, because that is the shape a configuration layer takes.
	asr, ok := document["asr"].(map[string]any)
	if !ok {
		t.Fatalf("asr is not a section: %#v", document)
	}
	if asr["model"] != "large-v3" {
		t.Errorf("asr.model = %#v", asr["model"])
	}

	subtitle := document["subtitle"].(map[string]any)

	// Read back the way it was written. What comes out is JSON's idea of the
	// types — every number a float, every list []any — and that is fine: the
	// layer is marshalled to YAML on its way into the configuration, which
	// turns 22 into an integer and ["srt"] into a list of strings. That the
	// config ends up with the right Go types is asserted where the config is,
	// in the app's own test.
	if subtitle["max_cps"] != float64(22) {
		t.Errorf("subtitle.max_cps = %#v", subtitle["max_cps"])
	}
	formats, isList := subtitle["formats"].([]any)
	if !isList || len(formats) != 1 || formats[0] != "srt" {
		t.Errorf("subtitle.formats = %#v", subtitle["formats"])
	}
	if document["vad"].(map[string]any)["enabled"] != false {
		t.Errorf("vad.enabled = %#v", document["vad"])
	}
}

func TestSetRejectsAKeyThatIsNotOffered(t *testing.T) {
	service := newService(t)

	// The catalog is the allowlist. Without it a request could write into any
	// part of the configuration document, including one no layer is supposed to
	// set from here.
	err := service.Set(context.Background(), map[string]any{"storage.data_dir": "/tmp/elsewhere"})
	if !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("err = %v, want ErrUnknownKey", err)
	}
}

func TestSetRejectsValuesTheKeyCannotHold(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	cases := []struct {
		name  string
		key   string
		value any
	}{
		{"not an option", "asr.model", "enormous"},
		{"not a number", "asr.beam_size", "wide"},
		{"above the range", "vad.threshold", 5},
		{"below the range", "vad.threshold", -1},
		{"not a whole number", "asr.beam_size", 2.5},
		{"a list naming something absent", "subtitle.formats", []any{"srt", "docx"}},
		{"a bare string for a number", "asr.beam_size", "abc"},
		{"the wrong type entirely", "subtitle.bilingual", "yes"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := service.Set(ctx, map[string]any{tc.key: tc.value})
			if !errors.Is(err, ErrInvalidValue) {
				t.Fatalf("err = %v, want ErrInvalidValue", err)
			}
		})
	}
}

// One bad value in a batch means none of them are stored.
//
// The interface saves a form, and half a form applied is a configuration
// nobody chose — with the added harm that the values which did apply are
// indistinguishable from the one that did not.
func TestSetIsAllOrNothing(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	err := service.Set(ctx, map[string]any{
		"asr.model":     "large-v3",
		"vad.threshold": 99,
	})
	if err == nil {
		t.Fatal("a batch with an invalid value was accepted")
	}

	stored, err := service.All(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 0 {
		t.Fatalf("stored = %#v, want nothing", stored)
	}
}

func TestDeleteReturnsTheKeyToTheLayersBelow(t *testing.T) {
	service := newService(t)
	ctx := context.Background()

	if err := service.Set(ctx, map[string]any{"asr.model": "large-v3"}); err != nil {
		t.Fatal(err)
	}

	overridden, err := service.Overridden(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !overridden["asr.model"] {
		t.Fatal("the key is not reported as overridden")
	}

	if err := service.Delete(ctx, "asr.model"); err != nil {
		t.Fatal(err)
	}

	document, err := service.All(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(document) != 0 {
		t.Fatalf("stored = %#v, want nothing after the reset", document)
	}
}

// A key set to the value it already had is still an override.
//
// Which is why Overridden is its own query rather than a comparison against
// the resolved configuration: the two are the same value, and the row is still
// the reason resetting does something.
func TestOverriddenDistinguishesFromTheResolvedValue(t *testing.T) {
	service := newService(t)

	if err := service.Set(context.Background(), map[string]any{"asr.language": ""}); err != nil {
		t.Fatal(err)
	}

	overridden, err := service.Overridden(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !overridden["asr.language"] {
		t.Error("a key set to the empty string is not reported as overridden")
	}
}
