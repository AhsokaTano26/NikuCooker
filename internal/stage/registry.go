package stage

import (
	"fmt"
	"sort"
	"strings"
)

// Registry holds the available stages in pipeline order.
//
// Registration order *is* pipeline order. A separate ordering table would be a
// second place to keep in step with the dependency graph, and the two would
// eventually disagree — silently, because both would still typecheck.
type Registry struct {
	byName map[string]Stage
	order  []Stage
}

// NewRegistry builds a registry and validates the pipeline it describes.
//
// Validation happens at construction rather than at run time because every
// problem it catches — a missing dependency, a duplicate name, a cycle — is a
// wiring bug that would otherwise surface as a failed job in front of a user.
func NewRegistry(stages ...Stage) (*Registry, error) {
	r := &Registry{byName: make(map[string]Stage, len(stages))}

	for _, s := range stages {
		spec := s.Spec()
		if spec.Name == "" {
			return nil, fmt.Errorf("stage: a registered stage has an empty name")
		}
		if spec.Version == "" {
			return nil, fmt.Errorf("stage %q: has no version; it participates in the artifact cache key", spec.Name)
		}
		if _, dup := r.byName[spec.Name]; dup {
			return nil, fmt.Errorf("stage %q: registered twice", spec.Name)
		}
		r.byName[spec.Name] = s
		r.order = append(r.order, s)
	}

	if err := r.validate(); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *Registry) validate() error {
	// Every declared dependency must exist...
	for _, s := range r.order {
		spec := s.Spec()

		hard := make(map[string]bool, len(spec.Depends))
		for _, dep := range spec.Depends {
			hard[dep] = true
		}

		for _, dep := range spec.Depends {
			if _, ok := r.byName[dep]; !ok {
				return fmt.Errorf("stage %q: depends on %q, which is not registered", spec.Name, dep)
			}
		}

		// An optional dependency is deliberately *not* required to be present.
		// Its absence is the configuration it exists to support: a registry
		// without the analysis pass is how a user disables it, and rejecting
		// that would make the whole declaration pointless.
		for _, dep := range spec.OptionalDepends {
			if dep == spec.Name {
				return fmt.Errorf("stage %q: depends on itself", spec.Name)
			}
		}

		// Listing a name in both is not an error the executor would notice,
		// because the hard list is what it checks — which is exactly why it is
		// worth rejecting here. The declaration would read as optional and
		// behave as required.
		for _, dep := range spec.OptionalDepends {
			if hard[dep] {
				return fmt.Errorf(
					"stage %q lists %q as both a required and an optional dependency", spec.Name, dep)
			}
		}
	}

	// ...and a dependency must come earlier in the order, which is what makes
	// the pipeline a chain the executor can walk in one pass. A dependency
	// declared after its consumer would mean the executor runs the consumer
	// with a missing input.
	//
	// Optional dependencies are held to the same ordering rule. They may not
	// arrive, but when they do they must arrive first, and the executor has no
	// second pass in which to go back for them.
	seen := map[string]bool{}
	for _, s := range r.order {
		spec := s.Spec()
		for _, dep := range append(append([]string(nil), spec.Depends...), spec.OptionalDepends...) {
			// An optional dependency that is not registered is not a problem
			// here either; there is no ordering to get wrong.
			if _, registered := r.byName[dep]; !registered {
				continue
			}
			if !seen[dep] {
				return fmt.Errorf(
					"stage %q depends on %q, which is registered later; the dependencies of a stage must come before it",
					spec.Name, dep)
			}
		}
		seen[spec.Name] = true
	}

	// A cycle is impossible given the ordering rule above, but the check is
	// cheap and the failure it prevents is an infinite loop.
	if _, err := r.topoSort(); err != nil {
		return err
	}
	return nil
}

func (r *Registry) topoSort() ([]string, error) {
	const (
		unvisited = 0
		visiting  = 1
		done      = 2
	)
	state := make(map[string]int, len(r.order))
	var order []string

	var visit func(name string, path []string) error
	visit = func(name string, path []string) error {
		switch state[name] {
		case done:
			return nil
		case visiting:
			return fmt.Errorf("stage dependency cycle: %s → %s", strings.Join(path, " → "), name)
		}
		state[name] = visiting

		s, ok := r.byName[name]
		if !ok {
			return fmt.Errorf("stage %q is referenced but not registered", name)
		}
		for _, dep := range s.Spec().Depends {
			if err := visit(dep, append(path, name)); err != nil {
				return err
			}
		}

		state[name] = done
		order = append(order, name)
		return nil
	}

	for _, s := range r.order {
		if err := visit(s.Spec().Name, nil); err != nil {
			return nil, err
		}
	}
	return order, nil
}

// Get returns a stage by name.
func (r *Registry) Get(name string) (Stage, bool) {
	s, ok := r.byName[name]
	return s, ok
}

// Ordered returns the stages in pipeline order.
func (r *Registry) Ordered() []Stage {
	out := make([]Stage, len(r.order))
	copy(out, r.order)
	return out
}

// Names returns the stage names in pipeline order.
func (r *Registry) Names() []string {
	names := make([]string, 0, len(r.order))
	for _, s := range r.order {
		names = append(names, s.Spec().Name)
	}
	return names
}

// OptionalNames returns the names of stages that can be disabled, sorted.
//
// Sorted rather than in pipeline order because its callers build error messages
// and configuration listings, where a stable alphabetical order is easier to
// scan than a dependency order.
func (r *Registry) OptionalNames() []string {
	var names []string
	for _, s := range r.order {
		if s.Spec().Optional {
			names = append(names, s.Spec().Name)
		}
	}
	sort.Strings(names)
	return names
}
