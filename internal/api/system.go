package api

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/AhsokaTano26/NikuCooker/internal/app"
	"github.com/AhsokaTano26/NikuCooker/internal/events"
	"github.com/AhsokaTano26/NikuCooker/internal/platform"
	"github.com/AhsokaTano26/NikuCooker/internal/project"
	"github.com/AhsokaTano26/NikuCooker/pkg/protocol"
)

// ---------------------------------------------------------------------------
// System
// ---------------------------------------------------------------------------

func (s *Server) getSystem(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	overview := systemOverview{
		Version:  s.version,
		Commit:   s.commit,
		Platform: runtime.GOOS + "-" + runtime.GOARCH,
		UptimeS:  time.Since(startedAt).Seconds(),

		Features: systemFeatures{
			PathSource:       s.app.Config().Server.AllowPathSource,
			MaxUploadBytes:   s.app.Config().Server.MaxUploadBytes,
			ConfigPath:       s.app.ConfigPath(),
			ConfigFileExists: s.app.ConfigFileExists(),
		},
	}

	s.fillCounts(ctx, &overview.Counts)
	s.fillWorker(ctx, &overview.Worker)
	s.fillStats(ctx, &overview.Stats)

	s.respond(w, http.StatusOK, overview)
}

func (s *Server) getHealth(w http.ResponseWriter, r *http.Request) {
	// Deliberately shallow. A health check that touches the database turns a
	// moment of lock contention into a failed liveness probe, which restarts a
	// process that was working fine.
	s.respond(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) fillCounts(ctx context.Context, counts *systemCounts) {
	if _, total, err := s.app.Projects.List(ctx, project.ListQuery{Limit: 1}); err == nil {
		counts.Projects = total
	}

	projects, _, err := s.app.Projects.List(ctx, project.ListQuery{Limit: 500})
	if err != nil {
		return
	}

	for _, p := range projects {
		segments, needsReview, err := s.app.Segments.Counts(ctx, p.ID)
		if err == nil {
			counts.Segments += segments
			counts.NeedsReview += needsReview
		}

		// Only the newest job matters for the running and pending counts: a
		// project can only have one of either at a time, and older rows are
		// history.
		recent, _, err := s.app.Jobs.ListByProject(ctx, p.ID, 1, 0)
		if err != nil || len(recent) == 0 {
			continue
		}
		switch recent[0].Status {
		case "running":
			counts.JobsRunning++
		case "pending":
			counts.JobsPending++
		}
	}
}

func (s *Server) fillWorker(ctx context.Context, view *workerView) {
	// The status is reported without starting a worker. A dashboard that
	// spawned a Python process to answer a status query would be making the
	// thing it was reporting on.
	view.Status = "stopped"
	view.Workers = 0
	view.LoadedModels = []loadedModelView{}

	if python, err := s.app.ResolvePython(ctx); err == nil {
		view.Python = python
	}
	if digest, err := protocol.SchemaDigest(); err == nil {
		view.SchemaDigest = digest
	}
}

func (s *Server) fillStats(ctx context.Context, stats *systemStats) {
	host := platform.HostStats(ctx, s.app.DataDir(), s.app.Config().Storage.ModelDir)

	stats.CPU = cpuStats{Cores: host.CPU.Cores, UsagePercent: host.CPU.UsagePercent}
	stats.Memory = memoryStats{TotalBytes: host.Memory.TotalBytes, UsedBytes: host.Memory.UsedBytes}
	stats.Disk = diskStats{
		DataFreeBytes:   host.Disk.DataFreeBytes,
		ModelsFreeBytes: host.Disk.ModelsFreeBytes,
	}

	stats.GPU = make([]gpuStats, 0, len(host.GPU))
	for _, gpu := range host.GPU {
		stats.GPU = append(stats.GPU, gpuStats{
			Index:              gpu.Index,
			Name:               gpu.Name,
			MemoryTotalBytes:   gpu.MemoryTotalBytes,
			MemoryUsedBytes:    gpu.MemoryUsedBytes,
			UtilizationPercent: gpu.UtilizationPercent,
			Driver:             gpu.Driver,
			CUDA:               gpu.CUDA,
		})
	}
}

// ---------------------------------------------------------------------------
// Events
// ---------------------------------------------------------------------------

// getEvents streams live updates.
//
// The one long-lived request in the API. Everything else is short and stateless;
// this holds a connection open until the client goes away, which is why it owns
// its own subscription and touches nothing else.
func (s *Server) getEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		s.fail(w, internal(errNotFlushable))
		return
	}

	since, err := queryInt(r, "since", 0)
	if err != nil {
		s.fail(w, err)
		return
	}

	// Browsers send the last id automatically on reconnect, which is the whole
	// resume mechanism. An explicit `since` wins, but both are honoured so that
	// a non-browser client can use either.
	if header := r.Header.Get("Last-Event-ID"); header != "" && since == 0 {
		if parsed, convErr := strconv.ParseInt(strings.TrimSpace(header), 10, 64); convErr == nil {
			since = int(parsed)
		}
	}

	projectFilter := map[string]bool{}
	for _, id := range r.URL.Query()["project_id"] {
		projectFilter[id] = true
	}

	topicFilter := map[events.Type]bool{}
	for _, topic := range r.URL.Query()["topics"] {
		topicFilter[events.Type(topic)] = true
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// Stops a reverse proxy from buffering the stream into uselessness, which
	// presents as "the UI never updates" and wastes an afternoon.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	subscription, replayable := s.app.Events.Subscribe(int64(since))
	defer subscription.Close()

	// The two events below are generated by this handler rather than published
	// on the bus, so they carry no sequence of their own. They are stamped with
	// the counter's *current* value instead, which is the honest meaning: "the
	// stream is caught up to here". Leaving them at zero would tell a client its
	// position was the beginning of time, and its next reconnect would ask for a
	// replay of everything the process has ever emitted.
	current := s.app.Events.CurrentSeq()

	s.writeEvent(w, handshakeEvent(events.TypeHello, current).
		With("server_version", s.version).
		With("current_seq", current).
		With("replay", replayable).
		With("replayed_from", since))

	if !replayable {
		// Said plainly rather than papered over. The client discards its state
		// and refetches over REST; a stream that silently started from the
		// oldest retained event would leave it with a gap it cannot see.
		//
		// The stream stays open afterwards: resyncing is a REST call, and live
		// updates should keep arriving while it happens.
		s.writeEvent(w, handshakeEvent(events.TypeResyncRequired, current).
			With("reason", "the requested position is no longer available").
			With("oldest_available", s.app.Events.OldestSeq()))
	}

	flusher.Flush()

	heartbeat := time.NewTicker(20 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return

		case event, ok := <-subscription.Events():
			if !ok {
				// The subscription closed because the client could not keep up.
				// Saying so is the difference between a client that recovers and
				// one that quietly shows stale state forever.
				if subscription.Overflowed() {
					s.writeEvent(w, handshakeEvent(events.TypeResyncRequired,
						s.app.Events.CurrentSeq()).
						With("reason", "the client fell behind"))
					flusher.Flush()
				}
				return
			}

			if !matches(event, projectFilter, topicFilter) {
				continue
			}
			s.writeEvent(w, event)
			flusher.Flush()

		case <-heartbeat.C:
			// A comment line keeps the connection alive through proxies that
			// close an idle stream, and costs nothing.
			_, _ = w.Write([]byte(": heartbeat\n\n"))
			flusher.Flush()
		}
	}
}

// handshakeEvent builds an event the stream handler generates itself.
//
// It is not published on the bus — nothing subscribed to it — so it has no
// sequence number to take. It is stamped with the counter's current value
// instead, which is what a client reads as "you are caught up to here".
func handshakeEvent(eventType events.Type, current int64) events.Event {
	event := events.New(eventType)
	event.Seq = current
	event.TS = time.Now().UTC()
	return event
}

// matches applies the stream's filters.
func matches(event events.Event, projects map[string]bool, topics map[events.Type]bool) bool {
	if len(projects) > 0 && event.ProjectID != "" && !projects[event.ProjectID] {
		return false
	}
	if len(topics) > 0 && !topics[event.Type] {
		return false
	}
	return true
}

// writeEvent renders one SSE frame.
func (s *Server) writeEvent(w http.ResponseWriter, event events.Event) {
	encoded, err := json.Marshal(event)
	if err != nil {
		s.log.Warn("could not encode an event", "type", event.Type, "error", err)
		return
	}

	// The payload must be one line. A JSON encoder escapes a newline inside a
	// string, so a multi-line log message cannot break the framing — but the
	// escaping is asserted here rather than assumed, because the day it stops
	// being true the stream breaks in a way that looks like a client bug.
	payload := strings.NewReplacer("\r", "", "\n", "\\n").Replace(string(encoded))

	// The event name is mirrored into the frame as well as the envelope, so a
	// browser can dispatch on either. Redundancy is deliberate: EventSource
	// consumers use addEventListener, and a non-browser client reading a dump
	// still gets the type from the envelope.
	_, _ = w.Write([]byte("id: " + strconv.FormatInt(event.Seq, 10) + "\n"))
	_, _ = w.Write([]byte("event: " + string(event.Type) + "\n"))
	_, _ = w.Write([]byte("data: " + payload + "\n\n"))
}

// ---------------------------------------------------------------------------
// Small helpers
// ---------------------------------------------------------------------------

var errNotFlushable = errString("the connection cannot be streamed on")

type errString string

func (e errString) Error() string { return string(e) }

// eventsForProject builds a project-scoped event.
func eventsForProject(projectID string, eventType events.Type) events.Event {
	return events.New(eventType).ForProject(projectID)
}

// emitProject announces that a project record changed.
func (s *Server) emitProject(projectID string) {
	s.app.Events.Emit(eventsForProject(projectID, events.TypeProjectUpdated))
}

// appRunOptions translates the API's run body into the application's options.
func appRunOptions(projectID string, body runBody) app.RunOptions {
	options := app.RunOptions{ProjectID: projectID, Force: body.Force}

	if body.FromStage != nil {
		options.FromStage = *body.FromStage
	}
	if body.ToStage != nil {
		options.ToStage = *body.ToStage
	}
	if len(body.Stages) > 0 {
		options.Only = body.Stages
	}
	return options
}

// baseName returns a path's final element.
func baseName(path string) string { return filepath.Base(path) }
