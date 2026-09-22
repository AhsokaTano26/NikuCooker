package api

import (
	"errors"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/AhsokaTano26/NikuCooker/internal/project"
)

// outputView is one published file.
type outputView struct {
	Name       string    `json:"name"`
	SizeBytes  int64     `json:"size_bytes"`
	ModifiedAt time.Time `json:"modified_at"`

	// Kind is a coarse label the interface groups by: subtitle, video, log,
	// other. Derived from the extension rather than from the stage that wrote
	// it, because that is what a person deciding what to download is looking at.
	Kind string `json:"kind"`

	// Description says what the file is and what it is for.
	//
	// Sent from here rather than written into the frontend because the answer
	// depends on the file: an .ass and an .srt are both "subtitles" and are not
	// interchangeable, and the two .mkv files differ in a way that only shows
	// when someone plays one. The server knows which is which; a client
	// matching on filename patterns would be a second implementation of that
	// knowledge, free to drift.
	Description string `json:"description"`
}

// describe says what a published file is, in the user's terms.
//
// Matched on the name because that is all a published file has: the stage that
// wrote it is not recorded on it, and the names are ours — the subtitle stage
// writes subs.<format> and the render stage writes <project>.nikucooker[.hardsub].mkv.
func describe(name string) string {
	lower := strings.ToLower(name)

	switch {
	case strings.HasSuffix(lower, ".hardsub.mkv"):
		return "字幕已经画进画面，任何播放器都关不掉。文件更大，因为整片重新编码过。"
	case strings.HasSuffix(lower, ".nikucooker.mkv"):
		return "字幕是独立轨道，播放器里可以开关或换字体。视频流是直接复制的，没有重新编码。"
	case strings.HasSuffix(lower, ".mkv"), strings.HasSuffix(lower, ".mp4"), strings.HasSuffix(lower, ".webm"):
		return "带字幕的视频。"
	case strings.HasSuffix(lower, ".ass"):
		return "带样式的字幕：字体、字号、描边、位置都在文件里。渲染阶段用的就是这份。"
	case strings.HasSuffix(lower, ".srt"):
		return "最通用的字幕格式，几乎所有播放器都能读，但不带样式。"
	case strings.HasSuffix(lower, ".vtt"):
		return "网页字幕格式，适合在浏览器里播放。"
	default:
		return ""
	}
}

func (s *Server) listOutputs(w http.ResponseWriter, r *http.Request) {
	id, apiErr := projectID(r)
	if apiErr != nil {
		s.fail(w, apiErr)
		return
	}

	// The project is loaded first so that a project that does not exist is a
	// 404 rather than an empty list, which would read as "this project finished
	// a run and produced nothing".
	if _, err := s.app.Projects.Get(r.Context(), id); err != nil {
		s.fail(w, classify(err))
		return
	}

	files, err := s.app.OutputFiles(id)
	if err != nil {
		s.fail(w, classify(err))
		return
	}

	views := make([]outputView, 0, len(files))
	for _, file := range files {
		views = append(views, outputView{
			Name:        file.Name,
			SizeBytes:   file.SizeBytes,
			ModifiedAt:  file.ModifiedAt,
			Kind:        outputKind(file.Name),
			Description: describe(file.Name),
		})
	}

	s.respond(w, http.StatusOK, struct {
		Items []outputView `json:"items"`

		// Dir is where they are on the machine running the core. It is what a
		// user who is not going to download anything needs, and it is the
		// answer to "the run finished, where is my file".
		Dir string `json:"dir"`
	}{Items: views, Dir: project.OutputDir(s.app.DataDir(), id)})
}

func (s *Server) downloadOutput(w http.ResponseWriter, r *http.Request) {
	id, apiErr := projectID(r)
	if apiErr != nil {
		s.fail(w, apiErr)
		return
	}

	name := r.PathValue("name")
	path, err := s.app.OutputPath(id, name)
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
		// Raced with a re-run replacing the directory, or the file was removed
		// underneath us. Either way it is gone as far as this request goes.
		s.fail(w, NotFound(name))
		return
	}
	defer func() { _ = file.Close() }()

	info, err := file.Stat()
	if err != nil {
		s.fail(w, classify(err))
		return
	}

	// The name is a basename by the time it gets here, but it is still the
	// client's string, so the header is built from it with the same care the
	// subtitle download uses: both forms, so a browser and a script each find
	// the one they understand.
	w.Header().Set("Content-Type", outputContentType(name))
	w.Header().Set("Content-Disposition",
		`attachment; filename="`+asciiFallback(name)+`"; filename*=UTF-8''`+url.PathEscape(name))
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))

	// Served from disk rather than read into memory first. A rendered video is
	// gigabytes, and the subtitle download's buffer-it-then-write approach is
	// there to avoid a truncated *generated* file — this one already exists and
	// is complete.
	http.ServeContent(w, r, name, info.ModTime(), file)
}

// outputKind groups files the way the interface shows them.
func outputKind(name string) string {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".srt", ".ass", ".vtt":
		return "subtitle"
	case ".mkv", ".mp4", ".webm":
		return "video"
	case ".log":
		return "log"
	default:
		return "other"
	}
}

// outputContentType prefers the system's table and falls back to a sensible
// default, because an empty content type makes some browsers download a file
// with no idea what it is.
func outputContentType(name string) string {
	if byExt := mime.TypeByExtension(strings.ToLower(filepath.Ext(name))); byExt != "" {
		return byExt
	}

	switch outputKind(name) {
	case "subtitle":
		return "text/plain; charset=utf-8"
	case "video":
		return "video/x-matroska"
	default:
		return "application/octet-stream"
	}
}
