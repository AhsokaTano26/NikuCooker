package api

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/AhsokaTano26/NikuCooker/internal/app"
	"github.com/AhsokaTano26/NikuCooker/internal/events"
	"github.com/AhsokaTano26/NikuCooker/internal/platform"
	"github.com/AhsokaTano26/NikuCooker/internal/project"
	"github.com/AhsokaTano26/NikuCooker/internal/provision"
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

	// Gathered once and shared. The accelerator probe is a separate process
	// with a two-second ceiling, and asking it twice for one page — once for
	// the host section and once for the CUDA option — would double the wait on
	// exactly the machines where it is slowest.
	host := platform.HostStats(ctx, s.app.DataDir(), s.app.Config().Storage.ModelDir)

	s.fillCounts(ctx, &overview.Counts)
	s.fillWorker(ctx, &overview.Worker)
	s.fillRuntime(&overview.Runtime, host.GPU)
	s.fillStats(host, &overview.Stats)

	s.respond(w, http.StatusOK, overview)
}

// fillRuntime reports the state of the provisioned AI environment.
//
// Three separate questions, kept separate because they disagree in the cases
// that matter: whether an install *could* run here, whether one is running, and
// whether an interpreter exists on disk right now — which stays true across a
// restart, and is the one the worker will actually be launched with.
//
// gpus comes from the host stats this same request gathered, rather than from a
// second probe.
func (s *Server) fillRuntime(view *runtimeView, gpus []platform.GPUInfo) {
	view.RuntimeDir = s.app.RuntimeDir()
	view.Available, view.Reason = s.app.ProvisioningAvailable()
	view.Status = string(provision.StatusIdle)

	// Installing is offered only when the startup check found something to fix.
	//
	// "No environment has been provisioned" is not that finding. A checkout with
	// a working ai/.venv, and a machine whose ai.python is set, both transcribe
	// perfectly and have never been provisioned — offering them a 300 MB
	// download would be answering a question nobody asked, and the download is
	// the kind of thing a user notices.
	if view.Available {
		if state := s.app.Environment(); !state.NeedsInstall() {
			view.Available = false
			switch state.State {
			case app.EnvironmentReady:
				view.Reason = "the AI environment is installed and working; nothing to do"
			default:
				view.Reason = "the AI environment is being checked"
			}
		}
	}

	if view.RuntimeDir != "" {
		python := platform.RuntimeVenvPython(view.RuntimeDir)
		if _, err := os.Stat(python); err == nil {
			view.Provisioned = true
			view.Python = python
		}
	}

	// After the block above, because what the environment was built with is a
	// question about an environment that exists.
	s.fillAccelerator(view)
	s.fillCUDA(view, gpus)

	state := s.app.ProvisioningState()
	view.Status = string(state.Status)
	view.Phase = string(state.Phase)
	view.ErrorCode = state.ErrorCode
	view.ErrorMessage = state.ErrorMessage
	view.Remediation = state.Remediation

	if !state.StartedAt.IsZero() {
		started := state.StartedAt
		view.StartedAt = &started
	}
	view.FinishedAt = state.FinishedAt

	// A finished install is reported as ready even on a process that did not
	// perform it: the state lives in the data directory, not in this process.
	if view.Provisioned && state.Status == provision.StatusIdle {
		view.Status = string(provision.StatusReady)
	}
}

// shutdownDelay is how long the shutdown waits after answering the request.
//
// Long enough for the response to reach the browser, short enough that nobody
// notices it. The alternative is closing the connection the caller is reading
// the answer on, which turns a clean stop into a broken pipe.
const shutdownDelay = 250 * time.Millisecond

// requestProcessShutdown ends this process, at the interface's request.
//
// It answers before it stops. The caller is the browser that asked, and the
// answer is the only evidence it gets that the button worked — the connection
// is about to go away, along with every other one.
func (s *Server) requestProcessShutdown(w http.ResponseWriter, r *http.Request) {
	if s.requestShutdown == nil {
		s.fail(w, Failed(http.StatusServiceUnavailable, CodeUnavailable,
			"this server cannot be stopped from the interface; stop the process instead"))
		return
	}

	s.log.Info("shutdown requested through the interface",
		"remote_addr", r.RemoteAddr, "user_agent", r.UserAgent())

	s.respond(w, http.StatusAccepted, map[string]string{"status": "shutting_down"})

	// In a goroutine, because the shutdown it triggers is what ends the request
	// that is still being written.
	go func() {
		time.Sleep(shutdownDelay)
		s.requestShutdown()
	}()
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
	// No worker process is started, and no interpreter is spawned to answer
	// this: the answer comes from the check the server ran at startup, which is
	// also what the event stream carries. A dashboard that spawned Python to
	// answer a status query would be making the thing it was reporting on, and
	// one that re-probed on every request would do it once per open tab.
	status := s.app.Environment()

	view.Status = string(status.State)
	view.Workers = 0
	view.LoadedModels = []loadedModelView{}
	view.Python = status.Python
	view.Detail = status.Detail

	if digest, err := protocol.SchemaDigest(); err == nil {
		view.SchemaDigest = digest
	}
}

// fillCUDA reports whether the GPU dependency set can be offered here.
//
// The same decision the provision request enforces, read from the same
// function, so that the button the page shows and the answer the server gives
// cannot disagree. A machine that cannot use it gets the reason instead of a
// disabled control with no explanation.
func (s *Server) fillCUDA(view *runtimeView, gpus []platform.GPUInfo) {
	available, reason := provision.CUDAUsable(runtime.GOOS, gpus)
	view.CUDA.Available = available
	view.CUDA.ReasonCode = string(reason)
	view.CUDA.ExtraBytes = provision.ExtraCUDABytes
	view.CUDA.GPUs = make([]string, 0, len(gpus))
	for _, gpu := range gpus {
		view.CUDA.GPUs = append(view.CUDA.GPUs, gpu.Name)
	}
}

// fillAccelerator reports which dependency set the environment was built with,
// read from the record the install wrote.
//
// A file this program wrote, and not a guess from the hardware: a card in the
// machine says nothing about what the interpreter beside it was installed with,
// and the two answers differ exactly when someone is wondering whether their
// GPU is being used. No record at all is left empty rather than reported as
// CPU, because that is the case this cannot answer.
func (s *Server) fillAccelerator(view *runtimeView) {
	if !view.Provisioned {
		return
	}

	manifest, err := provision.ReadManifest(platform.RuntimeManifestPath(view.RuntimeDir))
	if err != nil {
		return
	}

	view.Accelerator = "cpu"
	if manifest.Extra == provision.ExtraCUDA {
		view.Accelerator = "cuda"
	}
}

func (s *Server) fillStats(host platform.Stats, stats *systemStats) {
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
