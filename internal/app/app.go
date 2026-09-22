// Package app wires the business core together.
//
// Everything that knows how the pieces fit lives here and only here. The CLI,
// the HTTP server and the tests are all clients of an App: they ask it for a
// project's pipeline and run it, and none of them constructs a service, opens a
// database or registers a stage. That is what makes "the CLI and the API do the
// same thing" a property of the code rather than a promise in a document.
package app

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/AhsokaTano26/NikuCooker/internal/artifact"
	"github.com/AhsokaTano26/NikuCooker/internal/config"
	"github.com/AhsokaTano26/NikuCooker/internal/database"
	"github.com/AhsokaTano26/NikuCooker/internal/events"
	"github.com/AhsokaTano26/NikuCooker/internal/glossary"
	"github.com/AhsokaTano26/NikuCooker/internal/jobs"
	"github.com/AhsokaTano26/NikuCooker/internal/logging"
	"github.com/AhsokaTano26/NikuCooker/internal/media"
	"github.com/AhsokaTano26/NikuCooker/internal/models"
	"github.com/AhsokaTano26/NikuCooker/internal/pipeline"
	"github.com/AhsokaTano26/NikuCooker/internal/platform"
	"github.com/AhsokaTano26/NikuCooker/internal/project"
	"github.com/AhsokaTano26/NikuCooker/internal/provider"
	qcrepo "github.com/AhsokaTano26/NikuCooker/internal/qc"
	"github.com/AhsokaTano26/NikuCooker/internal/segments"
	"github.com/AhsokaTano26/NikuCooker/internal/stage"
	"github.com/AhsokaTano26/NikuCooker/internal/stage/asr"
	"github.com/AhsokaTano26/NikuCooker/internal/stage/audio"
	contextstage "github.com/AhsokaTano26/NikuCooker/internal/stage/context"
	"github.com/AhsokaTano26/NikuCooker/internal/stage/polish"
	"github.com/AhsokaTano26/NikuCooker/internal/stage/probe"
	"github.com/AhsokaTano26/NikuCooker/internal/stage/qc"
	"github.com/AhsokaTano26/NikuCooker/internal/stage/render"
	"github.com/AhsokaTano26/NikuCooker/internal/stage/segmentation"
	stagesubtitle "github.com/AhsokaTano26/NikuCooker/internal/stage/subtitle"
	stagestranslation "github.com/AhsokaTano26/NikuCooker/internal/stage/translation"
	"github.com/AhsokaTano26/NikuCooker/internal/stage/vad"
	translationcore "github.com/AhsokaTano26/NikuCooker/internal/translation"
	"github.com/AhsokaTano26/NikuCooker/internal/worker"
	"github.com/AhsokaTano26/NikuCooker/pkg/protocol"
)

// Options configures New.
type Options struct {
	// ConfigPath is the YAML file to read. An absent file is not an error: the
	// defaults are a complete, working configuration, and a user who has
	// changed nothing should not need to have created one.
	ConfigPath string

	// Flags are values from the command line, the highest-precedence layer.
	Flags map[string]any

	// Environ is the environment to read overrides from. Nil means the process's
	// own, which is what a caller wants everywhere except in a test.
	Environ []string

	// CodeRevision participates in every artifact key. It is passed in rather
	// than read from a package global so that the binary's build information
	// stays in the binary.
	CodeRevision string

	Log *slog.Logger

	// Logs retains recent records for the interface. When nil the application
	// makes its own, so a caller that does not wire one up — a test — still
	// gets a working log endpoint rather than a nil dereference.
	Logs *logging.Buffer
}

// App is the wired business core.
type App struct {
	cfg        *config.Config
	provenance config.Provenance
	db         *database.DB
	log        *slog.Logger

	codeRevision string
	dataDir      string

	Projects  *project.Service
	Glossary  *glossary.Service
	Providers *provider.Service
	Models    *models.Service
	Jobs      *jobs.Repository
	Scheduler *jobs.Scheduler
	Cache     *translationcore.Cache
	Media     *media.Service
	Artifacts *artifactStoreFactory
	Segments  *segments.Repository
	QC        *qcrepo.Repository

	// Events carries live updates to the UI. It is never nil in a running
	// server, and emit tolerates it being nil so that a CLI run — which has
	// nothing subscribed — does not have to construct one.
	Events *events.Bus

	// Logs is a window of recent log records, so the interface can show them
	// without the user finding a terminal.
	Logs *logging.Buffer

	registry *stage.Registry

	// The worker pool is started on first use rather than at construction.
	// A `project list` must not spawn Python processes, and it must not fail
	// because Python is missing.
	workerOnce sync.Once
	worker     *worker.Pool
	workerErr  error

	aiDirOnce sync.Once
	aiDir     string
	aiDirErr  error
}

// New builds the application.
func New(ctx context.Context, opts Options) (*App, error) {
	if opts.Log == nil {
		opts.Log = slog.Default()
	}

	logBuffer := opts.Logs
	if logBuffer == nil {
		logBuffer = logging.NewBuffer(logging.DefaultCapacity)
	}

	environ := opts.Environ
	if environ == nil {
		environ = os.Environ()
	}

	// A flag beats the environment, and the environment beats the default of no
	// file at all. The container image sets NIKUCOOKER_CONFIG, so a mounted
	// configuration has to be found without a flag — but a user who typed
	// --config meant it.
	configPath := opts.ConfigPath
	if configPath == "" {
		configPath = config.ConfigFileFromEnv(environ)
	}

	fileLayer, err := config.FileLayer(configPath)
	if err != nil {
		return nil, err
	}
	envLayer, err := config.EnvLayer(environ)
	if err != nil {
		return nil, err
	}

	layers := []config.Layer{fileLayer, envLayer}
	if len(opts.Flags) > 0 {
		layers = append(layers, config.Layer{Source: config.SourceCLI, Data: opts.Flags})
	}

	cfg, provenance, err := config.Load(layers...)
	if err != nil {
		return nil, err
	}

	if err := os.MkdirAll(cfg.Storage.DataDir, 0o755); err != nil {
		return nil, fmt.Errorf("app: create the data directory %s: %w", cfg.Storage.DataDir, err)
	}

	db, err := database.Open(database.Options{
		Path: filepath.Join(cfg.Storage.DataDir, "nikucooker.db"),
	})
	if err != nil {
		return nil, err
	}

	if _, err := database.Migrate(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}

	mediaService, err := media.New(media.Options{
		FFmpegPath:  cfg.Media.FFmpegPath,
		FFprobePath: cfg.Media.FFprobePath,
		Timeout:     cfg.Media.Timeout,
		Log:         opts.Log,
	})
	if err != nil {
		_ = db.Close()
		return nil, err
	}

	modelService, err := models.New(models.Options{
		DB:       db,
		ModelDir: cfg.Storage.ModelDir,
		Log:      opts.Log,
	})
	if err != nil {
		_ = db.Close()
		return nil, err
	}

	registry, err := stage.NewRegistry(
		probe.New(),
		audio.New(),
		vad.New(),
		asr.New(),
		segmentation.New(),
		contextstage.New(),
		stagestranslation.New(),
		polish.New(),
		qc.New(),
		stagesubtitle.New(),
		render.New(),
	)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("app: %w", err)
	}

	return &App{
		cfg:          cfg,
		provenance:   provenance,
		db:           db,
		log:          opts.Log,
		codeRevision: opts.CodeRevision,
		dataDir:      cfg.Storage.DataDir,
		Projects:     project.NewService(db, cfg.Storage.DataDir),
		Glossary:     glossary.NewService(db),
		Providers:    provider.NewService(db),
		Models:       modelService,
		Jobs:         jobs.NewRepository(db),
		Scheduler:    jobs.NewScheduler(),
		Cache:        translationcore.NewCache(db),
		Media:        mediaService,
		Artifacts:    &artifactStoreFactory{db: db, dataDir: cfg.Storage.DataDir},
		Segments:     segments.NewRepository(db),
		QC:           qcrepo.NewRepository(db),
		Events:       events.NewBus(events.DefaultBufferSize),
		Logs:         logBuffer,
		registry:     registry,
	}, nil
}

// Close releases the resources an App holds.
func (a *App) Close() error {
	if a.worker != nil {
		ctx, cancel := context.WithTimeout(context.Background(), a.cfg.Worker.ShutdownTimeout)
		defer cancel()
		_ = a.worker.Shutdown(ctx)
	}
	if a.db != nil {
		return a.db.Close()
	}
	return nil
}

// Config returns the resolved configuration.
func (a *App) Config() *config.Config { return a.cfg }

// Provenance returns which layer set each configuration key.
func (a *App) Provenance() config.Provenance { return a.provenance }

// DB exposes the database, for callers that need a query the services do not
// offer.
func (a *App) DB() *database.DB { return a.db }

// DataDir reports the data root.
func (a *App) DataDir() string { return a.dataDir }

// Registry returns the pipeline definition.
func (a *App) Registry() *stage.Registry { return a.registry }

// stageServices are the collaborators every stage is handed.
//
// Built in one place so that the runs started by the CLI, by the API and by a
// test all hand stages exactly the same set — a stage that worked from the CLI
// and not from the browser would otherwise be a difference nobody declared.
func (a *App) stageServices() stage.Services {
	return stage.Services{
		Providers:   a.Providers,
		Glossary:    a.Glossary,
		Translation: a.Cache,
		Lines:       a.Segments,
	}
}

// ResolvePython reports the interpreter the worker will be launched with.
//
// Exposed for the doctor command and the dashboard, which both need to answer
// "which Python is this using" without starting one.
func (a *App) ResolvePython(ctx context.Context) (string, error) {
	dir, err := a.AIDir()
	if err != nil {
		return "", err
	}
	return platform.ResolvePython(ctx, platform.ResolveOptions{
		Explicit:      a.cfg.AI.Python,
		AIDir:         dir,
		RequireImport: false,
	})
}

// Logger returns the application logger.
func (a *App) Logger() *slog.Logger { return a.log }

// ---------------------------------------------------------------------------
// Per-project resources
// ---------------------------------------------------------------------------

// artifactStoreFactory builds a store for a project.
//
// A store is scoped to one project's directory, so it cannot be built once at
// startup. It is a factory rather than something the caller constructs so that
// the layout of the projects tree stays in one place.
type artifactStoreFactory struct {
	db      *database.DB
	dataDir string
}

// For returns the artifact store for a project.
func (f *artifactStoreFactory) For(projectID string) *artifact.Store {
	return artifact.NewStore(f.db, projectID, project.Dir(f.dataDir, projectID))
}

// ---------------------------------------------------------------------------
// Worker pool
// ---------------------------------------------------------------------------

// Worker returns the AI worker pool.
//
// The pool is built on first use rather than in New, and it starts no process
// when it is built. The first stage that needs inference acquires a worker, so
// a command that only lists projects never touches Python — and a machine
// without it can still use everything the pipeline does before recognition.
func (a *App) Worker(ctx context.Context) (*worker.Pool, error) {
	a.workerOnce.Do(func() {
		dir, err := a.AIDir()
		if err != nil {
			a.workerErr = err
			return
		}

		// The digest is what the worker checks its own schema against during
		// the handshake. Both sides computing it from the same fixtures is what
		// makes a protocol change a startup failure rather than a
		// misinterpreted message halfway through a job.
		digest, err := protocol.SchemaDigest()
		if err != nil {
			a.workerErr = fmt.Errorf("app: compute the protocol schema digest: %w", err)
			return
		}

		// Resolved against the package directory, which is what makes the
		// virtual environment beside it discoverable.
		python, err := platform.ResolvePython(ctx, platform.ResolveOptions{
			Explicit:      a.cfg.AI.Python,
			AIDir:         dir,
			RequireImport: true,
		})
		if err != nil {
			a.workerErr = err
			return
		}

		// Checked here rather than left to the import. A Python that is the
		// wrong version fails deep inside a dependency with a message about a
		// syntax error or a missing symbol, which reads as a broken install
		// rather than as one wrong setting.
		if version, err := platform.Version(ctx, python); err != nil {
			a.workerErr = err
			return
		} else if constraint, err := platform.ParseConstraint(a.cfg.AI.PythonVersion); err != nil {
			a.workerErr = err
			return
		} else if !constraint.IsEmpty() && !constraint.Allows(version) {
			a.workerErr = fmt.Errorf(
				"app: the AI worker needs Python %s but %s is %s",
				a.cfg.AI.PythonVersion, python, version)
			return
		}

		args := a.cfg.AI.Args
		if len(args) == 0 {
			args = []string{"-m", "nikucooker_ai"}
		}

		a.worker = worker.NewPool(worker.Config{
			Python:           python,
			Args:             args,
			Dir:              dir,
			SchemaDigest:     digest,
			CodeRevision:     a.codeRevision,
			StartupTimeout:   a.cfg.Worker.StartupTimeout,
			ShutdownTimeout:  a.cfg.Worker.ShutdownTimeout,
			ModelLoadTimeout: a.cfg.Worker.ModelLoadTimeout,
			StallTimeout:     a.cfg.Worker.StallTimeout,
			Log:              a.log,
		}, a.cfg.Worker.PoolSize)
	})
	if a.workerErr != nil {
		return nil, a.workerErr
	}
	return a.worker, nil
}

// AIDir returns the directory holding the nikucooker_ai package.
//
// Memoised, and shared with the worker pool: the doctor command reports the
// path the worker will actually be launched from, and a diagnosis that resolved
// it differently from the thing it was diagnosing would be worse than none.
func (a *App) AIDir() (string, error) {
	a.aiDirOnce.Do(func() {
		a.aiDir, a.aiDirErr = resolveAIDir(a.cfg)
	})
	return a.aiDir, a.aiDirErr
}

// resolveAIDir finds the directory holding the nikucooker_ai package.
//
// Configured first, then alongside the executable — which is where a release
// archive puts it — then the working directory, which is where it is in a
// checkout. Failing to find it is reported here rather than at the first
// inference request, because "Python worker not found" is much easier to act on
// before a job has transcribed an hour of audio.
func resolveAIDir(cfg *config.Config) (string, error) {
	var tried []string

	check := func(dir string) (string, bool) {
		if dir == "" {
			return "", false
		}
		// Recorded once. The executable and the working directory are the same
		// when the binary is run from where it sits, and listing the same path
		// twice reads as a bug in the search rather than as two attempts.
		if !slices.Contains(tried, dir) {
			tried = append(tried, dir)
		}
		info, err := os.Stat(filepath.Join(dir, "nikucooker_ai"))
		if err != nil || !info.IsDir() {
			return "", false
		}
		return dir, true
	}

	if dir, ok := check(cfg.AI.Dir); ok {
		return dir, nil
	}
	if exe, err := os.Executable(); err == nil {
		if dir, ok := check(filepath.Join(filepath.Dir(exe), "ai")); ok {
			return dir, nil
		}
	}
	if cwd, err := os.Getwd(); err == nil {
		if dir, ok := check(filepath.Join(cwd, "ai")); ok {
			return dir, nil
		}
	}

	return "", fmt.Errorf(
		"app: cannot find the AI worker package (looked in %s); set ai.dir in the configuration",
		strings.Join(tried, ", "))
}

// ---------------------------------------------------------------------------
// Running the pipeline
// ---------------------------------------------------------------------------

// RunOptions selects what to run.
type RunOptions struct {
	// ProjectID is the project to run.
	ProjectID string

	// JobID identifies this run in the database. Generated when empty.
	JobID string

	// Force ignores the artifact cache and recomputes everything selected.
	Force bool

	// Only restricts the run to these stages. FromStage and ToStage restrict it
	// to an inclusive range by pipeline order.
	Only      []string
	FromStage string
	ToStage   string

	// Observer receives pipeline events. Nil discards them.
	Observer pipeline.Observer
}

// RunResult reports what a run produced.
type RunResult struct {
	JobID string
	Plan  *pipeline.Plan
}

// Run executes the pipeline synchronously, for a caller with nothing else to do.
//
// The CLI uses this: a user watching a terminal wants the command to block until
// the work is done. The HTTP API uses Start, which returns immediately and
// leaves the run to report itself over the event stream.
func (a *App) Run(ctx context.Context, opts RunOptions) (*RunResult, error) {
	prepared, err := a.prepare(ctx, opts)
	if err != nil {
		return nil, err
	}

	jobCtx, err := a.Scheduler.Begin(ctx, prepared.ProjectID, prepared.JobID)
	if err != nil {
		return nil, err
	}

	if err := a.Jobs.Create(ctx, jobFor(prepared.JobID, prepared.ProjectID, opts)); err != nil {
		a.Scheduler.Finish(prepared.ProjectID)
		return nil, err
	}

	// Executed rather than launched: the caller passed an observer for its own
	// progress display, and it expects this call to return when the work is
	// done.
	a.execute(jobCtx, prepared, opts)

	return &RunResult{JobID: prepared.JobID, Plan: prepared.plan}, nil
}

// stageNeedsWorker reports whether any stage in the plan will actually run and
// needs the AI worker.
func stageNeedsWorker(plan *pipeline.Plan) bool {
	for _, sp := range plan.Stages {
		if sp.State != pipeline.StatePending {
			continue
		}
		switch sp.Stage.Spec().Name {
		case "vad", "asr":
			return true
		}
	}
	return false
}

// projectConfig resolves the global configuration with a project's overlay.
func (a *App) projectConfig(prj *project.Project) (*config.Config, error) {
	cfg := a.cfg.Clone()
	if len(prj.Config) == 0 {
		return cfg, nil
	}
	if _, err := cfg.Apply(config.SourceProject, prj.Config); err != nil {
		return nil, fmt.Errorf("app: apply the configuration of project %s: %w", prj.ID, err)
	}
	return cfg, nil
}

// sourcePath resolves a project's source media to an absolute path.
func (a *App) sourcePath(prj *project.Project) (string, error) {
	path := filepath.Join(project.Dir(a.dataDir, prj.ID), filepath.FromSlash(prj.SourcePath))
	if _, err := os.Stat(path); err != nil {
		return "", fmt.Errorf(
			"app: the source media of project %s is missing at %s: %w", prj.ID, path, err)
	}
	return path, nil
}
