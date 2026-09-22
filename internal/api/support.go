package api

import (
	"context"
	"strings"
	"time"

	"github.com/AhsokaTano26/NikuCooker/internal/config"
	"github.com/AhsokaTano26/NikuCooker/internal/logging"
	"github.com/AhsokaTano26/NikuCooker/internal/provider"
)

// aTestTimeout bounds a provider check.
//
// Short, because a user is watching a spinner and the answer is binary. A slow
// generation would prove nothing that a fast one does not.
const aTestTimeout = 20 * time.Second

// contextWithoutCancel returns a context for work that must outlive its request.
//
// A model download is the case: the client that asked for it may close the tab,
// and several gigabytes should still arrive.
func contextWithoutCancel() context.Context { return context.WithoutCancel(context.Background()) }

// providerViewOf renders a provider record.
func providerViewOf(record *provider.Provider) providerView {
	return providerView{
		ID: record.ID, Name: record.Name,
		Kind: string(record.Kind), Type: string(record.Type),
		BaseURL: record.BaseURL, Model: record.Model,
		Enabled:   record.Enabled,
		HasKey:    record.APIKey != "",
		CreatedAt: record.CreatedAt, UpdatedAt: record.UpdatedAt,
	}
}

// toLogViews converts buffered records for the wire.
func toLogViews(records []logging.Record) []logRecordView {
	views := make([]logRecordView, 0, len(records))
	for _, record := range records {
		views = append(views, logRecordView{
			Seq: record.Seq, Time: record.Time,
			Level: record.Level, Msg: record.Msg, Attrs: record.Attrs,
		})
	}
	return views
}

// redactedConfig renders the configuration with secrets removed.
//
// By key name rather than by an explicit list of paths. A list is a thing to
// forget when a provider field is added, and the failure mode of forgetting is
// printing a live API key into a web page and the browser's cache.
func redactedConfig(cfg *config.Config) map[string]any {
	raw, err := cfg.AsMap()
	if err != nil {
		return map[string]any{}
	}
	return redact(raw).(map[string]any)
}

// secretNames are the key fragments that mark a value as secret.
var secretNames = []string{"api_key", "apikey", "secret", "token", "password"}

func redact(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, nested := range typed {
			if isSecretName(key) {
				out[key] = redactedPlaceholder(nested)
				continue
			}
			out[key] = redact(nested)
		}
		return out

	case []any:
		out := make([]any, 0, len(typed))
		for _, nested := range typed {
			out = append(out, redact(nested))
		}
		return out

	default:
		return value
	}
}

// isSecretName reports whether a key names something that must not be shown.
func isSecretName(key string) bool {
	lowered := strings.ToLower(key)
	for _, name := range secretNames {
		if strings.Contains(lowered, name) {
			return true
		}
	}
	return false
}

// redactedPlaceholder reports that a secret is set without disclosing it.
//
// An empty string would read as "not configured" for a key that is configured,
// which is worse than saying nothing.
func redactedPlaceholder(value any) any {
	text, _ := value.(string)
	if text == "" {
		return ""
	}
	return "***"
}
