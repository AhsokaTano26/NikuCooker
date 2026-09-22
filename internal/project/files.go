package project

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// The categories a project's files are shown and managed in.
//
// A fixed set rather than free-form paths: the interface names one of these and
// the server decides what it means. A path arriving from a client would be a
// request to delete arbitrary directories, and "the client only ever sends
// these four values" is a property that lasts until someone adds a fifth.
const (
	GroupSource    = "source"
	GroupArtifacts = "artifacts"
	GroupOutput    = "output"
	GroupLogs      = "logs"
)

// FileGroup is one category of a project's files.
type FileGroup struct {
	Kind string `json:"kind"`
	Name string `json:"name"`

	// Bytes and Files are measured, not stored. A project with ten thousand
	// small artifact files is a different problem from one with a single
	// multi-gigabyte source, and only one of the two numbers says which.
	Bytes int64 `json:"bytes"`
	Files int   `json:"files"`

	// Removable reports whether the interface may offer to delete this.
	Removable bool `json:"removable"`

	// Warning is what deleting costs, in the user's terms. Shown beside the
	// button rather than behind it: these four categories differ enormously in
	// what losing them means, and "delete" looks identical on all of them.
	Warning string `json:"warning"`

	// Detail is where the files are, for someone who wants to look.
	Detail string `json:"detail,omitempty"`
}

// groupNames and groupWarnings are the descriptions of each category.
//
// Kept together so that a category cannot be added to the list below without
// someone deciding what to say about it.
var groupDescriptions = map[string]struct {
	name    string
	warning string
}{
	GroupSource: {
		name: "源视频",
		warning: "删除后这个项目无法再运行。如果是上传进来的，" +
			"本机那份原件可能已经删了，那就再也找不回来了。",
	},
	GroupArtifacts: {
		name:    "中间产物",
		warning: "识别、切分、翻译的中间结果。重新运行时会自动重算，不会丢失任何成果。",
	},
	GroupOutput: {
		name:    "成品",
		warning: "字幕文件、质量报告和带字幕的视频。删掉要重新运行才有。",
	},
	GroupLogs: {
		name:    "运行日志",
		warning: "每次运行的日志，用来排查当时发生了什么。",
	},
}

// FileGroups measures a project's directories.
//
// A directory that does not exist is reported as empty rather than omitted, so
// the interface shows the same four rows for every project — one that appears
// and disappears as a project is run reads as something being wrong.
func FileGroups(dataDir, id string) ([]FileGroup, error) {
	dir, err := projectDirFor(dataDir, id)
	if err != nil {
		return nil, err
	}

	groups := make([]FileGroup, 0, len(groupDescriptions))
	for _, kind := range []string{GroupSource, GroupArtifacts, GroupOutput, GroupLogs} {
		description := groupDescriptions[kind]

		group := FileGroup{
			Kind:      kind,
			Name:      description.name,
			Warning:   description.warning,
			Removable: true,
			Detail:    filepath.Join(dir, kind),
		}

		path := group.Detail
		if kind == GroupSource {
			// The source is a file inside its directory rather than the
			// directory itself, and measuring the directory would be the same
			// number while naming the wrong thing.
			group.Detail = filepath.Join(dir, DirSource)
		}

		bytes, files, err := measure(path)
		if err != nil {
			return nil, err
		}
		group.Bytes, group.Files = bytes, files

		groups = append(groups, group)
	}
	return groups, nil
}

// RemoveGroup deletes one category of a project's files.
//
// `extra` is called after the directory is gone, for the bookkeeping the
// directory was the other half of — the artifact rows, the source path. It
// exists so that the caller cannot delete a directory and forget the row that
// pointed into it, which is the failure that leaves a project looking intact
// while every stage reads from an empty directory.
func RemoveGroup(dataDir, id, kind string, extra func(ctx context.Context) error) (int64, error) {
	if _, ok := groupDescriptions[kind]; !ok {
		return 0, fmt.Errorf("project: %q is not a category of project files", kind)
	}

	dir, err := projectDirFor(dataDir, id)
	if err != nil {
		return 0, err
	}

	path := filepath.Join(dir, kind)

	bytes, _, err := measure(path)
	if err != nil {
		return 0, err
	}

	if err := os.RemoveAll(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return 0, fmt.Errorf("project: remove %s: %w", kind, err)
	}

	// Recreated so that the project keeps its shape. A run writes into these
	// directories, and one that has to be recreated by whichever stage gets
	// there first is a directory whose existence depends on the order of the
	// pipeline.
	if kind != GroupSource {
		if err := os.MkdirAll(path, 0o755); err != nil {
			return bytes, fmt.Errorf("project: recreate %s: %w", kind, err)
		}
	}

	if extra != nil {
		if err := extra(context.Background()); err != nil {
			return bytes, err
		}
	}
	return bytes, nil
}

// ClearSourcePath forgets where a project's media was, after it is deleted.
//
// Without this the project keeps pointing at a file that is not there, and
// every run fails at the probe stage with a message about a missing file
// rather than about a project whose source was deleted on purpose.
func (s *Service) ClearSourcePath(ctx context.Context, id string) error {
	if _, err := s.Get(ctx, id); err != nil {
		return err
	}

	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := s.db.Write.ExecContext(ctx,
		`UPDATE projects SET source_path = '', updated_at = ? WHERE id = ?`,
		stamp, id); err != nil {
		return fmt.Errorf("project: clear source for %s: %w", id, err)
	}
	return nil
}

// projectDirFor resolves and validates a project directory.
func projectDirFor(dataDir, id string) (string, error) {
	if err := validateID(id); err != nil {
		return "", err
	}
	return Dir(dataDir, id), nil
}

// measure sums a path's files and their sizes.
//
// A path that is not there measures as zero: "this project has no logs yet" is
// the ordinary state of a new project, not a failure to report.
func measure(path string) (bytes int64, files int, err error) {
	err = filepath.WalkDir(path, func(_ string, entry fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if entry.IsDir() {
			return nil
		}

		info, infoErr := entry.Info()
		if infoErr != nil {
			// Vanished between the walk and the stat. Counting the ones that
			// remain is more useful than failing the whole measurement.
			return nil
		}
		bytes += info.Size()
		files++
		return nil
	})
	if err != nil {
		return 0, 0, fmt.Errorf("project: measure %s: %w", path, err)
	}
	return bytes, files, nil
}

// LogFiles lists the run logs of a project, newest first.
//
// Separate from FileGroups because the logs are the one category the interface
// lists individually: they are few, they are named after what produced them,
// and the reason to keep them is to open one.
func LogFiles(dataDir, id string) ([]OutputFile, error) {
	dir, err := projectDirFor(dataDir, id)
	if err != nil {
		return nil, err
	}

	logs := filepath.Join(dir, DirLogs)
	entries, err := os.ReadDir(logs)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return []OutputFile{}, nil
		}
		return nil, fmt.Errorf("project: read %s: %w", logs, err)
	}

	out := make([]OutputFile, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		info, infoErr := entry.Info()
		if infoErr != nil {
			continue
		}
		out = append(out, OutputFile{
			Name:       entry.Name(),
			SizeBytes:  info.Size(),
			ModifiedAt: info.ModTime().UTC(),
		})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].ModifiedAt.After(out[j].ModifiedAt) })
	return out, nil
}

// ResolveLog returns the path of one run log.
//
// The same traversal defence as ResolveOutput, and for the same reason: the
// name comes from a URL.
func ResolveLog(dataDir, id, name string) (string, error) {
	dir, err := projectDirFor(dataDir, id)
	if err != nil {
		return "", err
	}
	return resolveIn(filepath.Join(dir, DirLogs), name)
}

// TotalBytes measures everything a project occupies on disk.
func TotalBytes(dataDir, id string) (int64, error) {
	dir, err := projectDirFor(dataDir, id)
	if err != nil {
		return 0, err
	}

	bytes, _, err := measure(dir)
	return bytes, err
}
