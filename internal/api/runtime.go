package api

import (
	"errors"
	"net/http"
	"runtime"

	"github.com/AhsokaTano26/NikuCooker/internal/app"
	"github.com/AhsokaTano26/NikuCooker/internal/events"
	"github.com/AhsokaTano26/NikuCooker/internal/platform"
	"github.com/AhsokaTano26/NikuCooker/internal/provision"
)

// provisionBody is what an install can be asked for.
type provisionBody struct {
	// CUDA asks for the NVIDIA dependency set. Extra download, and the only
	// reason the environment is not one fixed set of packages.
	CUDA bool `json:"cuda"`
}

// provisionRuntime installs the AI environment, in the background.
//
// The shape is the model download's, and for the same reasons: it answers 202
// and returns, and the interface follows the event stream. The client that
// asked may close the tab, and several hundred megabytes should still arrive.
func (s *Server) provisionRuntime(w http.ResponseWriter, r *http.Request) {
	// The body is optional. Asking for the default environment is a request
	// with nothing in it, and requiring `{}` to say so would be a formality.
	var body provisionBody
	if r.ContentLength != 0 {
		if apiErr := decode(r, &body); apiErr != nil {
			s.fail(w, apiErr)
			return
		}
	}

	if ok, reason := s.app.ProvisioningAvailable(); !ok {
		s.fail(w, Failed(http.StatusPreconditionFailed, CodeUnavailable, reason))
		return
	}

	extra := ""
	if body.CUDA {
		// Checked rather than taken on trust: the interface does not offer this
		// on a machine that cannot use it, and a request that arrives anyway is
		// answered with the reason instead of seven hundred megabytes of
		// libraries that nothing would load.
		usable, reason := provision.CUDAUsable(runtime.GOOS, platform.GPUs(r.Context()))
		if !usable {
			s.fail(w, Failed(http.StatusPreconditionFailed, CodeUnavailable,
				"GPU acceleration was requested, but "+reason.Message()))
			return
		}
		extra = provision.ExtraCUDA
	}

	// Only when the check found something missing. The interface does not offer
	// the button otherwise, and a request that arrives anyway is answered with
	// the reason rather than with several hundred megabytes of download.
	if state := s.app.Environment(); !state.NeedsInstall() {
		if state.State == app.EnvironmentReady {
			s.fail(w, conflict(CodeConflict,
				"the AI environment is already installed and working; "+
					"delete the runtime directory and start again to replace it"))
			return
		}
		s.fail(w, conflict(CodeConflict, "the AI environment is still being checked"))
		return
	}

	// Two pools would mean twice the Python processes, each loading a model.
	// The same judgment the model deletion makes.
	if s.app.Scheduler.AnyRunning() {
		s.fail(w, conflict(CodeConflict,
			"a job is running; install the AI environment once it has finished"))
		return
	}

	s.log.Info("AI environment install requested through the interface",
		"remote_addr", r.RemoteAddr, "extra", extra)

	if err := s.app.StartProvision(s.provisionReporter(), extra); err != nil {
		if errors.Is(err, provision.ErrBusy) {
			s.fail(w, conflict(CodeConflict, "an install is already running"))
			return
		}
		s.fail(w, classify(err))
		return
	}

	s.respond(w, http.StatusAccepted, map[string]string{"status": "installing"})
}

// cancelProvision stops an install in progress.
func (s *Server) cancelProvision(w http.ResponseWriter, r *http.Request) {
	s.log.Info("AI environment install cancelled through the interface")

	if !s.app.CancelProvision() {
		s.fail(w, conflict(CodeConflict, "no install is running"))
		return
	}
	s.respond(w, http.StatusAccepted, map[string]string{"status": "cancelling"})
}

// provisionReporter turns phase transitions into events.
//
// One event per transition, so at most five per install. The installer narrates
// through uv, which writes thousands of lines; forwarding them would flood a
// stream every open tab is reading and trip the subscriber overflow path that
// exists to protect slow clients.
func (s *Server) provisionReporter() func(provision.Progress) {
	return func(p provision.Progress) {
		event := events.New(events.TypeRuntimeProvision).
			With("phase", string(p.Phase)).
			With("status", string(p.Status))

		if p.Message != "" {
			event = event.With("message", p.Message)
		}
		if p.Err != nil {
			event = event.With("error_message", p.Err.Error())
		}
		if p.Status == provision.StatusReady {
			if state := s.app.ProvisioningState(); state.Python != "" {
				event = event.With("python", state.Python)
			}
		}

		s.app.Events.Emit(event)

		// Also logged, so that an install that failed at three in the morning is
		// in the log file rather than only in a stream nobody was reading.
		if p.Status == provision.StatusFailed && p.Err != nil {
			s.log.Warn("AI environment install failed",
				"phase", string(p.Phase), "error", p.Err)
		}
	}
}
