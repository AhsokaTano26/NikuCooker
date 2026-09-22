// Package prompts holds the instructions sent to a language model.
//
// They live in files rather than in Go string literals because they are the part
// of this system that changes most often and are edited by people who are not
// reading the surrounding code. A prompt written as a Go raw string is also
// impossible to diff in review, which is how a prompt regression reaches
// production unnoticed.
//
// Every template is compiled and hashed at load time. The hash is what makes
// editing a prompt invalidate the translations cached against the old one —
// without it, a user who improves a prompt and re-runs would get the previous
// prompt's output back from the cache, and conclude the change did nothing.
package prompts

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"path"
	"sort"
	"strings"
	"sync"
	"text/template"
)

//go:embed translation analysis
var files embed.FS

// Template is a compiled prompt.
type Template struct {
	// Name is the template's identifier, used in provenance and in the cache
	// key.
	Name string

	// version is the content digest, short enough to read in a log line.
	version string

	tmpl *template.Template
}

// Version returns the content digest of this template.
//
// Derived from the file contents rather than declared by hand. A hand-written
// version number is a thing to forget, and forgetting it means cached
// translations outlive the prompt that produced them.
func (t *Template) Version() string { return t.version }

// Render fills the template.
func (t *Template) Render(data any) (string, error) {
	var out strings.Builder
	if err := t.tmpl.Execute(&out, data); err != nil {
		return "", fmt.Errorf("prompts: render %s: %w", t.Name, err)
	}
	return out.String(), nil
}

var (
	cacheMu sync.Mutex
	loaded  = map[string]*Template{}
)

// Load returns a template by its path within the prompts directory, without the
// extension — for example "translation/ja_zh_v1".
//
// Templates are compiled once and shared. Compilation is cheap but the parse
// error is the thing that matters, and surfacing it at startup rather than on
// the first translation is the difference between a build that fails and a job
// that fails.
func Load(name string) (*Template, error) {
	cacheMu.Lock()
	defer cacheMu.Unlock()

	if cached, ok := loaded[name]; ok {
		return cached, nil
	}

	file := path.Clean(name) + ".md"
	raw, err := files.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("prompts: no template named %q (have: %s)",
			name, strings.Join(available(), ", "))
	}

	tmpl, err := template.New(path.Base(file)).Parse(string(raw))
	if err != nil {
		return nil, fmt.Errorf("prompts: parse %s: %w", file, err)
	}

	sum := sha256.Sum256(raw)
	compiled := &Template{
		Name:    name,
		version: hex.EncodeToString(sum[:])[:12],
		tmpl:    tmpl,
	}
	loaded[name] = compiled
	return compiled, nil
}

// MustLoad returns a template, panicking if it is absent or malformed.
//
// For the handful of templates the pipeline cannot run without. A missing
// translation prompt is not a runtime condition to be handled: it means the
// binary was built wrong, and the embed directive should have caught it.
func MustLoad(name string) *Template {
	tmpl, err := Load(name)
	if err != nil {
		panic(err)
	}
	return tmpl
}

// available lists the embedded templates, for error messages.
func available() []string {
	var names []string
	err := walk("", &names)
	if err != nil {
		return nil
	}
	sort.Strings(names)
	return names
}

func walk(dir string, names *[]string) error {
	entries, err := files.ReadDir(orDot(dir))
	if err != nil {
		return err
	}
	for _, entry := range entries {
		child := path.Join(dir, entry.Name())
		if entry.IsDir() {
			if err := walk(child, names); err != nil {
				return err
			}
			continue
		}
		if strings.HasSuffix(child, ".md") {
			*names = append(*names, strings.TrimSuffix(child, ".md"))
		}
	}
	return nil
}

func orDot(dir string) string {
	if dir == "" {
		return "."
	}
	return dir
}
