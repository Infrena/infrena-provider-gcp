package gcprov

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/infrena/infrena/pkg/value"
)

// ExpandURL substitutes placeholders in tmpl with attrs, the way the
// catalog's base_url/create_url/update_url/delete_url/self_link templates are
// meant to be read (spec §5.2).
//
// Two placeholder spellings appear in the generated catalog and both must be
// handled: magic-modules writes "{{project}}" (double brace); some paths come
// from a service's Discovery document instead, which writes "{project}"
// (single brace) or, for a value meant to carry its own "/" unescaped (a
// precomputed hierarchical parent like "projects/p/locations/l"),
// "{+parent}" -- RFC 6570's reserved-expansion form. Handling only the
// double-brace form leaves every type whose url came from a Discovery
// document unable to expand at all (verified against the embedded catalog:
// 122 single-brace occurrences across 233 types, the majority of them
// "{+parent}").
//
// A placeholder with no matching, known attribute is an error naming the
// attribute, never a silently empty segment: expanding {{zone}} to ""
// produces "projects/p/zones//instances/web1", which GCP answers with a
// confusing 404 instead of a message naming what nobody set.
func ExpandURL(tmpl string, attrs map[string]value.Value) (string, error) {
	var b strings.Builder
	i := 0
	for i < len(tmpl) {
		if tmpl[i] != '{' {
			b.WriteByte(tmpl[i])
			i++
			continue
		}

		double := i+1 < len(tmpl) && tmpl[i+1] == '{'
		openLen := 1
		closeSeq := "}"
		if double {
			openLen = 2
			closeSeq = "}}"
		}
		contentStart := i + openLen

		rel := strings.Index(tmpl[contentStart:], closeSeq)
		if rel == -1 {
			return "", fmt.Errorf("gcprov: url template %q has an unterminated %s placeholder",
				tmpl, tmpl[i:contentStart])
		}
		content := strings.TrimSpace(tmpl[contentStart : contentStart+rel])

		// RFC 6570 reserved expansion, "{+name}": only meaningful single-brace
		// (Discovery never doubles a "+" placeholder in this catalog), and the
		// substituted value is used exactly as attrs holds it, not escaped --
		// it is already a valid multi-segment path (e.g. "projects/p/locations/l").
		reserved := !double && strings.HasPrefix(content, "+")
		name := strings.TrimPrefix(content, "+")

		v, ok := attrs[name]
		if !ok || !v.Known {
			return "", fmt.Errorf("gcprov: url template %q needs %q, which is not set", tmpl, name)
		}
		raw, err := rawSegment(v)
		if err != nil {
			return "", fmt.Errorf("gcprov: url template %q: %q: %w", tmpl, name, err)
		}
		if reserved {
			b.WriteString(raw)
		} else {
			b.WriteString(url.PathEscape(raw))
		}

		i = contentStart + rel + len(closeSeq)
	}
	return b.String(), nil
}

// rawSegment renders one resolved attribute as the literal text a
// placeholder expands to, unescaped -- callers escape it themselves except
// for a reserved-expansion placeholder, which must not be.
func rawSegment(v value.Value) (string, error) {
	switch v.Kind {
	case value.KindString:
		s, _ := v.Raw.(string)
		return s, nil
	case value.KindInt:
		i, _ := v.Raw.(int64)
		return strconv.FormatInt(i, 10), nil
	case value.KindFloat:
		f, _ := v.Raw.(float64)
		return strconv.FormatFloat(f, 'f', -1, 64), nil
	case value.KindBool:
		bv, _ := v.Raw.(bool)
		return strconv.FormatBool(bv), nil
	default:
		return "", fmt.Errorf("cannot appear in a url (kind %s)", v.Kind)
	}
}
