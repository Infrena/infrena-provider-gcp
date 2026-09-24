package gcprov

import (
	"context"
	"fmt"
	"os"

	"github.com/infrena/infrena-provider-gcp/internal/catalog"
	"github.com/infrena/infrena/pkg/resource"
	"github.com/infrena/infrena/pkg/value"
)

// attrByCanonical finds an attribute by its wire name, returning the schema
// name state is keyed by.
func attrByCanonical(ty *catalog.Type, canonical string) (string, *catalog.Attr) {
	for name, a := range ty.Attributes {
		if a.Canonical == canonical {
			return name, a
		}
	}
	return "", nil
}

// changedSetters are the setters with at least one field that desired sets
// and current does not hold. A field absent from desired is not a change,
// for the same never-removes reason as BuildMask.
func changedSetters(ty *catalog.Type, current, desired map[string]value.Value) []*catalog.Setter {
	var out []*catalog.Setter
	for i := range ty.Setters {
		s := &ty.Setters[i]
		for _, f := range s.Fields {
			name, a := attrByCanonical(ty, f)
			if a == nil {
				continue
			}
			want, ok := desired[name]
			if ok && want.Known && !current[name].Equal(want) {
				out = append(out, s)
				break
			}
		}
	}
	return out
}

// setterBody is the request a setter sends: every field it carries, from
// configuration where configuration says, otherwise the value the resource
// already has, because the method sets all of them at once. The lock comes
// from current, never from configuration.
func setterBody(ty *catalog.Type, s *catalog.Setter, current, desired map[string]value.Value) map[string]any {
	body := map[string]any{}
	for _, f := range s.Fields {
		name, a := attrByCanonical(ty, f)
		if a == nil {
			continue
		}
		v, ok := desired[name]
		if !ok || !v.Known {
			v, ok = current[name]
		}
		if ok && v.Known && v.Kind != value.KindInvalid {
			body[f] = wireValue(a, v)
		}
	}
	if s.Lock != "" {
		if name, a := attrByCanonical(ty, s.Lock); a != nil {
			if v, ok := current[name]; ok && v.Known && v.Kind != value.KindInvalid {
				body[s.Lock] = wireValue(a, v)
			}
		}
	}
	return body
}

// callSetter sends one setter and waits for it. sent reports whether the
// request reached Google, which decides whether a failure may still be
// returned as "nothing happened".
func (p *Provider) callSetter(ctx context.Context, ty *catalog.Type, s *catalog.Setter, id string, current, desired map[string]value.Value) (sent bool, err error) {
	idAttrs, err := ParseProviderID(ty, id)
	if err != nil {
		return false, err
	}
	reqURL, err := p.itemURL(ty, s.Path, id, idAttrs)
	if err != nil {
		return false, err
	}
	resp, err := p.client.Do(ctx, s.Verb, reqURL, setterBody(ty, s, current, desired))
	if err != nil {
		return false, err
	}
	_, err = p.await(ctx, ty, resp)
	return true, err
}

// setAfterCreate calls the setters for fields the create did not apply. An
// insert ignores what only a setter changes -- a backend service's
// securityPolicy, a compute address's labels -- so without this the first
// plan after every such create proposes the change again.
//
// The resource exists, so nothing here returns an error: a failure is
// reported and the state as it stands is returned, and the next plan
// proposes the change as an ordinary update.
func (p *Provider) setAfterCreate(ctx context.Context, ty *catalog.Type, st *resource.ResourceState, desired map[string]value.Value) *resource.ResourceState {
	due := changedSetters(ty, st.Attributes, desired)
	if len(due) == 0 || ctx.Err() != nil {
		return st
	}
	for _, s := range due {
		if _, err := p.callSetter(ctx, ty, s, st.ProviderID, st.Attributes, desired); err != nil {
			fmt.Fprintf(os.Stderr, "gcp: %s: created, but %s failed; the next plan will propose it again: %v\n",
				ty.Name, s.Method, err)
			break
		}
	}
	if fresh, err := p.readAfterPatch(ctx, ty, st, desired); err == nil && fresh != nil {
		return fresh
	}
	return st
}
