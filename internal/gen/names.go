package gen

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
)

// Candidate is one type before it has a name.
type Candidate struct {
	Service  string
	Resource string
	// Scope distinguishes a collection reached through a non-project resource
	// hierarchy root (organization, folder, billing account) from the
	// project-scoped collection that shares its resource segment after
	// singularizing it. These are genuinely different resources -- different
	// base URLs, different IAM, and Terraform models them separately too --
	// but they collapse to one Candidate without this: the four Discovery
	// collections behind an org's, a folder's, a billing account's and a
	// project's own logging buckets all singularize to "bucket". Empty for
	// the project-scoped collection and for anything not reached through a
	// recognised root, both of which keep naming exactly as it was before
	// this field existed.
	Scope string
	// Qualified marks a Resource segment that was extended by walking its
	// Discovery path leftward to disambiguate it from an unrelated candidate
	// that shared its original bare leaf (see resolveWithinServiceCollisions
	// in build.go — iam's ServiceAccountKey and WorkloadIdentityPoolProviderKey
	// both start out as "key"). Its Resource is already made unique by that
	// walk, but it still always takes the service-qualified form: a name this
	// specific (workloadidentitypool.provider.key) is not what "the short
	// name" is FOR, even on the rare occasion nothing else happens to want it.
	Qualified bool
}

func (c Candidate) key() string {
	if c.Scope == "" {
		return c.Service + "/" + c.Resource
	}
	return c.Service + "/" + c.Scope + "/" + c.Resource
}

// Lock records every name ever assigned. It ONLY GROWS: an entry is never
// removed and never changed, so a name released to users cannot later move to a
// different type. A type GCP withdraws keeps its reservation for the same
// reason.
type Lock struct {
	Names map[string]string `json:"names"`
}

// NewLock returns an empty lock.
func NewLock() *Lock { return &Lock{Names: map[string]string{}} }

// LoadLock reads the lock, treating absence as empty so a first run works.
func LoadLock(path string) (*Lock, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return NewLock(), nil
	}
	if err != nil {
		// Named, because an unreadable lock and an absent one are treated
		// completely differently a line apart, and a bare "permission denied"
		// with no path would not say which file was in question.
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	l := NewLock()
	if err := json.Unmarshal(data, l); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if l.Names == nil {
		l.Names = map[string]string{}
	}
	return l, nil
}

// Save writes the lock, sorted, so a diff is readable.
func (l *Lock) Save(path string) error {
	data, err := json.MarshalIndent(l, "", " ")
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

// Assign names every candidate, honouring and extending the lock.
//
// Two passes, and the order matters. The first hands every candidate the lock
// already knows its recorded name, whatever the current corpus looks like. Only
// then does the second pass decide the newcomers, treating a short name the lock
// has already given away as taken. That is what stops a new GCP type from
// renaming a released one.
func Assign(cands []Candidate, lock *Lock) (map[Candidate]string, error) {
	out := make(map[Candidate]string, len(cands))
	taken := map[string]string{} // infrena name -> candidate key that holds it
	for k, n := range lock.Names {
		taken[n] = k
	}

	var fresh []Candidate
	for _, c := range cands {
		if n, ok := lock.Names[c.key()]; ok {
			out[c] = n
			continue
		}
		fresh = append(fresh, c)
	}

	// Deterministic: sort, so two runs over the same corpus assign the same names.
	sort.Slice(fresh, func(i, j int) bool { return fresh[i].key() < fresh[j].key() })

	// How many NEW candidates want each resource segment. A segment wanted by more
	// than one newcomer is ambiguous even if the lock has never seen it.
	//
	// Only unscoped, unqualified candidates compete for the bare resource
	// segment: a scoped or already-qualified candidate never receives a short
	// name (nameFor always qualifies it), so counting it here would wrongly
	// push its unscoped sibling into a qualified name it has no need for.
	wants := map[string]int{}
	for _, c := range fresh {
		if c.Scope == "" && !c.Qualified {
			wants[c.Resource]++
		}
	}

	// Held until every fresh candidate resolves cleanly. The lock is append-only
	// and permanent once written, so a call that fails partway through must
	// leave it exactly as it found it -- nothing commits until the whole batch
	// succeeds.
	assigned := make(map[string]string, len(fresh)) // candidate key -> name

	for _, c := range fresh {
		name, err := nameFor(c, wants, taken)
		if err != nil {
			return nil, err
		}
		out[c] = name
		taken[name] = c.key()
		assigned[c.key()] = name
	}

	for k, n := range assigned {
		lock.Names[k] = n
	}
	return out, nil
}

// nameFor picks the name for c, given how many OTHER fresh candidates in this
// call want its resource segment, and which names are already spoken for (by
// the lock or by a fresh candidate resolved earlier in this same call).
//
// A scoped candidate (organization, folder or billing account) always gets
// the fully qualified gcp.<service>.<scope>.<resource> form -- there is no
// short form to consider, because the whole point of Scope is that this
// candidate is NOT what a user means by the bare resource name. Likewise a
// Qualified candidate (its Resource already walked to disambiguate it from
// an unrelated one) always gets gcp.<service>.<resource>, never the short
// form. An ordinary candidate keeps the original short-unless-contested rule
// unchanged.
func nameFor(c Candidate, wants map[string]int, taken map[string]string) (string, error) {
	if c.Scope != "" {
		name := "gcp." + c.Service + "." + c.Scope + "." + c.Resource
		if holder, clash := taken[name]; clash && holder != c.key() {
			return "", fmt.Errorf("cannot name %s: %s is already taken (by %s)", c.key(), name, holder)
		}
		return name, nil
	}
	if c.Qualified {
		name := "gcp." + c.Service + "." + c.Resource
		if holder, clash := taken[name]; clash && holder != c.key() {
			return "", fmt.Errorf("cannot name %s: %s is already taken (by %s)", c.key(), name, holder)
		}
		return name, nil
	}
	short := "gcp." + c.Resource
	qualified := "gcp." + c.Service + "." + c.Resource
	name := short
	if wants[c.Resource] > 1 || genericWords[c.Resource] {
		name = qualified
	}
	if holder, clash := taken[name]; clash && holder != c.key() {
		name = qualified
	}
	if holder, clash := taken[name]; clash && holder != c.key() {
		return "", fmt.Errorf("cannot name %s: both %s and %s are taken (by %s)",
			c.key(), short, qualified, holder)
	}
	return name, nil
}

// genericWords are resource names too general for one service to own the
// short name: left to the short-unless-contested rule, a Memcache instance
// became gcp.instance, a DNS policy gcp.policy and a monitoring group
// gcp.group, permanently, because the lock only grows. A fresh candidate
// with one of these names always takes gcp.<service>.<resource> (James,
// 2026-09-24). Names already in the lock are returned before this rule runs,
// so nothing published moves.
var genericWords = map[string]bool{
	"instance": true, "policy": true, "group": true, "metric": true,
	"schema": true, "queue": true, "job": true, "rule": true,
	"config": true, "key": true, "service": true, "cluster": true,
	"database": true, "table": true, "template": true, "version": true,
	"connection": true, "endpoint": true, "gateway": true, "repository": true,
	"resource": true, "operation": true, "channel": true, "trigger": true,
	// Added 2026-09-25, before AlloyDB's backup and VPC Access's connector
	// took gcp.backup and gcp.connector.
	"backup": true, "connector": true, "snapshot": true, "index": true, "user": true,
}
