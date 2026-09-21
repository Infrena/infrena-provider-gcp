package gen

import (
	"fmt"
	"slices"
	"strings"

	"github.com/infrena/infrena-provider-gcp/internal/disco"
	"github.com/infrena/infrena-provider-gcp/internal/mmv1"
)

// Tier is how far the generator can vouch for a type (spec §4.1).
type Tier int

const (
	// TierGeneric: the generator can vouch for it. Ships.
	TierGeneric Tier = 1
	// TierHooked: magic-modules carries hand-written Go that changes the wire
	// shape. Ships only with a ruling that names every hook.
	TierHooked Tier = 2
	// TierExcluded: not representable. Named in gen/warnings.txt with a reason.
	TierExcluded Tier = 3
)

// Decision is a tier and why.
type Decision struct {
	Tier   Tier
	Reason string
}

// Classify decides one type's tier.
//
// Exclusions are checked before hooks: a resource that cannot be created is out
// whatever its custom_code says, and reporting it as "hooked" would put it on a
// backlog of rulings that could never help it.
func Classify(col disco.Collection, mm *mmv1.Resource, ruling *Ruling) Decision {
	if mm != nil {
		if mm.Exclude {
			return Decision{TierExcluded, "magic-modules marks it exclude: true"}
		}
		if mm.MinVersion != "" && mm.MinVersion != "ga" {
			return Decision{TierExcluded, "min_version is " + mm.MinVersion + ", not ga"}
		}
	}

	hasCreate := col.Methods["insert"] != nil || col.Methods["create"] != nil
	hasGet := col.Methods["get"] != nil
	hasDelete := col.Methods["delete"] != nil
	switch {
	case !hasCreate:
		return Decision{TierExcluded, "no insert or create method"}
	case !hasDelete:
		return Decision{TierExcluded, "no delete method"}
	case !hasGet:
		// Readable only by listing its parent. Representable, but not generically,
		// so it needs a ruling that says how — see the tagBindings ruling.
		if ruling == nil || ruling.ReadVia == "" {
			return Decision{TierHooked, "no get method; needs a ruling with read_via"}
		}
	}

	var hooks []string
	if mm != nil {
		hooks = mm.WireHooks()
	}
	if len(hooks) == 0 {
		return Decision{TierGeneric, "generic-safe"}
	}
	if ruling == nil {
		return Decision{TierHooked, "unruled wire hooks: " + strings.Join(hooks, ", ")}
	}
	var unruled []string
	for _, h := range hooks {
		if !slices.Contains(ruling.Hooks, h) {
			unruled = append(unruled, h)
		}
	}
	if len(unruled) > 0 {
		// The ruling predates these hooks. Refusing is the point: this is the exact
		// path by which a vendor bump would otherwise ship something unreviewed.
		return Decision{TierHooked, fmt.Sprintf(
			"ruling covers %s but the resource now also declares %s; re-inspect and extend the ruling",
			strings.Join(ruling.Hooks, ", "), strings.Join(unruled, ", "))}
	}
	return Decision{TierGeneric, "ruled: " + ruling.Note}
}
