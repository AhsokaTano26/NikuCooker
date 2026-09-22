package api

import (
	"errors"
	"net/http"
	"os"
	"strconv"

	"github.com/AhsokaTano26/NikuCooker/internal/project"
)

// fileGroupView is one category of a project's files.
type fileGroupView struct {
	Kind string `json:"kind"`
	Name string `json:"name"`

	Bytes int64 `json:"bytes"`
	Files int   `json:"files"`

	Removable bool   `json:"removable"`
	Warning   string `json:"warning"`
	Detail    string `json:"detail,omitempty"`
}

func (s *Server) listProjectFiles(w http.ResponseWriter, r *http.Request) {
	id, apiErr := projectID(r)
	if apiErr != nil {
		s.fail(w, apiErr)
		return
	}

	if _, err := s.app.Projects.Get(r.Context(), id); err != nil {
		s.fail(w, classify(err))
		return
	}

	groups, err := s.app.ProjectFiles(id)
	if err != nil {
		s.fail(w, classify(err))
		return
	}

	logs, err := s.app.ProjectLogs(id)
	if err != nil {
		s.fail(w, classify(err))
		return
	}

	views := make([]fileGroupView, 0, len(groups))
	var total int64

	for _, group := range groups {
		total += group.Bytes
		views = append(views, fileGroupView{
			Kind: group.Kind, Name: group.Name,
			Bytes: group.Bytes, Files: group.Files,
			Removable: group.Removable, Warning: group.Warning,
			Detail: group.Detail,
		})
	}

	logViews := make([]outputView, 0, len(logs))
	for _, file := range logs {
		logViews = append(logViews, outputView{
			Name: file.Name, SizeBytes: file.SizeBytes,
			ModifiedAt: file.ModifiedAt, Kind: "log",
			Description: "这一次运行的完整记录，包括每个阶段说了什么。",
		})
	}

	s.respond(w, http.StatusOK, struct {
		Items      []fileGroupView `json:"items"`
		TotalBytes int64           `json:"total_bytes"`
		Logs       []outputView    `json:"logs"`
	}{Items: views, TotalBytes: total, Logs: logViews})
}

func (s *Server) deleteProjectFiles(w http.ResponseWriter, r *http.Request) {
	id, apiErr := projectID(r)
	if apiErr != nil {
		s.fail(w, apiErr)
		return
	}

	// The category is a name from a fixed set, never a path: the server decides
	// what each one means. See internal/project/files.go.
	kind := r.PathValue("kind")
	switch kind {
	case project.GroupSource, project.GroupArtifacts, project.GroupOutput, project.GroupLogs:
	default:
		s.fail(w, Invalid("unknown file category "+strconv.Quote(kind)+
			"; expected source, artifacts, output or logs"))
		return
	}

	if _, err := s.app.Projects.Get(r.Context(), id); err != nil {
		s.fail(w, classify(err))
		return
	}

	bytes, err := s.app.RemoveFileGroup(r.Context(), id, kind)
	if err != nil {
		s.fail(w, classify(err))
		return
	}

	s.emitProject(id)
	s.log.Info("removed project files", "project_id", id, "category", kind, "reclaimed_bytes", bytes)

	s.respond(w, http.StatusOK, struct {
		ReclaimedBytes int64 `json:"reclaimed_bytes"`
	}{ReclaimedBytes: bytes})
}

func (s *Server) downloadLog(w http.ResponseWriter, r *http.Request) {
	id, apiErr := projectID(r)
	if apiErr != nil {
		s.fail(w, apiErr)
		return
	}

	name := r.PathValue("name")
	path, err := s.app.LogPath(id, name)
	if err != nil {
		if errors.Is(err, project.ErrNoOutput) {
			s.fail(w, NotFound(name))
			return
		}
		s.fail(w, classify(err))
		return
	}

	file, err := os.Open(path)
	if err != nil {
		s.fail(w, NotFound(name))
		return
	}
	defer func() { _ = file.Close() }()

	info, err := file.Stat()
	if err != nil {
		s.fail(w, classify(err))
		return
	}

	// Plain text rather than a download: a log is read, not saved, and the
	// browser opening it beats a file landing in a downloads folder.
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	http.ServeContent(w, r, name, info.ModTime(), file)
}
