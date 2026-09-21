// Package jobs owns execution: which job is running, what state it is in, and
// what happens to it when the process dies.
//
// A job is one execution attempt against a pipeline. Retrying a failed stage
// creates a *new* job rather than rewinding a terminal stage in place, which is
// what keeps job history auditable and the state machine honest.
package jobs

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/AhsokaTano26/NikuCooker/internal/database"
)

// Status is a job's state.
type Status string

const (
	StatusPending   Status = "pending"
	StatusRunning   Status = "running"
	StatusPaused    Status = "paused"
	StatusCompleted Status = "completed"
	StatusFailed    Status = "failed"
	StatusCancelled Status = "cancelled"
)

// Terminal reports whether no further transition is possible.
func (s Status) Terminal() bool {
	switch s {
	case StatusCompleted, StatusFailed, StatusCancelled:
		return true
	default:
		return false
	}
}

// ErrIllegalTransition reports a state change the machine forbids.
var ErrIllegalTransition = errors.New("jobs: illegal status transition")

// ErrAlreadyRunning reports a second job for a project that already has one.
var ErrAlreadyRunning = errors.New("jobs: project already has a running job")

// ErrNotFound reports a job that does not exist.
var ErrNotFound = errors.New("jobs: not found")

// transitions is the job state machine.
//
// Terminal states are terminal. Retry and force re-run are expressed as new
// rows rather than illegal in-place rewinds, so a job's history always
// describes what actually happened.
var transitions = map[Status][]Status{
	StatusPending:   {StatusRunning, StatusCancelled, StatusFailed},
	StatusRunning:   {StatusPaused, StatusCompleted, StatusFailed, StatusCancelled, StatusPending},
	StatusPaused:    {StatusRunning, StatusCancelled, StatusFailed},
	StatusCompleted: {},
	StatusFailed:    {},
	StatusCancelled: {},
}

// CanTransition reports whether a status change is legal.
func CanTransition(from, to Status) bool {
	for _, allowed := range transitions[from] {
		if allowed == to {
			return true
		}
	}
	return false
}

// CheckTransition returns ErrIllegalTransition with both states named.
func CheckTransition(from, to Status) error {
	if from == to {
		return nil
	}
	if !CanTransition(from, to) {
		return fmt.Errorf("%w: %s → %s", ErrIllegalTransition, from, to)
	}
	return nil
}

// Kind is what a job covers.
type Kind string

const (
	// KindFull runs the whole pipeline.
	KindFull Kind = "full"
	// KindStage runs one stage and whatever of its dependencies is missing.
	KindStage Kind = "stage"
)

// Job is one execution attempt.
type Job struct {
	ID          string `json:"id"`
	ProjectID   string `json:"project_id"`
	Kind        Kind   `json:"kind"`
	TargetStage string `json:"target_stage,omitempty"`

	// Force ignores the artifact cache.
	Force bool `json:"force"`

	Status   Status  `json:"status"`
	Progress float64 `json:"progress"`

	ErrorCode    string `json:"error_code,omitempty"`
	ErrorMessage string `json:"error_message,omitempty"`

	CreatedAt   time.Time  `json:"created_at"`
	StartedAt   *time.Time `json:"started_at,omitempty"`
	FinishedAt  *time.Time `json:"finished_at,omitempty"`
	HeartbeatAt *time.Time `json:"heartbeat_at,omitempty"`
}

// StageRun is one stage's state within a job.
type StageRun struct {
	JobID     string `json:"job_id"`
	ProjectID string `json:"project_id"`
	Name      string `json:"name"`
	Ordinal   int    `json:"ordinal"`

	Status   string  `json:"status"`
	Progress float64 `json:"progress"`
	Attempt  int     `json:"attempt"`

	ArtifactID string `json:"artifact_id,omitempty"`

	ErrorCode    string `json:"error_code,omitempty"`
	ErrorMessage string `json:"error_message,omitempty"`

	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

// Error codes a job can carry.
const (
	// CodeInterrupted marks a job that was running when the process died.
	//
	// It exists so a user sees "interrupted" rather than a job stuck at
	// running forever, which is the single most confusing state a job
	// scheduler can be in.
	CodeInterrupted = "INTERRUPTED"
	// CodeCancelled marks a job stopped at the user's request.
	CodeCancelled = "CANCELLED"
	// CodeStageFailed marks a job whose stage errored.
	CodeStageFailed = "STAGE_FAILED"
)

// ---------------------------------------------------------------------------
// Repository
// ---------------------------------------------------------------------------

// Repository persists jobs.
type Repository struct {
	db *database.DB
}

// NewRepository binds a repository.
func NewRepository(db *database.DB) *Repository { return &Repository{db: db} }

// Create records a new job.
func (r *Repository) Create(ctx context.Context, job *Job) error {
	if job.ID == "" {
		job.ID = uuid.Must(uuid.NewV7()).String()
	}
	if job.CreatedAt.IsZero() {
		job.CreatedAt = time.Now().UTC()
	}
	if job.Status == "" {
		job.Status = StatusPending
	}

	_, err := r.db.Write.ExecContext(ctx, `
		INSERT INTO jobs (id, project_id, kind, target_stage, force, status, progress,
		                  error_code, error_message, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		job.ID, job.ProjectID, string(job.Kind), nullable(job.TargetStage), boolToInt(job.Force),
		string(job.Status), job.Progress, nullable(job.ErrorCode), nullable(job.ErrorMessage),
		formatTime(job.CreatedAt))
	if err != nil {
		return fmt.Errorf("jobs: create: %w", err)
	}
	return nil
}

// Get loads a job.
func (r *Repository) Get(ctx context.Context, id string) (*Job, error) {
	const query = `
		SELECT id, project_id, kind, COALESCE(target_stage, ''), force, status, progress,
		       COALESCE(error_code, ''), COALESCE(error_message, ''),
		       created_at, started_at, finished_at, heartbeat_at
		FROM jobs WHERE id = ?`

	var (
		job                                Job
		kind, status                       string
		force                              int
		createdAt                          string
		startedAt, finishedAt, heartbeatAt sql.NullString
	)
	err := r.db.Read.QueryRowContext(ctx, query, id).Scan(
		&job.ID, &job.ProjectID, &kind, &job.TargetStage, &force, &status, &job.Progress,
		&job.ErrorCode, &job.ErrorMessage,
		&createdAt, &startedAt, &finishedAt, &heartbeatAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if err != nil {
		return nil, fmt.Errorf("jobs: get %s: %w", id, err)
	}

	job.Kind = Kind(kind)
	job.Status = Status(status)
	job.Force = force != 0
	job.CreatedAt = parseTime(createdAt)
	job.StartedAt = parseNullTime(startedAt)
	job.FinishedAt = parseNullTime(finishedAt)
	job.HeartbeatAt = parseNullTime(heartbeatAt)
	return &job, nil
}

// SetStatus transitions a job, rejecting an illegal move.
//
// The current status is read inside the same transaction that writes the new
// one, so a concurrent transition cannot slip between the check and the write.
func (r *Repository) SetStatus(ctx context.Context, id string, to Status, code, message string) error {
	tx, err := r.db.Write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("jobs: begin transition: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var current string
	if err := tx.QueryRowContext(ctx, `SELECT status FROM jobs WHERE id = ?`, id).Scan(&current); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: %s", ErrNotFound, id)
		}
		return fmt.Errorf("jobs: read status of %s: %w", id, err)
	}

	if err := CheckTransition(Status(current), to); err != nil {
		return err
	}

	now := time.Now().UTC()
	assignments := []string{"status = ?"}
	args := []any{string(to)}

	switch to {
	case StatusRunning:
		assignments = append(assignments, "started_at = COALESCE(started_at, ?)", "heartbeat_at = ?")
		args = append(args, formatTime(now), formatTime(now))
	case StatusCompleted, StatusFailed, StatusCancelled:
		assignments = append(assignments, "finished_at = ?")
		args = append(args, formatTime(now))
	}

	assignments = append(assignments, "error_code = ?", "error_message = ?")
	args = append(args, nullable(code), nullable(message))
	args = append(args, id)

	query := "UPDATE jobs SET " + joinComma(assignments) + " WHERE id = ?"
	if _, err := tx.ExecContext(ctx, query, args...); err != nil {
		return fmt.Errorf("jobs: update status of %s: %w", id, err)
	}
	return tx.Commit()
}

// SetProgress records progress.
//
// Progress is written at a throttled cadence rather than per stage update, so a
// restart loses at most a few seconds of reporting. It never moves backwards
// within a job, because a bar that retreats reads as a bug.
func (r *Repository) SetProgress(ctx context.Context, id string, progress float64) error {
	if progress < 0 {
		progress = 0
	}
	if progress > 1 {
		progress = 1
	}

	_, err := r.db.Write.ExecContext(ctx,
		`UPDATE jobs SET progress = MAX(progress, ?) WHERE id = ?`, progress, id)
	if err != nil {
		return fmt.Errorf("jobs: set progress of %s: %w", id, err)
	}
	return nil
}

// Heartbeat records that a running job is still alive.
func (r *Repository) Heartbeat(ctx context.Context, id string) error {
	_, err := r.db.Write.ExecContext(ctx,
		`UPDATE jobs SET heartbeat_at = ? WHERE id = ?`, formatTime(time.Now().UTC()), id)
	if err != nil {
		return fmt.Errorf("jobs: heartbeat %s: %w", id, err)
	}
	return nil
}

// StageRuns lists a job's stage states in pipeline order.
func (r *Repository) StageRuns(ctx context.Context, jobID string) ([]StageRun, error) {
	const query = `
		SELECT job_id, project_id, name, ordinal, status, progress, attempt,
		       COALESCE(artifact_id, ''), COALESCE(error_code, ''), COALESCE(error_message, ''),
		       started_at, finished_at
		FROM stages WHERE job_id = ? ORDER BY ordinal`

	rows, err := r.db.Read.QueryContext(ctx, query, jobID)
	if err != nil {
		return nil, fmt.Errorf("jobs: list stage runs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []StageRun
	for rows.Next() {
		var (
			run                   StageRun
			startedAt, finishedAt sql.NullString
		)
		if err := rows.Scan(&run.JobID, &run.ProjectID, &run.Name, &run.Ordinal, &run.Status,
			&run.Progress, &run.Attempt, &run.ArtifactID, &run.ErrorCode, &run.ErrorMessage,
			&startedAt, &finishedAt); err != nil {
			return nil, fmt.Errorf("jobs: scan stage run: %w", err)
		}
		run.StartedAt = parseNullTime(startedAt)
		run.FinishedAt = parseNullTime(finishedAt)
		out = append(out, run)
	}
	return out, rows.Err()
}

// UpsertStageRun records a stage's state within a job.
func (r *Repository) UpsertStageRun(ctx context.Context, run StageRun) error {
	_, err := r.db.Write.ExecContext(ctx, `
		INSERT INTO stages (job_id, project_id, name, ordinal, status, progress, attempt,
		                    artifact_id, error_code, error_message, started_at, finished_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (job_id, name) DO UPDATE SET
			status = excluded.status,
			progress = excluded.progress,
			attempt = excluded.attempt,
			artifact_id = excluded.artifact_id,
			error_code = excluded.error_code,
			error_message = excluded.error_message,
			started_at = COALESCE(stages.started_at, excluded.started_at),
			finished_at = excluded.finished_at`,
		run.JobID, run.ProjectID, run.Name, run.Ordinal, run.Status, run.Progress, run.Attempt,
		nullable(run.ArtifactID), nullable(run.ErrorCode), nullable(run.ErrorMessage),
		nullableTime(run.StartedAt), nullableTime(run.FinishedAt))
	if err != nil {
		return fmt.Errorf("jobs: upsert stage run %s/%s: %w", run.JobID, run.Name, err)
	}
	return nil
}

// ListByProject returns a project's jobs, newest first.
func (r *Repository) ListByProject(ctx context.Context, projectID string, limit, offset int) ([]*Job, int, error) {
	if limit <= 0 {
		limit = 50
	}

	var total int
	if err := r.db.Read.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM jobs WHERE project_id = ?`, projectID).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("jobs: count: %w", err)
	}

	rows, err := r.db.Read.QueryContext(ctx, `
		SELECT id, project_id, kind, COALESCE(target_stage, ''), force, status, progress,
		       COALESCE(error_code, ''), COALESCE(error_message, ''),
		       created_at, started_at, finished_at, heartbeat_at
		FROM jobs WHERE project_id = ?
		ORDER BY created_at DESC, id DESC
		LIMIT ? OFFSET ?`, projectID, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("jobs: list: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []*Job
	for rows.Next() {
		var (
			job                                Job
			kind, status                       string
			force                              int
			createdAt                          string
			startedAt, finishedAt, heartbeatAt sql.NullString
		)
		if err := rows.Scan(&job.ID, &job.ProjectID, &kind, &job.TargetStage, &force, &status,
			&job.Progress, &job.ErrorCode, &job.ErrorMessage, &createdAt,
			&startedAt, &finishedAt, &heartbeatAt); err != nil {
			return nil, 0, fmt.Errorf("jobs: scan: %w", err)
		}
		job.Kind = Kind(kind)
		job.Status = Status(status)
		job.Force = force != 0
		job.CreatedAt = parseTime(createdAt)
		job.StartedAt = parseNullTime(startedAt)
		job.FinishedAt = parseNullTime(finishedAt)
		job.HeartbeatAt = parseNullTime(heartbeatAt)
		out = append(out, &job)
	}
	return out, total, rows.Err()
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullableTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return formatTime(*t)
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func formatTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func parseTime(raw string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}
	}
	return t
}

func parseNullTime(raw sql.NullString) *time.Time {
	if !raw.Valid || raw.String == "" {
		return nil
	}
	t := parseTime(raw.String)
	if t.IsZero() {
		return nil
	}
	return &t
}

func joinComma(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += ", "
		}
		out += p
	}
	return out
}
