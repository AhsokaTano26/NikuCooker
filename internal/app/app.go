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
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

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
	"github.com/AhsokaTano26/NikuCooker/internal/provision"
	qcrepo "github.com/AhsokaTano26/NikuCooker/internal/qc"
	"github.com/AhsokaTano26/NikuCooker/internal/segments"
	"github.com/AhsokaTano26/NikuCooker/internal/settings"
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

	// LogLevel is the logger's threshold, held by the caller so that changing
	// it in the interface takes effect without a restart. Nil means the logger
	// is fixed for the process, which is what a test wants.
	LogLevel *slog.LevelVar

	// Logs retains recent records for the interface. When nil the application
	// makes its own, so a caller that does not wire one up — a test — still
	// gets a working log endpoint rather than a nil dereference.
	Logs *logging.Buffer
}

// resolved is the configuration and where each of its values came from.
//
// One value rather than two fields because they are read together — the
// settings endpoint joins every key to its source — and a reader that got the
// new configuration with the previous provenance would report a setting as
// coming from the file when it came from the browser.
type resolved struct {
	cfg  *config.Config
	prov config.Provenance
}

// App is the wired business core.
type App struct {
	// config holds the resolved configuration. It is replaced wholesale when a
	// setting changes, so it is read through an atomic rather than a plain
	// field: the HTTP handlers read it on their own goroutines while a job
	// reads it on another, and a bare pointer swap would be a data race.
	//
	// A snapshot is immutable once published. A run takes its own clone when it
	// starts, so a setting changed mid-run applies to the next one and cannot
	// alter the artifact keys of the one in flight.
	config atomic.Pointer[resolved]

	db  *database.DB
	log *slog.Logger

	codeRevision string
	dataDir      string
	configPath   string

	// The inputs the layer stack is rebuilt from when a setting changes. Kept
	// because a reload has to re-read the file — it may have been edited since
	// — and the environment and flags have not changed.
	environ []string
	flags   map[string]any

	// logLevel is the logger's threshold, held so that changing it at runtime
	// takes effect. The alternative is a restart to turn on debug output, which
	// is exactly the moment a restart is least welcome.
	logLevel *slog.LevelVar

	Settings *settings.Service

	Projects  *project.Service
	Uploads   *project.Uploads
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
	//
	// Replaceable rather than a bare sync.Once, because a Once latches its
	// failure for the life of the process — and the failure this one reports is
	// "there is no Python environment", which the user can then fix from the
	// interface. Without a way to forget, the message would survive the thing
	// it was about. See InvalidateWorker.
	workerMu   sync.Mutex
	workerSlot *workerSlot
	aiDirSlot  *aiDirSlot
	// retired holds pools replaced by InvalidateWorker. Kept rather than shut
	// down at that moment because a job in flight may hold a worker from one,
	// and stopping it there would fail that job at an unpredictable point.
	retired []*worker.Pool

	provision *provision.Manager
}

// workerSlot is one attempt at resolving an interpreter and building a pool.
//
// The Once inside supplies the happens-before that a field-level Once used to:
// every caller that reads the slot blocks in Do until the first of them has
// filled in pool and err.
type workerSlot struct {
	once sync.Once
	pool *worker.Pool
	err  error
}

// aiDirSlot is one attempt at finding the worker package.
type aiDirSlot struct {
	once sync.Once
	dir  string
	err  error
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

	// A flag beats the environment, which beats the default path. The container
	// image sets NIKUCOOKER_CONFIG, so a mounted configuration has to be found
	// without a flag — but a user who typed --config meant it.
	configPath := opts.ConfigPath
	if configPath == "" {
		configPath = config.ConfigFileFromEnv(environ)
	}
	if configPath == "" {
		configPath = config.DefaultPath
	}

	// The database needs a data directory, and the directory comes from the
	// configuration, so the first resolution happens before the settings that
	// live in the database can be read. It is redone below, with them, and the
	// cost is one extra file read at startup.
	cfg, _, err := resolveConfig(ctx, configPath, environ, opts.Flags, nil)
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

	application := &App{
		db:           db,
		log:          opts.Log,
		codeRevision: opts.CodeRevision,
		dataDir:      cfg.Storage.DataDir,
		configPath:   configPath,
		environ:      environ,
		flags:        opts.Flags,
		logLevel:     opts.LogLevel,
		Settings:     settings.NewService(db),
		Projects:     project.NewService(db, cfg.Storage.DataDir),
		Uploads:      project.NewUploads(cfg.Storage.DataDir),
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
		provision:    provision.NewManager(opts.Log),
	}

	// Swept once per process, here rather than on a timer: an abandoned upload
	// is only created by a user walking away, and the moment to notice is when
	// someone comes back. A ticker would be a goroutine and a lifetime to
	// manage for work that happens at most once per few hours.
	// Read again now that the database exists, so that anything the interface
	// has stored is in effect from the first request rather than only after the
	// first settings change.
	if err := application.reloadConfig(ctx); err != nil {
		// A stored setting the configuration rejects would otherwise be fatal,
		// and permanently so: the server would never start again and the
		// interface that would clear the value needs the server. Started
		// without the stored settings instead, so that the settings page loads
		// and the value can be reset, with the problem named.
		opts.Log.Error("a stored setting is not valid and has been left out; "+
			"reset it in the settings page or remove the row from the settings table",
			"error", err)

		cfg, provenance, fallbackErr := resolveConfig(ctx, configPath, environ, opts.Flags, nil)
		if fallbackErr != nil {
			_ = db.Close()
			return nil, fallbackErr
		}
		application.publish(cfg, provenance)
	}

	application.sweepUploads()

	return application, nil
}

// resolveConfig builds the configuration from every layer, lowest first.
//
// One function for startup and for every reload, because a reload that
// assembled the layers in a different order — or forgot one — would produce a
// running configuration that disagrees with the one a restart produces, and
// the difference would only show up as a setting that works until the next
// restart and then stops.
//
// overrides is the database layer, and is nil before the database is open.
func resolveConfig(
	ctx context.Context,
	configPath string,
	environ []string,
	flags map[string]any,
	overrides map[string]any,
) (*config.Config, config.Provenance, error) {
	fileLayer, err := config.FileLayer(configPath)
	if err != nil {
		return nil, nil, err
	}
	envLayer, err := config.EnvLayer(environ)
	if err != nil {
		return nil, nil, err
	}

	layers := []config.Layer{
		fileLayer,
		envLayer,
		// Above the environment, below the project overlay: a person changing
		// a setting in the browser is being more specific than a shell that
		// exported it once, and less specific than an override attached to the
		// project they are working on.
		{Source: config.SourceDatabase, Data: overrides},
	}
	if len(flags) > 0 {
		layers = append(layers, config.Layer{Source: config.SourceCLI, Data: flags})
	}

	cfg, provenance, err := config.Load(layers...)
	if err != nil {
		return nil, nil, err
	}

	// Before anything derives a path from the configuration. A relative
	// data_dir would otherwise be resolved differently by the Python worker,
	// whose working directory is its own, and every artifact path handed across
	// that boundary would point at nothing.
	if err := cfg.ResolvePaths(); err != nil {
		return nil, nil, err
	}

	_ = ctx
	return cfg, provenance, nil
}

// UpdateSettings stores settings and republishes the configuration.
//
// One method rather than two because the two are one operation: a stored value
// that the running configuration has not picked up is a setting the user has
// changed and cannot see the effect of, which is the state this whole feature
// exists to remove.
func (a *App) UpdateSettings(ctx context.Context, values map[string]any) error {
	if a.Settings == nil {
		return errors.New("app: settings are not available")
	}

	stored, err := a.Settings.All(ctx)
	if err != nil {
		return err
	}

	// Resolved before anything is written. A value the configuration rejects —
	// an empty render.modes, say — must not reach the database, because the
	// database is read at startup: a rejected value stored there is a server
	// that will not restart, and the interface that would fix it needs the
	// server running.
	cfg, provenance, err := resolveConfig(ctx, a.configPath, a.environ, a.flags,
		settings.Merge(stored, values))
	if err != nil {
		return err
	}

	if err := a.Settings.Set(ctx, values); err != nil {
		return err
	}

	a.publish(cfg, provenance)
	return nil
}

// ClearSetting removes one override and republishes the configuration.
func (a *App) ClearSetting(ctx context.Context, key string) error {
	if a.Settings == nil {
		return errors.New("app: settings are not available")
	}

	stored, err := a.Settings.All(ctx)
	if err != nil {
		return err
	}

	cfg, provenance, err := resolveConfig(ctx, a.configPath, a.environ, a.flags,
		settings.Without(stored, key))
	if err != nil {
		return err
	}

	if err := a.Settings.Delete(ctx, key); err != nil {
		return err
	}

	a.publish(cfg, provenance)
	return nil
}

// reloadConfig re-resolves the configuration and publishes it.
//
// Readings in flight keep the snapshot they already loaded, and a run that has
// started keeps the clone it took, so this changes what happens next rather
// than what is happening. That is the whole reason editing a setting does not
// need a restart.
func (a *App) reloadConfig(ctx context.Context) error {
	var overrides map[string]any
	if a.Settings != nil {
		stored, err := a.Settings.All(ctx)
		if err != nil {
			return err
		}
		overrides = stored
	}

	cfg, provenance, err := resolveConfig(ctx, a.configPath, a.environ, a.flags, overrides)
	if err != nil {
		return err
	}

	a.publish(cfg, provenance)
	return nil
}

// publish makes a resolved configuration the one in effect.
func (a *App) publish(cfg *config.Config, provenance config.Provenance) {
	a.config.Store(&resolved{cfg: cfg, prov: provenance})

	// The logger was built before the configuration existed, so its threshold
	// is applied here instead of being baked into the handler at construction.
	if a.logLevel != nil {
		a.logLevel.Set(slogLevel(cfg.Log.Level))
	}
}

// slogLevel maps a configured level name onto slog's.
//
// An unrecognized name falls back to info rather than erroring: the level is a
// diagnostic convenience, and refusing to start over a misspelled one would be
// out of proportion. Validate reports it separately, which is where a typo is
// caught.
func slogLevel(name string) slog.Level {
	switch name {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// uploadRetention is how long an upload waits to become a project.
//
// Long, because the cost of being wrong is asymmetric: sweeping something a
// user is still filling a form in for loses a multi-gigabyte transfer, while
// keeping it a day longer costs disk that was going to be freed anyway.
const uploadRetention = 24 * time.Hour

func (a *App) sweepUploads() {
	if a.Uploads == nil {
		return
	}

	removed, err := a.Uploads.Sweep(uploadRetention)
	if err != nil {
		// Never fatal: an unwritable staging directory is a problem for
		// uploading, which will report it, not for listing projects.
		a.log.Debug("sweeping stale uploads", "error", err)
		return
	}
	if removed > 0 {
		a.log.Info("discarded stale uploads", "count", removed)
	}
}

// Close releases the resources an App holds.
func (a *App) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), a.Config().Worker.ShutdownTimeout)
	defer cancel()

	a.workerMu.Lock()
	pool := (*worker.Pool)(nil)
	if a.workerSlot != nil {
		pool = a.workerSlot.pool
	}
	retired := a.retired
	a.retired = nil
	a.workerMu.Unlock()

	if pool != nil {
		_ = pool.Shutdown(ctx)
	}
	// Pools replaced by provisioning, shut down now that nothing can be holding
	// one. They were kept alive at the moment of replacement for exactly this:
	// a job in flight then is finished by now, or it is not coming back.
	for _, old := range retired {
		if old != pool {
			_ = old.Shutdown(ctx)
		}
	}

	if a.db != nil {
		return a.db.Close()
	}
	return nil
}

// Config returns the resolved configuration.
func (a *App) Config() *config.Config { return a.config.Load().cfg }

// Provenance returns which layer set each configuration key.
func (a *App) Provenance() config.Provenance { return a.config.Load().prov }

// DB exposes the database, for callers that need a query the services do not
// offer.
func (a *App) DB() *database.DB { return a.db }

// DataDir reports the data root.
func (a *App) DataDir() string { return a.dataDir }

// ConfigPath reports the configuration file this process reads.
//
// Reported even when the file does not exist, because "there is no file and
// here is where one would go" is the answer a user needs — an empty string
// would leave them with nothing to act on.
func (a *App) ConfigPath() string { return a.configPath }

// ConfigFileExists reports whether that file is actually there.
func (a *App) ConfigFileExists() bool {
	_, err := os.Stat(a.configPath)
	return err == nil
}

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
		Explicit:      a.Config().AI.Python,
		AIDir:         dir,
		RuntimeDir:    a.RuntimeDir(),
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
	slot := a.currentWorkerSlot()
	slot.once.Do(func() { slot.pool, slot.err = a.buildWorker(ctx) })

	if slot.err != nil {
		return nil, slot.err
	}
	return slot.pool, nil
}

// currentWorkerSlot returns the slot in use, creating it if this is the first
// caller.
func (a *App) currentWorkerSlot() *workerSlot {
	a.workerMu.Lock()
	defer a.workerMu.Unlock()

	if a.workerSlot == nil {
		a.workerSlot = &workerSlot{}
	}
	return a.workerSlot
}

// buildWorker resolves an interpreter and builds the pool.
//
// Extracted from the closure it used to be, so that it returns its error rather
// than latching it: a resolution order that can only be exercised by starting a
// server is a resolution order with no tests.
func (a *App) buildWorker(ctx context.Context) (*worker.Pool, error) {
	dir, err := a.AIDir()
	if err != nil {
		return nil, err
	}

	// The digest is what the worker checks its own schema against during the
	// handshake. Both sides computing it from the same fixtures is what makes a
	// protocol change a startup failure rather than a misinterpreted message
	// halfway through a job.
	digest, err := protocol.SchemaDigest()
	if err != nil {
		return nil, fmt.Errorf("app: compute the protocol schema digest: %w", err)
	}

	// Resolved against the package directory, which is what makes the virtual
	// environment beside it discoverable.
	python, err := platform.ResolvePython(ctx, platform.ResolveOptions{
		Explicit:      a.Config().AI.Python,
		AIDir:         dir,
		RuntimeDir:    a.RuntimeDir(),
		RequireImport: true,
		Hint:          a.provisionHint(),
	})
	if err != nil {
		return nil, err
	}

	// Checked here rather than left to the import. A Python that is the wrong
	// version fails deep inside a dependency with a message about a syntax
	// error or a missing symbol, which reads as a broken install rather than as
	// one wrong setting.
	version, err := platform.Version(ctx, python)
	if err != nil {
		return nil, err
	}
	constraint, err := platform.ParseConstraint(a.Config().AI.PythonVersion)
	if err != nil {
		return nil, err
	}
	if !constraint.IsEmpty() && !constraint.Allows(version) {
		return nil, fmt.Errorf("app: the AI worker needs Python %s but %s is %s",
			a.Config().AI.PythonVersion, python, version)
	}

	args := a.Config().AI.Args
	if len(args) == 0 {
		args = []string{"-m", "nikucooker_ai"}
	}

	return worker.NewPool(worker.Config{
		Python:           python,
		Args:             args,
		Dir:              dir,
		SchemaDigest:     digest,
		CodeRevision:     a.codeRevision,
		StartupTimeout:   a.Config().Worker.StartupTimeout,
		ShutdownTimeout:  a.Config().Worker.ShutdownTimeout,
		ModelLoadTimeout: a.Config().Worker.ModelLoadTimeout,
		StallTimeout:     a.Config().Worker.StallTimeout,
		Log:              a.log,
	}, a.Config().Worker.PoolSize), nil
}

// InvalidateWorker forgets the resolved worker and AI directory.
//
// Called after provisioning succeeds. Both are forgotten because both can have
// been resolved while the environment was missing: the AI directory fails when
// a user extracts the archive into a folder the binary had already searched,
// and the interpreter fails before anything is installed.
//
// Concurrency is the same as it was with a bare sync.Once for every ordinary
// caller: they all observe the same slot and block in its Do. The one new
// window is a caller that read the slot just before this replaced it — a stale
// success hands back a pool that still works, and a stale failure is corrected
// on the next call. Closing that window would mean holding the lock across
// buildWorker, which spawns Python; that is the worse trade.
func (a *App) InvalidateWorker() {
	a.workerMu.Lock()
	defer a.workerMu.Unlock()

	if a.workerSlot != nil && a.workerSlot.pool != nil {
		a.retired = append(a.retired, a.workerSlot.pool)
	}
	a.workerSlot = nil
	a.aiDirSlot = nil
}

// AIDir returns the directory holding the nikucooker_ai package.
//
// Memoised, and shared with the worker pool: the doctor command reports the
// path the worker will actually be launched from, and a diagnosis that resolved
// it differently from the thing it was diagnosing would be worse than none.
func (a *App) AIDir() (string, error) {
	a.workerMu.Lock()
	if a.aiDirSlot == nil {
		a.aiDirSlot = &aiDirSlot{}
	}
	slot := a.aiDirSlot
	a.workerMu.Unlock()

	slot.once.Do(func() { slot.dir, slot.err = resolveAIDir(a.Config()) })
	return slot.dir, slot.err
}

// RuntimeDir is where a provisioned environment lives, or "" when there is no
// data directory to put one in.
//
// The empty case is not a degraded mode: a container has ai.python set and a
// checkout has ai/.venv, and neither needs a second place to look.
//
// Exported because the doctor reports on it, and a diagnosis that computed the
// path differently from the thing it was diagnosing would be worse than none.
func (a *App) RuntimeDir() string {
	if a.Config().Storage.DataDir == "" {
		return ""
	}
	return platform.RuntimeDir(a.Config().Storage.DataDir)
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
		// The working directory itself, for a layout where ai/ is not a
		// subdirectory of it but *is* it — which is what the container has, and
		// what running this from inside a checkout's ai/ directory has.
		//
		// Checked last so the checkout keeps resolving the way it always has:
		// from the repository root, ai/ is the subdirectory, and a bare cwd
		// there holds no package to find.
		if dir, ok := check(cwd); ok {
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
	JobID     string
	ProjectID string
	Plan      *pipeline.Plan
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

	return &RunResult{JobID: prepared.JobID, ProjectID: prepared.ProjectID, Plan: prepared.plan}, nil
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
	cfg := a.Config().Clone()
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
	// Checked before the join, because joining an empty path yields the project
	// directory — which exists, so the stat below would pass and the failure
	// would surface much later as "is a directory" from whatever tried to read
	// the media.
	if prj.SourcePath == "" {
		return "", fmt.Errorf(
			"app: project %s has no source media; its video was deleted, so there is "+
				"nothing to run over. Create a new project from the video to run this again", prj.ID)
	}

	path := filepath.Join(project.Dir(a.dataDir, prj.ID), filepath.FromSlash(prj.SourcePath))
	if _, err := os.Stat(path); err != nil {
		return "", fmt.Errorf(
			"app: the source media of project %s is missing at %s: %w", prj.ID, path, err)
	}
	return path, nil
}
