package strategy

import (
	"fmt"
	"log/slog"
	"sort"
	"sync"
)

// ParamKind is the type of a strategy parameter, used to render and validate
// the right form control in the web UI.
type ParamKind string

const (
	KindString ParamKind = "string"
	KindInt    ParamKind = "int"
	KindFloat  ParamKind = "float"
	KindBool   ParamKind = "bool"
	KindEnum   ParamKind = "enum"
	KindTime   ParamKind = "time" // "15:15", an IST clock time
)

// ParamSpec describes one strategy parameter completely enough that the web UI
// can render a form field for it, and the registry can validate what comes back.
//
// Declaring parameters as data rather than parsing them ad hoc inside Init means
// adding a strategy never requires touching the UI, and every strategy gets the
// same validation.
type ParamSpec struct {
	Key         string    `json:"key"`   // matches the key in config.StrategyCfg.Params
	Label       string    `json:"label"` // human-facing field name
	Kind        ParamKind `json:"kind"`
	Default     any       `json:"default"`
	Min         *float64  `json:"min,omitempty"`
	Max         *float64  `json:"max,omitempty"`
	Options     []string  `json:"options,omitempty"` // for KindEnum
	Description string    `json:"description,omitempty"`
	Group       string    `json:"group,omitempty"` // form section heading
}

// Factory builds a fresh strategy instance. instanceID becomes Strategy.Name(),
// and therefore the StrategyID on every order, fill, and position it produces.
type Factory func(instanceID string, logger *slog.Logger) (Strategy, error)

// Descriptor is everything the platform knows about a strategy type.
type Descriptor struct {
	Type    string      `json:"type"` // registry key, e.g. "short-straddle"
	Title   string      `json:"title"`
	Summary string      `json:"summary"`
	Params  []ParamSpec `json:"params"`
	Factory Factory     `json:"-"`
}

// Registry maps strategy type names to their descriptors.
type Registry struct {
	mu sync.RWMutex
	m  map[string]Descriptor
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{m: make(map[string]Descriptor)}
}

// Default is the process-wide registry that strategy packages register into
// from their init functions.
var Default = NewRegistry()

// Register adds a descriptor to the default registry.
func Register(d Descriptor) { Default.Register(d) }

// Register adds a descriptor.
//
// It panics on a duplicate or malformed descriptor because both are programmer
// errors detectable at startup, and a strategy that silently failed to register
// would look to the operator like it had simply vanished from the UI.
func (r *Registry) Register(d Descriptor) {
	if d.Type == "" {
		panic("strategy: descriptor with no Type")
	}
	if d.Factory == nil {
		panic("strategy: descriptor " + d.Type + " has no Factory")
	}
	seen := make(map[string]struct{}, len(d.Params))
	for _, p := range d.Params {
		if p.Key == "" {
			panic("strategy: " + d.Type + " has a parameter with no Key")
		}
		if _, dup := seen[p.Key]; dup {
			panic("strategy: " + d.Type + " declares parameter " + p.Key + " twice")
		}
		seen[p.Key] = struct{}{}
		if p.Kind == KindEnum && len(p.Options) == 0 {
			panic("strategy: " + d.Type + " parameter " + p.Key + " is an enum with no Options")
		}
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.m[d.Type]; exists {
		panic("strategy: duplicate registration for " + d.Type)
	}
	r.m[d.Type] = d
}

// Get returns a descriptor by type name.
func (r *Registry) Get(typ string) (Descriptor, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	d, ok := r.m[typ]
	return d, ok
}

// List returns every registered descriptor, sorted by type for a stable UI.
func (r *Registry) List() []Descriptor {
	r.mu.RLock()
	out := make([]Descriptor, 0, len(r.m))
	for _, d := range r.m {
		out = append(out, d)
	}
	r.mu.RUnlock()

	sort.Slice(out, func(i, j int) bool { return out[i].Type < out[j].Type })
	return out
}

// New builds an instance of a registered strategy.
func (r *Registry) New(typ, instanceID string, logger *slog.Logger) (Strategy, Descriptor, error) {
	d, ok := r.Get(typ)
	if !ok {
		return nil, Descriptor{}, fmt.Errorf("unknown strategy type %q", typ)
	}
	s, err := d.Factory(instanceID, logger)
	if err != nil {
		return nil, d, fmt.Errorf("build %s: %w", typ, err)
	}
	return s, d, nil
}
