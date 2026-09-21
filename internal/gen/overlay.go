package gen

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Ruling is a human decision about one hooked type.
type Ruling struct {
	// Hooks must name EVERY wire hook the resource declares. A partial list is
	// treated as stale, not as partial approval.
	Hooks []string `yaml:"hooks"`
	// Note says what was inspected and why it is safe. Required: a ruling with no
	// note is a rubber stamp nobody can review.
	Note string `yaml:"note"`
	// ReadVia names how to read a type with no get method, e.g. "list_by_parent".
	ReadVia string `yaml:"read_via"`
	// AllForceNew marks a type with no update path at all.
	AllForceNew bool `yaml:"all_force_new"`
}

// Overlay is gen/overlay.yaml: everything a human decided.
type Overlay struct {
	// Rulings are keyed by "<product>/<Resource>", matching the vendored path.
	Rulings map[string]*Ruling `yaml:"rulings"`
	// Aliases are friendly names, keyed by infrena type then canonical attribute.
	Aliases map[string]map[string]string `yaml:"aliases"`
	// DiscoverDefault is the type list `discover` scans when the instance names none.
	DiscoverDefault []string `yaml:"discover_default"`
}

// LoadOverlay reads and validates the overlay.
func LoadOverlay(path string) (*Overlay, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var o Overlay
	if err := yaml.Unmarshal(data, &o); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	for key, r := range o.Rulings {
		if r.Note == "" {
			return nil, fmt.Errorf("%s: ruling %q has no note; a ruling with no reasoning is a rubber stamp", path, key)
		}
		if len(r.Hooks) == 0 && r.ReadVia == "" {
			return nil, fmt.Errorf("%s: ruling %q names no hooks and no read_via, so it rules on nothing", path, key)
		}
	}
	return &o, nil
}
