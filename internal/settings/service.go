package settings

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/AhsokaTano26/NikuCooker/internal/database"
)

// Service reads and writes the configuration the web interface owns.
type Service struct {
	db *database.DB
}

// NewService binds the service to a database.
func NewService(db *database.DB) *Service {
	return &Service{db: db}
}

var (
	// ErrUnknownKey reports a key the interface does not offer.
	ErrUnknownKey = errors.New("settings: unknown key")

	// ErrInvalidValue reports a value the key cannot hold.
	ErrInvalidValue = errors.New("settings: invalid value")
)

// All returns the stored overrides, nested by document path.
//
// Nested rather than a flat map because that is what a configuration layer is:
// `config.Layer` takes a partial document, and building the document from the
// dotted keys is this function's job rather than every caller's.
func (s *Service) All(ctx context.Context) (map[string]any, error) {
	rows, err := s.db.Read.QueryContext(ctx, `SELECT key, value_json FROM settings`)
	if err != nil {
		return nil, fmt.Errorf("settings: read: %w", err)
	}
	defer func() { _ = rows.Close() }()

	document := map[string]any{}

	for rows.Next() {
		var key, raw string
		if err := rows.Scan(&key, &raw); err != nil {
			return nil, fmt.Errorf("settings: scan: %w", err)
		}

		var value any
		if err := json.Unmarshal([]byte(raw), &value); err != nil {
			// A row nobody can read is skipped rather than failing the whole
			// document. Losing one setting back to its default is a smaller
			// problem than a server that will not start — and the row is
			// reachable through the interface, which is where it gets fixed.
			continue
		}

		setPath(document, key, value)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("settings: read: %w", err)
	}
	return document, nil
}

// Set validates and stores a batch of values.
//
// A batch rather than one key at a time because the interface saves a form:
// half a form applied is a configuration nobody chose, and the failure would
// be discovered later, on a run, by which point the settings that did apply
// are indistinguishable from the ones that did not.
func (s *Service) Set(ctx context.Context, values map[string]any) error {
	if len(values) == 0 {
		return nil
	}

	coerced := make(map[string]any, len(values))
	for key, value := range values {
		setting, ok := Lookup(key)
		if !ok {
			return fmt.Errorf("%w: %s", ErrUnknownKey, key)
		}

		checked, err := coerce(setting, value)
		if err != nil {
			return err
		}
		coerced[key] = checked
	}

	tx, err := s.db.Write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("settings: write: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	now := time.Now().UTC().Format(time.RFC3339Nano)

	// Sorted so that a batch writes in a stable order. Nothing depends on it,
	// but the failure is easier to read when it is not in map order.
	keys := make([]string, 0, len(coerced))
	for key := range coerced {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	for _, key := range keys {
		encoded, err := json.Marshal(coerced[key])
		if err != nil {
			return fmt.Errorf("settings: encode %s: %w", key, err)
		}

		if _, err := tx.ExecContext(ctx, `
			INSERT INTO settings (key, value_json, updated_at) VALUES (?, ?, ?)
			ON CONFLICT(key) DO UPDATE SET value_json = excluded.value_json,
			                               updated_at = excluded.updated_at`,
			key, string(encoded), now); err != nil {
			return fmt.Errorf("settings: write %s: %w", key, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("settings: write: %w", err)
	}
	return nil
}

// Delete drops an override, returning the key to whatever the layers below say.
//
// Deleting a key that is not set is not an error: the caller is stating an
// intent, and the outcome it wants — this key is no longer overridden — is
// already true.
func (s *Service) Delete(ctx context.Context, key string) error {
	if _, ok := Lookup(key); !ok {
		return fmt.Errorf("%w: %s", ErrUnknownKey, key)
	}

	if _, err := s.db.Write.ExecContext(ctx, `DELETE FROM settings WHERE key = ?`, key); err != nil {
		return fmt.Errorf("settings: delete %s: %w", key, err)
	}
	return nil
}

// Overridden reports which keys the database currently sets.
//
// Read separately from All because the interface needs the set of keys without
// the values, and comparing a rebuilt document against the resolved one to
// work it out would get the "set to its default value on purpose" case wrong.
func (s *Service) Overridden(ctx context.Context) (map[string]bool, error) {
	rows, err := s.db.Read.QueryContext(ctx, `SELECT key FROM settings`)
	if err != nil {
		return nil, fmt.Errorf("settings: read: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := map[string]bool{}
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, fmt.Errorf("settings: scan: %w", err)
		}
		out[key] = true
	}
	return out, rows.Err()
}

// coerce checks a value against its setting and returns it in the form the
// configuration expects.
//
// The value arrives from JSON, where every number is a float and every list is
// []any. Converting here rather than at the point of use means the layer handed
// to config.Load holds real ints and []string, which is what its YAML
// round-trip does and does not undo.
func coerce(setting Setting, value any) (any, error) {
	fail := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s: %s", ErrInvalidValue, setting.Key, fmt.Sprintf(format, args...))
	}

	switch setting.Kind {
	case KindBool:
		boolean, ok := value.(bool)
		if !ok {
			return nil, fail("expected true or false, got %s", describe(value))
		}
		return boolean, nil

	case KindInt:
		number, err := asNumber(value)
		if err != nil {
			return nil, fail("%s", err)
		}
		if number != math.Trunc(number) {
			return nil, fail("expected a whole number, got %v", number)
		}
		if err := checkBounds(setting, number); err != nil {
			return nil, fail("%s", err)
		}
		return int(number), nil

	case KindBytes:
		number, err := asNumber(value)
		if err != nil {
			return nil, fail("%s", err)
		}
		if err := checkBounds(setting, number); err != nil {
			return nil, fail("%s", err)
		}
		return int64(number), nil

	case KindFloat:
		number, err := asNumber(value)
		if err != nil {
			return nil, fail("%s", err)
		}
		if err := checkBounds(setting, number); err != nil {
			return nil, fail("%s", err)
		}
		return number, nil

	case KindString:
		text, ok := value.(string)
		if !ok {
			return nil, fail("expected text, got %s", describe(value))
		}
		return strings.TrimSpace(text), nil

	case KindEnum:
		text, ok := value.(string)
		if !ok {
			return nil, fail("expected one of %s, got %s", optionList(setting), describe(value))
		}
		text = strings.TrimSpace(text)
		if !hasOption(setting, text) {
			return nil, fail("expected one of %s, got %q", optionList(setting), text)
		}
		return text, nil

	case KindList:
		items, err := asStringList(value)
		if err != nil {
			return nil, fail("%s", err)
		}

		// Every member checked against the options, for the same reason an enum
		// is: a list naming something that does not exist is a setting that
		// silently does nothing.
		seen := map[string]bool{}
		out := make([]string, 0, len(items))
		for _, item := range items {
			if len(setting.Options) > 0 && !hasOption(setting, item) {
				return nil, fail("expected only %s, got %q", optionList(setting), item)
			}
			if seen[item] {
				continue
			}
			seen[item] = true
			out = append(out, item)
		}
		return out, nil

	default:
		return nil, fail("this setting cannot be edited")
	}
}

func checkBounds(setting Setting, number float64) error {
	if math.IsNaN(number) || math.IsInf(number, 0) {
		return errors.New("expected a finite number")
	}
	if setting.Min != 0 && number < setting.Min {
		return fmt.Errorf("must be at least %v", setting.Min)
	}
	if setting.Max != 0 && number > setting.Max {
		return fmt.Errorf("must be at most %v", setting.Max)
	}
	return nil
}

func asNumber(value any) (float64, error) {
	switch typed := value.(type) {
	case float64:
		return typed, nil
	case int:
		return float64(typed), nil
	case int64:
		return float64(typed), nil
	case json.Number:
		return typed.Float64()
	case string:
		// A form sends text for a number field that was left showing its
		// default, and rejecting that would be pedantic.
		number, err := strconv.ParseFloat(strings.TrimSpace(typed), 64)
		if err != nil {
			return 0, fmt.Errorf("expected a number, got %q", typed)
		}
		return number, nil
	default:
		return 0, fmt.Errorf("expected a number, got %s", describe(value))
	}
}

func asStringList(value any) ([]string, error) {
	switch typed := value.(type) {
	case []string:
		return typed, nil
	case []any:
		out := make([]string, 0, len(typed))
		for _, item := range typed {
			text, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("expected text, got %s", describe(item))
			}
			out = append(out, strings.TrimSpace(text))
		}
		return out, nil
	case string:
		// Comma-separated, for a field the form renders as a text input.
		parts := []string{}
		for _, part := range strings.Split(typed, ",") {
			if trimmed := strings.TrimSpace(part); trimmed != "" {
				parts = append(parts, trimmed)
			}
		}
		return parts, nil
	default:
		return nil, fmt.Errorf("expected a list, got %s", describe(value))
	}
}

func hasOption(setting Setting, value string) bool {
	for _, option := range setting.Options {
		if option.Value == value {
			return true
		}
	}
	return false
}

func optionList(setting Setting) string {
	values := make([]string, 0, len(setting.Options))
	for _, option := range setting.Options {
		values = append(values, option.Value)
	}
	return strings.Join(values, ", ")
}

// describe names a value's type for an error message.
//
// The Go type name would be accurate and useless: the reader is looking at a
// form, not at a Go program.
func describe(value any) string {
	switch value.(type) {
	case nil:
		return "nothing"
	case bool:
		return "true/false"
	case float64, int, int64, json.Number:
		return "a number"
	case string:
		return "text"
	case []any, []string:
		return "a list"
	default:
		return "an unsupported value"
	}
}

// Merge returns a copy of a settings document with values applied.
//
// Exported for the caller that has to resolve the configuration *before*
// storing anything: a value that the configuration then rejects must not reach
// the database, because the database is read at startup and a rejected value
// there is a server that will not start — with no way back through the
// interface that needs the server.
func Merge(document map[string]any, values map[string]any) map[string]any {
	merged := make(map[string]any, len(document)+len(values))
	for key, value := range document {
		merged[key] = value
	}
	for key, value := range values {
		setPath(merged, key, value)
	}
	return merged
}

// Without returns a copy of a settings document with one path removed.
func Without(document map[string]any, key string) map[string]any {
	parts := strings.Split(key, ".")

	// Rebuilt from the stored rows rather than by deleting through the nested
	// map, because removing a leaf leaves its empty parent behind — and an
	// empty section is not the same document as no section, to a decoder that
	// rejects unknown and malformed keys alike.
	out := map[string]any{}
	for existing, value := range document {
		if existing == parts[0] {
			continue
		}
		out[existing] = value
	}
	return out
}

// setPath writes a value into a nested document, creating sections.
func setPath(document map[string]any, key string, value any) {
	parts := strings.Split(key, ".")
	current := document

	for _, part := range parts[:len(parts)-1] {
		next, ok := current[part].(map[string]any)
		if !ok {
			next = map[string]any{}
			current[part] = next
		}
		current = next
	}
	current[parts[len(parts)-1]] = value
}
