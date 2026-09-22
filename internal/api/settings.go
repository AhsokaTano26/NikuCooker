package api

import (
	"net/http"
	"strings"

	"github.com/AhsokaTano26/NikuCooker/internal/settings"
)

// settingView is one editable setting, as the form needs it.
//
// The label and the help text travel with the value so that the interface
// renders what the server says the setting means. A frontend with its own copy
// of the names would keep showing them after the meaning changed, and nobody
// would notice until a user acted on a stale description.
type settingView struct {
	Key  string `json:"key"`
	Name string `json:"name"`
	Help string `json:"help"`

	Group string `json:"group"`

	Kind    string       `json:"kind"`
	Options []optionView `json:"options,omitempty"`

	Unit string  `json:"unit,omitempty"`
	Min  float64 `json:"min,omitempty"`
	Max  float64 `json:"max,omitempty"`
	Step float64 `json:"step,omitempty"`

	Advanced bool `json:"advanced"`

	// Value is the value in effect right now, after every layer.
	Value any `json:"value"`

	// Source names the layer that set it: "default", "config_file",
	// "environment", "database", "project" or "cli".
	Source string `json:"source"`

	// Overridden reports that the database holds a value for this key, which
	// is what makes the reset control meaningful. It is not derivable from
	// Source being "database": a key the interface set to the same value the
	// default already had still has a row, and clearing it is still the way to
	// stop overriding whatever is underneath.
	Overridden bool `json:"overridden"`
}

type optionView struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

// settingsBody is the request shape for changing settings.
type settingsBody struct {
	// Values maps a dotted configuration path to its new value. All of them are
	// applied or none are: a half-saved form is a configuration nobody chose.
	Values map[string]any `json:"values"`
}

func (s *Server) updateSettings(w http.ResponseWriter, r *http.Request) {
	var body settingsBody
	if apiErr := decode(r, &body); apiErr != nil {
		s.fail(w, apiErr)
		return
	}

	if len(body.Values) == 0 {
		s.fail(w, Invalid("no settings were given; expected {\"values\": {...}}"))
		return
	}

	// Stored and re-resolved together. A write the configuration then rejects —
	// an impossible combination of values, say — is reported here rather than
	// stored and applied at the next restart, where nobody would connect it to
	// what they changed.
	if err := s.app.UpdateSettings(r.Context(), body.Values); err != nil {
		s.fail(w, classify(err))
		return
	}

	s.emitSettingChanged("settings")
	s.respond(w, http.StatusOK, s.settingsResponse(r))
}

func (s *Server) deleteSetting(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	if key == "" {
		s.fail(w, Invalid("a setting key is required"))
		return
	}

	if err := s.app.ClearSetting(r.Context(), key); err != nil {
		s.fail(w, classify(err))
		return
	}

	s.emitSettingChanged("settings")
	w.WriteHeader(http.StatusNoContent)
}

// settingsResponse assembles the whole settings payload.
func (s *Server) settingsResponse(r *http.Request) settingsView {
	cfg := s.app.Config()

	config := redactedConfig(cfg)

	provenance := map[string]string{}
	for key, source := range s.app.Provenance() {
		provenance[key] = string(source)
	}

	// A key no layer set is showing its default, and saying so is the whole
	// point of this endpoint. Leaving it absent would make every client
	// reimplement the same rule — and the one that forgets shows "not set" for
	// a value that is working perfectly.
	fillDefaults(config, "", provenance)

	overridden := map[string]bool{}
	if s.app.Settings != nil {
		if stored, err := s.app.Settings.Overridden(r.Context()); err == nil {
			overridden = stored
		}
	}

	return settingsView{
		Config:           config,
		Provenance:       provenance,
		DataDir:          s.app.DataDir(),
		ConfigPath:       s.app.ConfigPath(),
		ConfigFileExists: s.app.ConfigFileExists(),
		Catalog:          catalogView(config, provenance, overridden),
	}
}

// catalogView joins the catalog to the values in effect.
func catalogView(config map[string]any, provenance map[string]string, overridden map[string]bool) []settingView {
	views := make([]settingView, 0, len(settings.Catalog))

	for _, setting := range settings.Catalog {
		options := make([]optionView, 0, len(setting.Options))
		for _, option := range setting.Options {
			options = append(options, optionView{Value: option.Value, Label: option.Label})
		}

		value, _ := valueAt(config, setting.Key)

		views = append(views, settingView{
			Key:        setting.Key,
			Name:       setting.Name,
			Help:       setting.Help,
			Group:      setting.Group,
			Kind:       string(setting.Kind),
			Options:    options,
			Unit:       setting.Unit,
			Min:        setting.Min,
			Max:        setting.Max,
			Step:       setting.Step,
			Advanced:   setting.Advanced,
			Value:      value,
			Source:     provenance[setting.Key],
			Overridden: overridden[setting.Key],
		})
	}
	return views
}

// valueAt walks a dotted path through the configuration document.
func valueAt(document map[string]any, key string) (any, bool) {
	var current any = document

	for _, part := range strings.Split(key, ".") {
		nested, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		current, ok = nested[part]
		if !ok {
			return nil, false
		}
	}
	return current, true
}
