package tests

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type releaseConfig struct {
	Changelog struct {
		Groups []struct {
			Title  string `yaml:"title"`
			Regexp string `yaml:"regexp"`
		} `yaml:"groups"`
	} `yaml:"changelog"`
	Release struct {
		Draft  *bool  `yaml:"draft"`
		Header string `yaml:"header"`
	} `yaml:"release"`
}

type releaseWorkflow struct {
	Jobs struct {
		Release struct {
			Steps []struct {
				Name string `yaml:"name"`
				Run  string `yaml:"run"`
			} `yaml:"steps"`
		} `yaml:"release"`
	} `yaml:"jobs"`
}

func TestTaggedReleasePublishesAfterArchiveVerificationWithReadableNotes(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", ".goreleaser.yaml"))
	if err != nil {
		t.Fatal(err)
	}

	var cfg releaseConfig
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("parse .goreleaser.yaml: %v", err)
	}

	if cfg.Release.Draft == nil || !*cfg.Release.Draft {
		t.Error("artifacts must stay in a draft until the archive verification passes")
	}
	if strings.TrimSpace(cfg.Release.Header) == "" {
		t.Error("release notes need a user-facing introduction before the commit list")
	}

	for _, message := range []string{
		"feat(web): add subtitle review",
		"fix(render): keep soft subtitles",
	} {
		if title := matchingReleaseGroup(t, cfg.Changelog.Groups, message); title == "" {
			t.Errorf("commit %q is not placed in a readable release-note group", message)
		}
	}

	workflowRaw, err := os.ReadFile(filepath.Join("..", ".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var workflow releaseWorkflow
	if err := yaml.Unmarshal(workflowRaw, &workflow); err != nil {
		t.Fatalf("parse release workflow: %v", err)
	}

	verifiedAt := workflowStepIndex(workflow.Jobs.Release.Steps, "Check what the archives contain")
	publishedAt := workflowStepIndex(workflow.Jobs.Release.Steps, "Publish verified release")
	if verifiedAt < 0 {
		t.Fatal("release workflow does not verify archive contents")
	}
	if publishedAt <= verifiedAt {
		t.Fatal("release workflow must publish only after archive verification")
	}
	publishCommand := workflow.Jobs.Release.Steps[publishedAt].Run
	for _, required := range []string{"gh release edit", "--draft=false", "--latest"} {
		if !strings.Contains(publishCommand, required) {
			t.Errorf("publish step is missing %q", required)
		}
	}
}

func workflowStepIndex(steps []struct {
	Name string `yaml:"name"`
	Run  string `yaml:"run"`
}, name string) int {
	for index, step := range steps {
		if step.Name == name {
			return index
		}
	}
	return -1
}

func matchingReleaseGroup(t *testing.T, groups []struct {
	Title  string `yaml:"title"`
	Regexp string `yaml:"regexp"`
}, message string) string {
	t.Helper()
	for _, group := range groups {
		if group.Regexp == "" {
			continue
		}
		pattern, err := regexp.Compile(group.Regexp)
		if err != nil {
			t.Fatalf("compile release-note group %q: %v", group.Title, err)
		}
		if pattern.MatchString(message) {
			return group.Title
		}
	}
	return ""
}
