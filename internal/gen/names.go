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
}

func (c Candidate) key() string { return c.Service + "/" + c.Resource }

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
		return nil, err
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
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
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
	wants := map[string]int{}
	for _, c := range fresh {
		wants[c.Resource]++
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

// nameFor picks the short or qualified name for c, given how many OTHER fresh
// candidates in this call want its resource segment, and which names are
// already spoken for (by the lock or by a fresh candidate resolved earlier in
// this same call).
func nameFor(c Candidate, wants map[string]int, taken map[string]string) (string, error) {
	short := "gcp." + c.Resource
	qualified := "gcp." + c.Service + "." + c.Resource
	name := short
	if wants[c.Resource] > 1 {
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
