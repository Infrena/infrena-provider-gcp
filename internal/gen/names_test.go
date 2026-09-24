package gen

import (
	"path/filepath"
	"testing"
)

// TestAUniqueResourceSegmentGetsTheShortName, and a clash does not.
func TestNamesAreShortWhenUniqueAndQualifiedWhenNot(t *testing.T) {
	cands := []Candidate{
		{Service: "compute", Resource: "subnetwork"}, // unique
		{Service: "compute", Resource: "instance"},   // clashes with sql
		{Service: "sqladmin", Resource: "instance"},  // clashes with compute
		{Service: "redis", Resource: "instance"},     // and so does this
	}
	got, err := Assign(cands, NewLock())
	if err != nil {
		t.Fatal(err)
	}
	want := map[Candidate]string{
		{Service: "compute", Resource: "subnetwork"}: "gcp.subnetwork",
		{Service: "compute", Resource: "instance"}:   "gcp.compute.instance",
		{Service: "sqladmin", Resource: "instance"}:  "gcp.sqladmin.instance",
		{Service: "redis", Resource: "instance"}:     "gcp.redis.instance",
	}
	for c, w := range want {
		if got[c] != w {
			t.Errorf("%v -> %q, want %q", c, got[c], w)
		}
	}
}

// TestAnAssignedNameNeverMoves is the whole point of the lock. A short name already
// given to one type must stay with it even when a new GCP type would now make the
// resource segment ambiguous — the newcomer takes the qualified form.
// "widget", not "instance": instance is a generic word (genericWords) and is
// qualified from the start, which is a different rule from this one.
func TestAnAssignedNameNeverMoves(t *testing.T) {
	lock := NewLock()
	first := []Candidate{{Service: "compute", Resource: "widget"}}
	got, err := Assign(first, lock)
	if err != nil {
		t.Fatal(err)
	}
	if got[first[0]] != "gcp.widget" {
		t.Fatalf("first pass gave %q, want gcp.widget", got[first[0]])
	}

	// A new release of GCP adds sqladmin widgets. Without the lock, the naive rule
	// would now qualify BOTH and silently rename a released type.
	second := []Candidate{
		{Service: "compute", Resource: "widget"},
		{Service: "sqladmin", Resource: "widget"},
	}
	got2, err := Assign(second, lock)
	if err != nil {
		t.Fatal(err)
	}
	if got2[second[0]] != "gcp.widget" {
		t.Errorf("compute widget renamed to %q; a released name must never move", got2[second[0]])
	}
	if got2[second[1]] != "gcp.sqladmin.widget" {
		t.Errorf("newcomer got %q, want gcp.sqladmin.widget", got2[second[1]])
	}
}

// TestTheLockOnlyGrows. A type GCP removed keeps its entry, so its name can never be
// handed to something else later.
func TestTheLockOnlyGrows(t *testing.T) {
	lock := NewLock()
	if _, err := Assign([]Candidate{{Service: "gone", Resource: "widget"}}, lock); err != nil {
		t.Fatal(err)
	}
	before := len(lock.Names)
	if _, err := Assign(nil, lock); err != nil {
		t.Fatal(err)
	}
	if len(lock.Names) != before {
		t.Errorf("lock shrank from %d to %d", before, len(lock.Names))
	}
	if lock.Names["gone/widget"] != "gcp.widget" {
		t.Errorf("a withdrawn type lost its reservation: %v", lock.Names)
	}
}

func TestTheLockRoundTripsThroughDisk(t *testing.T) {
	lock := NewLock()
	if _, err := Assign([]Candidate{{Service: "compute", Resource: "subnetwork"}}, lock); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "names.lock.json")
	if err := lock.Save(path); err != nil {
		t.Fatal(err)
	}
	back, err := LoadLock(path)
	if err != nil {
		t.Fatal(err)
	}
	if back.Names["compute/subnetwork"] != "gcp.subnetwork" {
		t.Errorf("round trip lost the entry: %v", back.Names)
	}
}

// TestAnUnresolvableClashLeavesTheLockUnchanged. The lock is append-only and its
// entries are permanent, so a call that fails partway through a batch must not
// leave a partial assignment behind -- there is no way to undo it later short of
// hand-editing the file the rules say never to hand-edit.
//
// Forcing the error needs two candidates whose SHORT names are both already
// taken by someone else, and whose QUALIFIED names then collide with each
// other -- which only happens with a dot inside a service or resource
// segment. That never occurs in the real GCP corpus, so it is constructed
// synthetically here: "a.b"/"c" and "a"/"b.c" both qualify to "gcp.a.b.c".
func TestAnUnresolvableClashLeavesTheLockUnchanged(t *testing.T) {
	lock := NewLock()
	lock.Names["x/c"] = "gcp.c"     // takes the short name "a.b"/"c" would want
	lock.Names["y/b.c"] = "gcp.b.c" // takes the short name "a"/"b.c" would want
	before := make(map[string]string, len(lock.Names))
	for k, v := range lock.Names {
		before[k] = v
	}

	cands := []Candidate{
		{Service: "a.b", Resource: "c"}, // qualifies to gcp.a.b.c, sorts first
		{Service: "a", Resource: "b.c"}, // also qualifies to gcp.a.b.c -- clash
	}
	if _, err := Assign(cands, lock); err == nil {
		t.Fatal("expected an unresolvable clash error, got nil")
	}

	if len(lock.Names) != len(before) {
		t.Fatalf("lock mutated on error: had %d entries, now has %d: %v", len(before), len(lock.Names), lock.Names)
	}
	for k, v := range before {
		if lock.Names[k] != v {
			t.Errorf("lock entry %s changed from %q to %q", k, v, lock.Names[k])
		}
	}
	if _, ok := lock.Names["a.b/c"]; ok {
		t.Errorf("the candidate resolved before the failing one leaked into the lock: %v", lock.Names)
	}
}

// TestAGenericWordIsQualifiedEvenUncontested. Left to the short-unless-
// contested rule, a Memcache instance became gcp.instance for good. A word
// on the list is always qualified, an ordinary word keeps its short name, and
// a generic name already in the lock does not move.
func TestAGenericWordIsQualifiedEvenUncontested(t *testing.T) {
	lock := NewLock()
	lock.Names["pubsub/topic"] = "gcp.topic"
	lock.Names["eventarc/channel"] = "gcp.channel"
	cands := []Candidate{
		{Service: "memcache", Resource: "instance"},
		{Service: "dns", Resource: "responsepolicy"},
		{Service: "eventarc", Resource: "channel"},
	}
	got, err := Assign(cands, lock)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		cand Candidate
		want string
	}{
		{cands[0], "gcp.memcache.instance"},
		{cands[1], "gcp.responsepolicy"},
		{cands[2], "gcp.channel"},
	} {
		if got[c.cand] != c.want {
			t.Errorf("%s/%s named %q, want %q", c.cand.Service, c.cand.Resource, got[c.cand], c.want)
		}
	}
}
