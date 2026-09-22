package api

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/AhsokaTano26/NikuCooker/internal/project"
)

// ---------------------------------------------------------------------------
// Uploads
// ---------------------------------------------------------------------------

// uploadView describes a staged file.
//
// The name is echoed back because the browser already knows it, and confirming
// it is how a user notices they picked the wrong file before waiting for a
// multi-gigabyte upload to finish.
type uploadView struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	SizeBytes int64  `json:"size_bytes"`

	// MaxBytes is what this server will accept, so the interface can refuse a
	// file it already knows is too large instead of streaming it to find out.
	MaxBytes int64 `json:"max_bytes"`
}

// uploadFile is the multipart field the file must arrive in.
//
// A named field rather than "the first part", so that a request with a stray
// extra part is an error rather than a coin flip over which file was meant.
const uploadFile = "file"

// createUpload accepts a file into the staging area.
//
// Open by default, unlike the server-side path source, and the asymmetry is
// deliberate rather than an oversight. Reading a path grants access to
// everything the process can see; accepting an upload grants only the right to
// write into a directory this server names, under a name it derives, up to a
// size it chooses. Nothing is read, nothing existing is overwritten, and the
// limit is enforced while streaming instead of after.
func (s *Server) createUpload(w http.ResponseWriter, r *http.Request) {
	limit := s.app.Config().Server.MaxUploadBytes

	// Refused before a byte of body is read, when the client volunteers the
	// size. Nobody should wait for 20 GB to stream in and be told it was too
	// big, and an honest client always knows.
	if r.ContentLength > 0 {
		// Multipart framing adds a little over the file itself; the generous
		// allowance keeps this a fast-path rejection rather than a second,
		// stricter limit that disagrees with the one enforced while streaming.
		if r.ContentLength > limit+multipartOverhead {
			s.fail(w, tooLarge(limit))
			return
		}
	}

	reader, err := r.MultipartReader()
	if err != nil {
		s.fail(w, Invalid("expected a multipart/form-data upload: "+err.Error()))
		return
	}

	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			// A malformed or truncated body. Reported as the client's mistake,
			// because that is what it is.
			s.fail(w, Invalid("reading the upload: "+err.Error()))
			return
		}

		name := part.FormName()
		if name != uploadFile {
			// Drained rather than abandoned, so the next part can be read. A
			// part left unread desynchronises the multipart stream.
			_, _ = io.Copy(io.Discard, part)
			_ = part.Close()
			continue
		}

		staged, stageErr := s.app.Uploads.Stage(r.Context(), part.FileName(), part, limit)
		_ = part.Close()

		if errors.Is(stageErr, project.ErrUploadTooLarge) {
			s.fail(w, tooLarge(limit))
			return
		}
		if stageErr != nil {
			s.fail(w, classify(stageErr))
			return
		}

		s.log.Info("upload staged",
			"upload", staged.ID, "name", staged.Name, "bytes", staged.Size)

		s.respond(w, http.StatusCreated, uploadView{
			ID: staged.ID, Name: staged.Name, SizeBytes: staged.Size, MaxBytes: limit,
		})
		return
	}

	s.fail(w, Invalid("no file in the request; expected a multipart part named "+uploadFile))
}

func (s *Server) deleteUpload(w http.ResponseWriter, r *http.Request) {
	if err := s.app.Uploads.Remove(r.PathValue("uploadID")); err != nil {
		s.fail(w, classify(err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// tooLarge renders the size limit the way a person reads it.
func tooLarge(limit int64) *Error {
	return Failed(http.StatusRequestEntityTooLarge, CodeInvalid,
		fmt.Sprintf("that file is larger than this server accepts (%s); raise server.max_upload_bytes to change the limit",
			humanBytes(limit)))
}

// multipartOverhead is how much multipart framing is allowed on top of the
// file itself. Boundaries and part headers are measured in hundreds of bytes;
// this is loose enough to never be the thing that rejects a valid upload.
const multipartOverhead = 1 << 20

// humanBytes renders a byte count for a message a person reads.
//
// Its own function rather than a shared one because this is the only place the
// server renders sizes as prose — the interface formats its own, in the user's
// locale and units.
func humanBytes(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}

	value := float64(bytes)
	for _, suffix := range []string{"KiB", "MiB", "GiB", "TiB"} {
		value /= unit
		if value < unit {
			return strings.TrimSuffix(fmt.Sprintf("%.1f", value), ".0") + " " + suffix
		}
	}
	return fmt.Sprintf("%.1f PiB", value/unit)
}
