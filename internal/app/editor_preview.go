package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/AhsokaTano26/NikuCooker/internal/media"
	"github.com/AhsokaTano26/NikuCooker/internal/project"
)

const editorPreviewName = "editor-preview.mp4"

// SourceMediaPath resolves the source recorded by a project without accepting
// a path from the caller. It is the safe boundary used by the editor's streaming
// endpoint.
func (a *App) SourceMediaPath(ctx context.Context, projectID string) (string, error) {
	prj, err := a.Projects.Get(ctx, projectID)
	if err != nil {
		return "", err
	}
	return a.sourcePath(prj)
}

// EditorPreviewPath returns the fixed cache path for the browser-compatible
// review proxy. The project lookup is intentional: a missing project must not
// become a path under the data directory merely because its id looks valid.
func (a *App) EditorPreviewPath(ctx context.Context, projectID string) (string, error) {
	if _, err := a.Projects.Get(ctx, projectID); err != nil {
		return "", err
	}
	return filepath.Join(project.Dir(a.dataDir, projectID), project.DirArtifacts, editorPreviewName), nil
}

// GenerateEditorPreview creates a browser-compatible proxy atomically.
// Readers either see the previous complete file or the new complete file,
// never FFmpeg's partially written output.
func (a *App) GenerateEditorPreview(
	ctx context.Context,
	projectID string,
	onProgress func(float64, string),
) (string, error) {
	source, err := a.SourceMediaPath(ctx, projectID)
	if err != nil {
		return "", err
	}
	output, err := a.EditorPreviewPath(ctx, projectID)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(output), 0o755); err != nil {
		return "", fmt.Errorf("app: create editor preview directory: %w", err)
	}

	info, err := a.Media.Probe(ctx, source)
	if err != nil {
		return "", err
	}

	temporary := filepath.Join(filepath.Dir(output), ".editor-preview.part.mp4")
	_ = os.Remove(temporary)
	defer func() { _ = os.Remove(temporary) }()

	err = a.Media.GeneratePreview(ctx, source, temporary, media.PreviewOptions{
		ScaleHeight: 720,
		CRF:         25,
		Preset:      "veryfast",
	}, info.Duration, onProgress)
	if err != nil {
		return "", err
	}
	if err := os.Rename(temporary, output); err != nil {
		return "", fmt.Errorf("app: publish editor preview: %w", err)
	}
	return output, nil
}
