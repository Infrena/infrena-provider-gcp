package disco

import (
	"os"
	"testing"
)

func load(t *testing.T, name string) *Document {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	d, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse(%s): %v", name, err)
	}
	return d
}

func TestOutputOnlyUnionsTheFlagAndTheProse(t *testing.T) {
	d := load(t, "tiny.json")
	w := d.Schemas["Widget"]
	cases := map[string]bool{
		"name":              false,
		"selfLink":          true, // prose only, no flag — compute's whole convention
		"creationTimestamp": true, // flag only, no prose
		"spec":              false,
	}
	for prop, want := range cases {
		if got := d.OutputOnly(w.Properties[prop]); got != want {
			t.Errorf("OutputOnly(%s) = %v, want %v", prop, got, want)
		}
	}

	// A $ref property can be marked readOnly at the referring site even though
	// the target schema (WidgetSpec) carries neither the flag nor the prose.
	// OutputOnly must see that only after Resolve inlines the ref and carries
	// the call site's readOnly onto the result — a resolver that drops that
	// inheritance would make this false.
	resolved, err := d.Resolve(w.Properties["activeConfig"])
	if err != nil {
		t.Fatalf("Resolve(activeConfig): %v", err)
	}
	if !d.OutputOnly(resolved) {
		t.Errorf("OutputOnly(resolved activeConfig) = false, want true (readOnly at the $ref site)")
	}
}

// TestBehaviorsReadsTheWholeLeadingRunAndNothingElse. The AIP convention puts
// field-behaviour tags at the start of a comment, stacked in any order --
// "Optional. Input only. Immutable. Tag keys bound to this resource." -- and
// compute writes its own in brackets. A prefix test sees only the first tag,
// and a search anywhere in the text reads declarations into ordinary prose.
func TestBehaviorsReadsTheWholeLeadingRunAndNothingElse(t *testing.T) {
	for desc, want := range map[string][]Behavior{
		"Optional. Input only. Immutable. Tag keys bound to this resource.":    {BehaviorOptional, BehaviorInputOnly, BehaviorImmutable},
		"Immutable. Required. The location where this cluster's nodes reside.": {BehaviorImmutable, BehaviorRequired},
		"[Output Only] Server-defined URL for the resource.":                   {BehaviorOutputOnly},
		"[Input Only] Specifies the parameters for a new disk.":                {BehaviorInputOnly},
		"Identifier. The resource name.":                                       {BehaviorIdentifier},
		// Declarations after ordinary prose are not declarations.
		"The deadline for changing this field. Immutable. After that, fixed.": nil,
		"The name of the file share.":                                         nil,
	} {
		got := Behaviors(&Schema{Description: desc})
		if len(got) != len(want) {
			t.Errorf("%q: got %v, want %v", desc, got, want)
			continue
		}
		for _, b := range want {
			if !got[b] {
				t.Errorf("%q: missing %q in %v", desc, b, got)
			}
		}
	}
}

// TestOutputOnlyReadsPastTheFirstTag. "Optional. Output only." is output-only;
// a prefix test on the description would call it settable.
func TestOutputOnlyReadsPastTheFirstTag(t *testing.T) {
	d := &Document{}
	if !d.OutputOnly(&Schema{Description: "Optional. Output only. When it was made."}) {
		t.Error("a second-position Output only. tag was not read")
	}
	if d.OutputOnly(&Schema{Description: "The time. Output only when the job ran."}) {
		t.Error("prose mentioning output-only was read as a declaration")
	}
}

// TestATagAfterADeprecationNoticeIsRead. compute's CustomerEncryptionKey
// sha256 puts "[Output only]" after a deprecation notice, and read only from
// the start it was settable on every disk, image and snapshot key. A
// deprecation notice with no tag behind it gives nothing.
func TestATagAfterADeprecationNoticeIsRead(t *testing.T) {
	out := Behaviors(&Schema{Description: "[DEPRECATED] CSEK is no longer supported. Use CMEK instead. [Output only] The RFC 4648 base64 encoded SHA-256 hash."})
	if !out[BehaviorOutputOnly] {
		t.Errorf("behaviours %v, want output only", out)
	}
	if got := Behaviors(&Schema{Description: "[DEPRECATED] Use foo instead. The size."}); len(got) != 0 {
		t.Errorf("a deprecation notice alone gave %v", got)
	}
}
