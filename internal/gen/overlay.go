package gen

import (
	"fmt"
	"os"
	"path/filepath"

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

	// ProductAliases maps a Discovery API name (e.g. "cloudresourcemanager") to
	// every vendored magic-modules product DIRECTORY that also carries
	// resources for it. This is needed because mmv1's own product directory
	// names routinely diverge from Google's Discovery API names: TagBinding
	// lives under gen/mmv1/products/tags, not cloudresourcemanager; CryptoKey
	// lives under kms, not cloudkms. Without this, matchResource cannot find
	// those resources at all, mm comes back nil, and any ruling keyed to them
	// (e.g. G6's cloudresourcemanager/TagBinding) is silently never consulted
	// — the exact bug that motivated this field.
	//
	// It is a human-maintained crosswalk, not something derivable from the
	// vendored tree: mmv1 product directories carry no field that reliably
	// names the Discovery API they correspond to (a few have `legacy_name`,
	// most do not, and it disagrees with the Discovery name as often as it
	// agrees), so a mapping only a person can attest to belongs here beside
	// the rulings, not guessed at in Go.
	ProductAliases map[string][]string `yaml:"product_aliases"`
}

// LoadOverlay reads and validates the overlay. mmv1Dir is the vendored
// products directory (e.g. gen/mmv1/products): every directory named in
// ProductAliases must actually exist under it, or a typo here would silently
// reintroduce the exact bug this field exists to fix, just spelled
// differently and just as invisibly.
func LoadOverlay(path, mmv1Dir string) (*Overlay, error) {
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
	for api, dirs := range o.ProductAliases {
		for _, dir := range dirs {
			info, statErr := os.Stat(filepath.Join(mmv1Dir, dir))
			if statErr != nil || !info.IsDir() {
				return nil, fmt.Errorf("%s: product_aliases[%q] names directory %q, which does not exist under %s",
					path, api, dir, mmv1Dir)
			}
		}
	}
	return &o, nil
}
