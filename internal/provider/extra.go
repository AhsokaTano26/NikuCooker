package provider

import (
	"encoding/json"
	"fmt"
)

// marshalExtra encodes the provider-specific settings map for storage.
func marshalExtra(extra map[string]any) (string, error) {
	if len(extra) == 0 {
		return "{}", nil
	}
	raw, err := json.Marshal(extra)
	if err != nil {
		return "", fmt.Errorf("provider: encode extra settings: %w", err)
	}
	return string(raw), nil
}

// unmarshalExtra decodes the stored settings.
//
// A malformed blob yields an empty map rather than an error. The column has a
// default of '{}' and holds only optional settings, so refusing to load the
// provider because of it would make one bad value hide a working configuration
// with no way to reach the form that could fix it.
func unmarshalExtra(raw string) (map[string]any, error) {
	if raw == "" || raw == "{}" {
		return nil, nil
	}
	var extra map[string]any
	if err := json.Unmarshal([]byte(raw), &extra); err != nil {
		return nil, nil
	}
	return extra, nil
}
