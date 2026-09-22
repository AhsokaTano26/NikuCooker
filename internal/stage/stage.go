// Package stage defines what a pipeline stage is.
//
// A stage declares its identity and dependencies, runs, and writes exactly one
// artifact. It never calls another stage and never imports one: stages
// communicate only through artifacts and the shared data model, which is what
// makes one stage's internals invisible to every other
// (docs/requirements.md FR-PIPE-5).
//
// Concrete stages live in subpackages and are registered in internal/app. This
// package does not import them, so there is no cycle and no registry that knows
// about implementations it does not need.
package stage

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/AhsokaTano26/NikuCooker/internal/artifact"
	"github.com/AhsokaTano26/NikuCooker/internal/config"
	"github.com/AhsokaTano26/NikuCooker/internal/glossary"
	"github.com/AhsokaTano26/NikuCooker/internal/media"
	"github.com/AhsokaTano26/NikuCooker/internal/provider"
	"github.com/AhsokaTano26/NikuCooker/internal/translation"
	"github.com/AhsokaTano26/NikuCooker/pkg/protocol"
)

// Spec is a stage's identity and wiring.
type Spec struct {
	// Name is the stage's stable identifier, used in artifact paths, event
	// payloads and the API. Changing it invalidates that stage's cache.
	Name string

	// Version is the implementation version, bumped by hand when the stage's
	// algorithm changes. It participates in the artifact key, so bumping it
	// invalidates this stage and everything downstream, and nothing upstream.
	//
	// This is the intentional invalidation lever. Forgetting it is covered by
	// the build revision, which is also in the key.
	Version string

	// Depends names the stages whose artifacts this one consumes. The pipeline
	// resolves inputs from these names, so a stage never looks up an artifact by
	// path or by guessing.
	//
	// A missing artifact from one of these makes the stage unrunnable. Where the
	// input is genuinely optional, use OptionalDepends instead: expressing it as
	// a hard dependency forces a choice between skipping a stage when an
	// unrelated one is disabled, and reaching around the artifact system to get
	// at the data another way.
	Depends []string

	// OptionalDepends names stages whose artifacts this one uses when they are
	// available and does without when they are not.
	//
	// The stage is responsible for checking whether the input arrived; nothing
	// guarantees it. An optional dependency that failed or was skipped does not
	// block this stage, and does not appear in Env.Inputs.
	OptionalDepends []string

	// Optional stages are excluded from a run when disabled, and the pipeline
	// renumbers nothing when they are: optional stages own an ordinal in the
	// definition, so disabling one leaves a gap rather than shifting every
	// stage after it.
	Optional bool

	// ConfigKey names the configuration subtree this stage reads, used for the
	// stage's cache key and for the UI's per-stage settings.
	//
	// ConfigSubtree returns the actual value; this field is the label for it.
	ConfigKey string
}

// Stage is one step of the pipeline.
type Stage interface {
	// Spec describes the stage.
	Spec() Spec

	// Run executes the stage.
	//
	// It writes its payload into env.OutDir and returns a description of what
	// it produced. The pipeline publishes the artifact only if Run returns
	// without error, so a partial write is never visible.
	Run(ctx context.Context, env *Env) (*Result, error)

	// ConfigSubtree returns the configuration this stage reads.
	//
	// Only this value is hashed into the artifact key, so changing an unrelated
	// setting — a render CRF, say — does not invalidate transcription.
	ConfigSubtree(cfg *config.Config) any

	// Fingerprint returns stage-specific external inputs that are neither
	// configuration nor an upstream artifact: the context document and glossary
	// for translation, the QC rule set version, an ASS preset's contents.
	//
	// Returning an error is the right response to a missing required input,
	// because a fingerprint that silently omits an input produces a cache key
	// that does not describe the work.
	Fingerprint(ctx context.Context, env *Env) (map[string]string, error)
}

// Env is everything a stage is allowed to reach.
//
// A stage gets no other handles to the outside world: it cannot open the
// database, cannot read another stage's code, and cannot reach a global. That
// is what makes the "stages communicate only through artifacts" rule
// enforceable in review rather than aspirational.
type Env struct {
	// ProjectID and ProjectDir locate the project the stage is running for.
	ProjectID  string
	ProjectDir string
	JobID      string

	// Project carries the project's own facts.
	//
	// They are here rather than in Config because they are not settings: the
	// language a project is in and the style it is translated in are properties
	// of the work, and a stage that read them from configuration would be
	// reading a global default and calling it the project's.
	Project ProjectInfo

	// SourcePath is the absolute path to the project's source media.
	SourcePath string

	// Media is the FFmpeg service. It is the only way a stage reaches FFmpeg,
	// and the only place arguments are constructed.
	Media *media.Service

	// Models resolves a model to its on-disk directory, so the worker loads
	// the copy the core downloaded rather than fetching its own.
	Models ModelLocator

	// Worker runs requests against the Python AI worker.
	//
	// An interface rather than the concrete pool so that a stage can be tested
	// without spawning a process, and so that stage has no dependency on the
	// pool's internals.
	Worker WorkerPool

	// Config is the fully resolved configuration, including this project's
	// overlay.
	Config *config.Config

	// Services are shared collaborators that had to be built by the core.
	//
	// They arrive constructed because constructing them requires the database,
	// which a stage cannot reach — that restriction is what keeps project state
	// in one place. Each is optional; a stage that needs one checks for nil and
	// explains what is missing rather than dereferencing it.
	Services Services

	// Inputs holds the upstream artifacts this stage declared in Spec.Depends,
	// keyed by producer stage name.
	Inputs map[string]*artifact.Artifact

	// Artifacts reads upstream payloads. A stage uses this rather than joining
	// paths, so the on-disk layout stays private to the artifact package.
	Artifacts ArtifactReader

	// OutDir is where the stage writes its payload. It is a temporary
	// directory; the pipeline publishes it on success.
	OutDir string

	// Progress reports completion and a short status. The pipeline throttles
	// and forwards these to the event stream.
	Progress func(fraction float64, message string)

	Log *slog.Logger

	// Clock is injectable so stage behaviour that depends on time can be tested
	// without sleeping.
	Clock Clock

	// SourceFingerprint identifies the source media, computed once by the
	// pipeline. Stages that read the source fold it into their key; stages that
	// do not leave it out.
	SourceFingerprint string

	// Now is when the run started, for elapsed-time reporting.
	StartedAt time.Time
}

// ReportProgress calls the progress callback if one is set.
//
// A stage should use this rather than calling Env.Progress directly, so that a
// stage run outside a job — a test, or a CLI one-off — does not need a callback
// wired up.
func (e *Env) ReportProgress(fraction float64, message string) {
	if e.Progress == nil {
		return
	}
	if fraction < 0 {
		fraction = 0
	}
	if fraction > 1 {
		fraction = 1
	}
	e.Progress(fraction, message)
}

// Result describes what a stage produced.
//
// An alias rather than a parallel type: a stage's output description and the
// artifact that records it are the same thing, and two structurally identical
// types would need a conversion at every call site — which is exactly the kind
// of seam a field added to one and not the other slips through.
//
// Primary names the payload file a consumer should open first. The pipeline
// verifies it exists before publishing, so a stage cannot claim an artifact it
// did not write.
//
// Provider, Model and PromptVersion record provenance, which is what makes
// "which model produced this transcript" answerable a month later.
//
// Metadata carries stage-specific facts: device actually used, token usage,
// counts, timings. Nothing in the pipeline may *depend* on a metadata key —
// that would be an undeclared interface between stages, and it would not be
// part of the cache key.
type Result = artifact.Result

// Services are the database-backed collaborators a stage may use.
type Services struct {
	// Providers resolves a configured language-model endpoint.
	Providers *provider.Service

	// Glossary reads the terminology in scope for a project.
	Glossary *glossary.Service

	// Translation is the line cache, which is global and survives across
	// projects.
	Translation *translation.Cache
}

// ProjectInfo is what a stage needs to know about the project it is serving.
type ProjectInfo struct {
	Name string

	// SourceLanguage and TargetLanguage are BCP-47 tags. An empty source
	// language means the recogniser should detect it.
	SourceLanguage string
	TargetLanguage string

	// Style is "literal", "natural" or "fansub".
	Style string
}

// ModelLocator resolves a model to its on-disk directory.
type ModelLocator interface {
	Resolve(kind string, name string) (string, bool)
}

// LLM returns a client for the configured language-model provider.
//
// A convenience over Services.Providers for the common case of one enabled
// provider, and the place the timeout from configuration is applied.
func (s Services) LLM(ctx context.Context, cfg *config.Config) (*provider.Client, string, error) {
	if s.Providers == nil {
		return nil, "", errors.New("stage: no provider service is available")
	}

	record, err := s.Providers.Default(ctx, provider.KindLLM)
	if err != nil {
		return nil, "", err
	}

	client, err := provider.NewClient(record, provider.ClientOptions{
		Timeout: cfg.Translation.Timeout,
	})
	if err != nil {
		return nil, "", err
	}
	return client, record.Name, nil
}

// WorkerPool runs inference requests.
type WorkerPool interface {
	Call(
		ctx context.Context,
		method string,
		params any,
		onProgress func(protocol.ProgressEvent),
	) ([]byte, error)
}

// ArtifactReader loads artifact payloads.
type ArtifactReader interface {
	// Decode reads an artifact's primary payload into v.
	Decode(a *artifact.Artifact, v any) error
	// Path resolves an artifact's primary payload to an absolute path, for
	// stages that hand the file to an external process.
	Path(a *artifact.Artifact) (string, error)
}

// InputPath resolves the primary payload of a named upstream stage.
func (e *Env) InputPath(producer string) (string, error) {
	a, ok := e.Inputs[producer]
	if !ok || a == nil {
		return "", fmt.Errorf("stage: no artifact from %q", producer)
	}
	if e.Artifacts == nil {
		return "", errors.New("stage: no artifact reader is available")
	}
	return e.Artifacts.Path(a)
}

// ReadInput decodes the primary payload of the artifact produced by a named
// upstream stage.
func (e *Env) ReadInput(producer string, v any) error {
	a, ok := e.Inputs[producer]
	if !ok || a == nil {
		return fmt.Errorf("stage: no artifact from %q; it is declared in Depends but was not supplied", producer)
	}
	if e.Artifacts == nil {
		return errors.New("stage: no artifact reader is available")
	}
	return e.Artifacts.Decode(a, v)
}

// Clock is the subset of time that stages use.
type Clock interface {
	Now() time.Time
}

// SystemClock is the real clock.
type SystemClock struct{}

// Now returns the current time.
func (SystemClock) Now() time.Time { return time.Now() }
