package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/AhsokaTano26/NikuCooker/internal/events"
	"github.com/AhsokaTano26/NikuCooker/internal/logging"
	"github.com/AhsokaTano26/NikuCooker/internal/project"
)

// maintenanceInterval is how often the sweeps run.
//
// Hourly, because nothing they delete is urgent: the retention periods are
// measured in days, and a sweep that ran every minute would spend its life
// walking directories to find nothing.
const maintenanceInterval = time.Hour

// StartMaintenance runs the retention sweeps in the background.
//
// Returns a function that stops it. The caller is the server, not the
// application: a `nikucooker run` invoked to re-run one project must not
// decide, on its way past, that some other project has expired.
//
// This is the codebase's first timer. Everything else that deletes is swept
// once per process, on the reasoning that the work happens at most once per
// few hours and a goroutine needs a lifetime managed. That reasoning holds for
// uploads, which are abandoned by the minute. It does not hold here: the
// server runs for weeks, and a retention period measured in days is only
// enforced if something is awake to enforce it.
func (a *App) StartMaintenance(ctx context.Context) func() {
	stop := make(chan struct{})
	done := make(chan struct{})

	go func() {
		defer close(done)

		// Once at startup, so a restart enforces the retention that elapsed
		// while the process was not running.
		a.RunMaintenance(ctx)

		ticker := time.NewTicker(maintenanceInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				a.RunMaintenance(ctx)
			case <-stop:
				return
			case <-ctx.Done():
				return
			}
		}
	}()

	return func() {
		close(stop)
		<-done
	}
}

// RunMaintenance performs one pass of every sweep.
//
// Exported so that it can be called directly — by a test, and by a user with a
// terminal who wants to reclaim the space now rather than at the top of the
// hour.
func (a *App) RunMaintenance(ctx context.Context) {
	cfg := a.Config()

	a.pruneRunLogs(cfg.Retention.LogDays)
	a.expireProjects(ctx, cfg.Retention.ProjectDays)
}

// pruneRunLogs deletes run logs past their retention.
func (a *App) pruneRunLogs(days int) {
	if days <= 0 {
		return
	}

	projects, err := a.projectDirectories()
	if err != nil {
		a.log.Warn("could not list projects for log cleanup", "error", err)
		return
	}

	removed := 0
	for _, dir := range projects {
		count, err := logging.PruneLogs(filepath.Join(dir, project.DirLogs), time.Duration(days)*24*time.Hour)
		if err != nil {
			// One unreadable directory must not stop the rest: the log
			// directory of a project being deleted at this moment is expected.
			a.log.Debug("could not prune a project's logs", "dir", dir, "error", err)
			continue
		}
		removed += count
	}

	if removed > 0 {
		a.log.Info("deleted expired run logs", "count", removed, "older_than_days", days)
	}
}

// expireProjects deletes projects nobody has touched for the retention period.
//
// The most destructive thing this program does without being asked, so it is
// built to be legible afterwards: every deletion is logged with the name and
// the space reclaimed, and announced on the event stream so an open interface
// removes it from the list rather than leaving a project that cannot be opened.
func (a *App) expireProjects(ctx context.Context, days int) {
	if days <= 0 {
		return
	}

	idle, err := a.Projects.IdleSince(ctx, time.Duration(days)*24*time.Hour)
	if err != nil {
		a.log.Warn("could not list idle projects", "error", err)
		return
	}

	for _, candidate := range idle {
		// A project being worked on right now is not idle, whatever its
		// timestamps say: the timestamps are written when a run is created, and
		// a long run started yesterday would otherwise be deleted from under
		// itself.
		if _, running := a.Scheduler.Running(candidate.ID); running {
			a.log.Debug("skipping a project that is running", "project_id", candidate.ID)
			continue
		}

		bytes, _ := project.TotalBytes(a.dataDir, candidate.ID)

		if err := a.Projects.Delete(ctx, candidate.ID, true); err != nil {
			a.log.Error("could not delete an idle project",
				"project_id", candidate.ID, "name", candidate.Name, "error", err)
			continue
		}

		a.log.Info("deleted an idle project",
			"project_id", candidate.ID, "name", candidate.Name,
			"idle_days", days, "reclaimed_bytes", bytes)

		a.emit(events.New(events.TypeProjectDeleted).
			ForProject(candidate.ID).
			With("name", candidate.Name).
			With("reason", fmt.Sprintf("已有 %d 天没有使用", days)))
	}
}

// projectDirectories lists every project directory on disk.
//
// Read from the filesystem rather than from the database, because what is being
// cleaned is files: a directory whose row is already gone is exactly the kind
// of leftover this is meant to find.
func (a *App) projectDirectories() ([]string, error) {
	root := filepath.Join(a.dataDir, "projects")

	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	dirs := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			dirs = append(dirs, filepath.Join(root, entry.Name()))
		}
	}
	return dirs, nil
}
