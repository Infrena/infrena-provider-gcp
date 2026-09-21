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
func TestAnAssignedNameNeverMoves(t *testing.T) {
	lock := NewLock()
	first := []Candidate{{Service: "compute", Resource: "instance"}}
	got, err := Assign(first, lock)
	if err != nil {
		t.Fatal(err)
	}
	if got[first[0]] != "gcp.instance" {
		t.Fatalf("first pass gave %q, want gcp.instance", got[first[0]])
	}

	// A new release of GCP adds sqladmin instances. Without the lock, the naive rule
	// would now qualify BOTH and silently rename a released type.
	second := []Candidate{
		{Service: "compute", Resource: "instance"},
		{Service: "sqladmin", Resource: "instance"},
	}
	got2, err := Assign(second, lock)
	if err != nil {
		t.Fatal(err)
	}
	if got2[second[0]] != "gcp.instance" {
		t.Errorf("compute instance renamed to %q; a released name must never move", got2[second[0]])
	}
	if got2[second[1]] != "gcp.sqladmin.instance" {
		t.Errorf("newcomer got %q, want gcp.sqladmin.instance", got2[second[1]])
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
