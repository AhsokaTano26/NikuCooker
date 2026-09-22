package settings

import (
	"strings"
	"testing"

	"github.com/AhsokaTano26/NikuCooker/internal/config"
)

// Every key the interface offers must resolve to a real configuration field.
//
// The catalog names keys as strings, so renaming a field in the config struct
// leaves this list pointing at nothing — and a setting that silently does
// nothing when changed is worse than one that is absent, because the user has
// no way to tell the difference between "applied" and "ignored".
func TestCatalogKeysExistInTheConfig(t *testing.T) {
	document, err := config.Default().AsMap()
	if err != nil {
		t.Fatal(err)
	}

	for _, setting := range Catalog {
		parts := strings.Split(setting.Key, ".")
		current := any(document)

		for i, part := range parts {
			nested, ok := current.(map[string]any)
			if !ok {
				t.Errorf("%s: %q is not a section", setting.Key, strings.Join(parts[:i], "."))
				break
			}
			value, ok := nested[part]
			if !ok {
				t.Errorf("%s: no such key in the configuration", setting.Key)
				break
			}
			current = value
		}
	}
}
