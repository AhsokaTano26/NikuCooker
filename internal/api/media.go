package api

import (
	"context"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/AhsokaTano26/NikuCooker/internal/events"
)

type previewState struct {
	Status       string
	Progress     float64
	ErrorMessage string
	UpdatedAt    time.Time
}

type previewView struct {
	Status         string  `json:"status"`
	Progress       float64 `json:"progress,omitempty"`
	ErrorMessage   string  `json:"error_message,omitempty"`
	SourceURL      string  `json:"source_url"`
	PreviewURL     string  `json:"preview_url,omitempty"`
	DirectPlayback bool    `json:"direct_playback"`
	MediaKind      string  `json:"media_kind"`
}

// streamSourceMedia serves only the source recorded by the project.
//
// The client never supplies a path. Besides keeping arbitrary files outside
// the HTTP surface, that means moving the data directory keeps this endpoint
// valid because the project service resolves the relative source path each
// time. ServeContent supplies byte ranges, which native media controls need
// for seeking without downloading the whole file first.
func (s *Server) streamSourceMedia(w http.ResponseWriter, r *http.Request) {
	id, apiErr := projectID(r)
	if apiErr != nil {
		s.fail(w, apiErr)
		return
	}

	prj, err := s.app.Projects.Get(r.Context(), id)
	if err != nil {
		s.fail(w, classify(err))
		return
	}

	path := s.app.Projects.SourceAbs(prj)
	if path == "" {
		s.fail(w, NotFound("this project has no source media"))
		return
	}

	file, err := os.Open(path)
	if err != nil {
		s.fail(w, NotFound("the project's source media"))
		return
	}
	defer func() { _ = file.Close() }()

	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		s.fail(w, NotFound("the project's source media"))
		return
	}

	name := filepath.Base(path)
	w.Header().Set("Content-Type", mediaContentType(name))
	w.Header().Set("Content-Disposition",
		`inline; filename="`+asciiFallback(name)+`"; filename*=UTF-8''`+url.PathEscape(name))
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, name, info.ModTime(), file)
}

func (s *Server) getEditorPreview(w http.ResponseWriter, r *http.Request) {
	id, apiErr := projectID(r)
	if apiErr != nil {
		s.fail(w, apiErr)
		return
	}
	s.respondPreviewState(w, r, id, http.StatusOK)
}

func (s *Server) startEditorPreview(w http.ResponseWriter, r *http.Request) {
	id, apiErr := projectID(r)
	if apiErr != nil {
		s.fail(w, apiErr)
		return
	}
	if _, err := s.app.SourceMediaPath(r.Context(), id); err != nil {
		s.fail(w, classify(err))
		return
	}
	if path, err := s.app.EditorPreviewPath(r.Context(), id); err == nil {
		if info, statErr := os.Stat(path); statErr == nil && info.Mode().IsRegular() {
			s.respondPreviewState(w, r, id, http.StatusOK)
			return
		}
	}

	s.previewMu.Lock()
	state := s.previews[id]
	if state.Status == "running" {
		s.previewMu.Unlock()
		s.respondPreviewState(w, r, id, http.StatusAccepted)
		return
	}
	s.previews[id] = previewState{Status: "running", UpdatedAt: time.Now().UTC()}
	s.previewMu.Unlock()

	go s.generateEditorPreview(contextWithoutCancel(), id)
	s.respondPreviewState(w, r, id, http.StatusAccepted)
}

func (s *Server) generateEditorPreview(ctx context.Context, projectID string) {
	set := func(state previewState) {
		state.UpdatedAt = time.Now().UTC()
		s.previewMu.Lock()
		s.previews[projectID] = state
		s.previewMu.Unlock()

		event := events.New(events.TypeMediaPreview).
			ForProject(projectID).
			With("status", state.Status).
			With("progress", state.Progress)
		if state.ErrorMessage != "" {
			event = event.With("error_message", state.ErrorMessage)
		}
		s.app.Events.Emit(event)
	}

	_, err := s.app.GenerateEditorPreview(ctx, projectID, func(progress float64, message string) {
		set(previewState{Status: "running", Progress: progress})
	})
	if err != nil {
		s.log.Error("editor preview generation failed", "project_id", projectID, "error", err)
		set(previewState{Status: "failed", ErrorMessage: "无法生成网页预览，请检查源文件和 FFmpeg 后重试。"})
		return
	}
	set(previewState{Status: "ready", Progress: 1})
}

func (s *Server) respondPreviewState(w http.ResponseWriter, r *http.Request, id string, status int) {
	source, err := s.app.SourceMediaPath(r.Context(), id)
	if err != nil {
		s.fail(w, classify(err))
		return
	}

	s.previewMu.Lock()
	state := s.previews[id]
	s.previewMu.Unlock()

	previewPath, err := s.app.EditorPreviewPath(r.Context(), id)
	if err != nil {
		s.fail(w, classify(err))
		return
	}
	if info, statErr := os.Stat(previewPath); statErr == nil && info.Mode().IsRegular() {
		state = previewState{Status: "ready", Progress: 1, UpdatedAt: info.ModTime()}
	}
	if state.Status == "" {
		state.Status = "missing"
	}

	ext := strings.ToLower(filepath.Ext(source))
	kind := "video"
	if isAudioExtension(ext) {
		kind = "audio"
	}
	view := previewView{
		Status:         state.Status,
		Progress:       state.Progress,
		ErrorMessage:   state.ErrorMessage,
		SourceURL:      "/api/v1/projects/" + id + "/media/source",
		DirectPlayback: supportsDirectPlayback(ext),
		MediaKind:      kind,
	}
	if state.Status == "ready" {
		view.PreviewURL = "/api/v1/projects/" + id + "/media/preview/file"
	}
	s.respond(w, status, view)
}

func (s *Server) streamEditorPreview(w http.ResponseWriter, r *http.Request) {
	id, apiErr := projectID(r)
	if apiErr != nil {
		s.fail(w, apiErr)
		return
	}
	path, err := s.app.EditorPreviewPath(r.Context(), id)
	if err != nil {
		s.fail(w, classify(err))
		return
	}
	s.streamMediaFile(w, r, path, "editor-preview.mp4")
}

func (s *Server) streamMediaFile(w http.ResponseWriter, r *http.Request, path, name string) {
	file, err := os.Open(path)
	if err != nil {
		s.fail(w, NotFound("the editor preview"))
		return
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		s.fail(w, NotFound("the editor preview"))
		return
	}
	w.Header().Set("Content-Type", mediaContentType(name))
	w.Header().Set("Content-Disposition", `inline; filename="`+name+`"`)
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, name, info.ModTime(), file)
}

func isAudioExtension(ext string) bool {
	switch ext {
	case ".mp3", ".m4a", ".aac", ".wav", ".flac", ".ogg", ".opus":
		return true
	default:
		return false
	}
}

func supportsDirectPlayback(ext string) bool {
	switch ext {
	case ".mp4", ".m4v", ".webm", ".mp3", ".m4a", ".aac", ".wav", ".ogg", ".opus":
		return true
	default:
		return false
	}
}

func mediaContentType(name string) string {
	ext := strings.ToLower(filepath.Ext(name))
	if byExt := mime.TypeByExtension(ext); byExt != "" {
		return byExt
	}

	switch ext {
	case ".mkv":
		return "video/x-matroska"
	case ".m4a":
		return "audio/mp4"
	case ".flac":
		return "audio/flac"
	case ".wav":
		return "audio/wav"
	default:
		return "application/octet-stream"
	}
}
