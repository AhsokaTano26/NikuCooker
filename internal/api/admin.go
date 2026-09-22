package api

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/AhsokaTano26/NikuCooker/internal/events"
	"github.com/AhsokaTano26/NikuCooker/internal/provider"
)

// ---------------------------------------------------------------------------
// Models
// ---------------------------------------------------------------------------

func (s *Server) listModels(w http.ResponseWriter, r *http.Request) {
	records, err := s.app.Models.List(r.Context())
	if err != nil {
		s.fail(w, classify(err))
		return
	}

	views := make([]modelView, 0, len(records))
	for _, record := range records {
		views = append(views, modelView{
			ID: record.ID, Name: record.Name, Kind: string(record.Kind),
			Status:    string(record.Status),
			SizeBytes: record.SizeBytes, EstimatedBytes: record.ApproxBytes,
			Progress: record.Progress, Note: record.Note,
			ErrorMessage: record.ErrorMessage, InstalledAt: record.InstalledAt,
		})
	}

	s.respond(w, http.StatusOK, struct {
		Items []modelView `json:"items"`
	}{Items: views})
}

// resolveModel accepts a bare name ("large-v3") or a full id ("asr:large-v3").
func (s *Server) resolveModel(r *http.Request) (string, *Error) {
	name := r.PathValue("modelID")
	if name == "" {
		return "", Invalid("a model is required")
	}

	records, err := s.app.Models.List(r.Context())
	if err != nil {
		return "", classify(err)
	}

	for _, record := range records {
		if record.ID == name || record.Name == name {
			return record.ID, nil
		}
	}

	names := make([]string, 0, len(records))
	for _, record := range records {
		names = append(names, record.Name)
	}
	// Built directly rather than through NotFound, which appends "was not
	// found" to whatever it is given — producing a sentence that ends with the
	// list of what does exist followed by a statement that it does not.
	return "", Failed(http.StatusNotFound, CodeNotFound,
		"no model named "+name+"; available: "+strings.Join(names, ", "))
}

func (s *Server) downloadModel(w http.ResponseWriter, r *http.Request) {
	id, apiErr := s.resolveModel(r)
	if apiErr != nil {
		s.fail(w, apiErr)
		return
	}

	// Started in the background and reported over the event stream. A download
	// of several gigabytes held open as a request would be killed by any proxy
	// in front of it, and gives the interface nothing to show while it waits.
	go s.runDownload(id)

	w.WriteHeader(http.StatusAccepted)
}

// runDownload fetches a model, publishing progress as it goes.
func (s *Server) runDownload(id string) {
	// Detached from any request: the client that asked for the download may
	// well close the tab, and the model should still arrive.
	ctx := contextWithoutCancel()

	name := id
	if record, err := s.app.Models.Get(ctx, id); err == nil {
		name = record.Name
	}

	s.app.Events.Emit(events.New(events.TypeModelProgress).
		With("model", name).With("status", "downloading").With("progress", 0.0))

	var lastEmitted float64
	err := s.app.Models.Download(ctx, id, func(written, total int64) {
		if total <= 0 {
			return
		}
		fraction := float64(written) / float64(total)

		// Coalesced to whole percents. A download reports per chunk, which for a
		// gigabyte is thousands of calls, and each one would be an event on a
		// stream every open tab is reading.
		if fraction-lastEmitted < 0.01 && fraction < 1 {
			return
		}
		lastEmitted = fraction

		s.app.Events.Emit(events.New(events.TypeModelProgress).
			With("model", name).
			With("status", "downloading").
			With("progress", fraction).
			With("written_bytes", written).
			With("total_bytes", total))
	})

	if err != nil {
		s.log.Warn("model download failed", "model", name, "error", err)
		s.app.Events.Emit(events.New(events.TypeModelProgress).
			With("model", name).
			With("status", "failed").
			With("error_message", err.Error()))
		return
	}

	s.app.Events.Emit(events.New(events.TypeModelProgress).
		With("model", name).With("status", "ready").With("progress", 1.0))
}

func (s *Server) deleteModel(w http.ResponseWriter, r *http.Request) {
	id, apiErr := s.resolveModel(r)
	if apiErr != nil {
		s.fail(w, apiErr)
		return
	}

	// `inUse` is passed as false because deleting the weights of a model a run
	// is currently using would fail that run at an unpredictable point. The
	// scheduler is what knows whether anything is running.
	inUse := s.app.Scheduler.AnyRunning()

	if err := s.app.Models.Delete(r.Context(), id, inUse); err != nil {
		if inUse {
			s.fail(w, conflict(CodeConflict,
				"a run is in progress; deleting a model it may be using would fail that run"))
			return
		}
		s.fail(w, classify(err))
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// Providers
// ---------------------------------------------------------------------------

func (s *Server) listProviders(w http.ResponseWriter, r *http.Request) {
	records, err := s.app.Providers.List(r.Context(), "")
	if err != nil {
		s.fail(w, classify(err))
		return
	}

	views := make([]providerView, 0, len(records))
	for _, record := range records {
		views = append(views, providerViewOf(&record))
	}

	s.respond(w, http.StatusOK, struct {
		Items []providerView `json:"items"`
	}{Items: views})
}

// providerBody is the request shape for creating or changing a provider.
type providerBody struct {
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	Type    string `json:"type"`
	BaseURL string `json:"base_url"`

	// APIKey is accepted and never returned. An empty value on an existing
	// provider leaves the stored key alone, because the interface cannot
	// round-trip a value it is not allowed to read.
	APIKey string `json:"api_key"`

	Model   string `json:"model"`
	Enabled *bool  `json:"enabled"`
}

func (s *Server) createProvider(w http.ResponseWriter, r *http.Request) {
	var body providerBody
	if apiErr := decode(r, &body); apiErr != nil {
		s.fail(w, apiErr)
		return
	}

	record := &provider.Provider{
		Name:    body.Name,
		Kind:    provider.Kind(body.Kind),
		Type:    provider.Type(body.Type),
		BaseURL: body.BaseURL,
		APIKey:  body.APIKey,
		Model:   body.Model,
		Enabled: true,
	}
	if body.Enabled != nil {
		record.Enabled = *body.Enabled
	}

	if err := s.app.Providers.Save(r.Context(), record); err != nil {
		s.fail(w, Invalid(err.Error()).Wrap(err))
		return
	}

	s.emitSettingChanged("providers")
	s.respond(w, http.StatusCreated, providerViewOf(record))
}

func (s *Server) updateProvider(w http.ResponseWriter, r *http.Request) {
	existing, err := s.app.Providers.Get(r.Context(), r.PathValue("providerID"))
	if err != nil {
		s.fail(w, classify(err))
		return
	}

	var body providerBody
	if apiErr := decode(r, &body); apiErr != nil {
		s.fail(w, apiErr)
		return
	}

	// Merged onto the stored record, so a request that names three fields does
	// not clear the rest. The key is the reason this matters most: the
	// interface never receives it, so a "save" that sent everything it knew
	// would blank it.
	if body.Name != "" {
		existing.Name = body.Name
	}
	if body.Kind != "" {
		existing.Kind = provider.Kind(body.Kind)
	}
	if body.Type != "" {
		existing.Type = provider.Type(body.Type)
	}
	if body.BaseURL != "" {
		existing.BaseURL = body.BaseURL
	}
	if body.APIKey != "" {
		existing.APIKey = body.APIKey
	}
	if body.Model != "" {
		existing.Model = body.Model
	}
	if body.Enabled != nil {
		existing.Enabled = *body.Enabled
	}

	if err := s.app.Providers.Save(r.Context(), existing); err != nil {
		s.fail(w, Invalid(err.Error()).Wrap(err))
		return
	}

	s.emitSettingChanged("providers")
	s.respond(w, http.StatusOK, providerViewOf(existing))
}

func (s *Server) deleteProvider(w http.ResponseWriter, r *http.Request) {
	if err := s.app.Providers.Delete(r.Context(), r.PathValue("providerID")); err != nil {
		s.fail(w, classify(err))
		return
	}

	s.emitSettingChanged("providers")
	w.WriteHeader(http.StatusNoContent)
}

// testProvider asks the endpoint whether it answers.
//
// Worth a round trip of its own: the failure it catches — a wrong key, a model
// name the provider does not serve, a base URL missing its version segment —
// otherwise surfaces in the middle of a translation, after the transcription
// has already been paid for.
func (s *Server) testProvider(w http.ResponseWriter, r *http.Request) {
	record, err := s.app.Providers.Get(r.Context(), r.PathValue("providerID"))
	if err != nil {
		s.fail(w, classify(err))
		return
	}

	if record.Type != provider.TypeOpenAICompatible {
		s.respond(w, http.StatusOK, providerTestView{
			OK:      false,
			Message: "only OpenAI-compatible providers can be tested here",
		})
		return
	}

	client, err := provider.NewClient(record, provider.ClientOptions{Timeout: aTestTimeout})
	if err != nil {
		s.respond(w, http.StatusOK, providerTestView{OK: false, Message: err.Error()})
		return
	}

	reply, err := client.Chat(r.Context(), provider.ChatRequest{
		User: "Reply with the single word: ok",
		// Small, but not arbitrarily small. The answer is one word, and the
		// budget is not sized for the answer — it is sized for a model that
		// thinks before it emits one, because on those the answer arrives after
		// the thinking and runs out of room first.
		//
		// This is a ceiling, not a target: a model with nothing to think about
		// stops long before it, so raising it does not slow the ordinary case
		// down.
		MaxTokens: provider.ReasoningTokenFloor,
	})

	if err != nil && !errors.Is(err, provider.ErrTruncated) {
		// Reported as a 200 with ok:false rather than as an HTTP error. The
		// request succeeded and its answer is "your provider is misconfigured",
		// which is a result the interface renders, not a transport failure.
		s.respond(w, http.StatusOK, providerTestView{OK: false, Message: err.Error()})
		return
	}

	// `reply` is nil whenever there was an error, truncation included: Chat
	// returns the response only on success. The configured model is what to
	// name in that case, which is also the more useful answer — it is what the
	// user will have to change if the endpoint is serving something else.
	model := record.Model
	if reply != nil && reply.Model != "" {
		model = reply.Model
	}

	view := providerTestView{OK: true, Message: "the provider answered", Model: model}

	// A reply that ran out of room still proves everything this check exists to
	// prove: the address is right, the key is accepted, the model is served.
	// Reporting it as a failure sent users to fix a provider that was working.
	//
	// It is not nothing either, so it is carried as a warning rather than
	// swallowed: a model that spends the whole budget thinking behaves
	// differently from one that does not, and the translation stage's budget is
	// scaled to its batch rather than fixed, which is the difference worth
	// knowing about before a run rather than during one.
	if errors.Is(err, provider.ErrTruncated) {
		view.Warning = fmt.Sprintf(
			"the reply used the whole %d-token check budget before finishing, which is normal for "+
				"a model that reasons before it answers. Translation scales its budget to the batch, "+
				"so this is worth knowing rather than fixing.",
			provider.ReasoningTokenFloor)
	}

	s.respond(w, http.StatusOK, view)
}

// ---------------------------------------------------------------------------
// Settings and logs
// ---------------------------------------------------------------------------

func (s *Server) getSettings(w http.ResponseWriter, r *http.Request) {
	config := redactedConfig(s.app.Config())

	provenance := map[string]string{}
	for key, source := range s.app.Provenance() {
		provenance[key] = string(source)
	}

	// A key no layer set is showing its default, and saying so is the whole
	// point of this endpoint. Leaving it absent would make every client
	// reimplement the same rule — and the one that forgets shows "not set" for
	// a value that is working perfectly.
	fillDefaults(config, "", provenance)

	s.respond(w, http.StatusOK, settingsView{
		Config:           config,
		Provenance:       provenance,
		DataDir:          s.app.DataDir(),
		ConfigPath:       s.app.ConfigPath(),
		ConfigFileExists: s.app.ConfigFileExists(),
	})
}

func (s *Server) listLogs(w http.ResponseWriter, r *http.Request) {
	after, apiErr := queryInt(r, "after", 0)
	if apiErr != nil {
		s.fail(w, apiErr)
		return
	}
	limit, apiErr := queryInt(r, "limit", 500)
	if apiErr != nil {
		s.fail(w, apiErr)
		return
	}

	if s.app.Logs == nil {
		s.respond(w, http.StatusOK, struct {
			Items []any `json:"items"`
			Seq   int64 `json:"seq"`
		}{Items: []any{}})
		return
	}

	records := s.app.Logs.Tail(int64(after), limit)

	s.respond(w, http.StatusOK, struct {
		Items []logRecordView `json:"items"`
		Seq   int64           `json:"seq"`
	}{
		Items: toLogViews(records),
		Seq:   s.app.Logs.CurrentSeq(),
	})
}

// fillDefaults records "default" for every configuration leaf no layer set.
func fillDefaults(value any, prefix string, provenance map[string]string) {
	switch typed := value.(type) {
	case map[string]any:
		for key, nested := range typed {
			path := key
			if prefix != "" {
				path = prefix + "." + key
			}
			fillDefaults(nested, path, provenance)
		}
	case []any:
		// A list is a leaf: the provenance records the whole key, not its
		// elements.
		if _, ok := provenance[prefix]; !ok && prefix != "" {
			provenance[prefix] = "default"
		}
	default:
		if _, ok := provenance[prefix]; !ok && prefix != "" {
			provenance[prefix] = "default"
		}
	}
}

// emitSettingChanged announces that a stored setting moved.
func (s *Server) emitSettingChanged(what string) {
	s.app.Events.Emit(events.New(events.TypeSettingsChanged).With("changed", []string{what}))
}
