package gcprov

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/infrena/infrena/pkg/value"
)

// ExpandURL substitutes {{placeholder}} markers in tmpl with attrs, the way
// the catalog's base_url/create_url/update_url/delete_url/self_link
// templates are meant to be read (spec §5.2).
//
// A placeholder with no matching, known attribute is an error naming the
// attribute, never a silently empty segment: expanding {{zone}} to ""
// produces "projects/p/zones//instances/web1", which GCP answers with a
// confusing 404 instead of a message naming what nobody set.
func ExpandURL(tmpl string, attrs map[string]value.Value) (string, error) {
	var b strings.Builder
	rest := tmpl
	for {
		start := strings.Index(rest, "{{")
		if start == -1 {
			b.WriteString(rest)
			break
		}
		b.WriteString(rest[:start])

		end := strings.Index(rest[start:], "}}")
		if end == -1 {
			return "", fmt.Errorf("gcprov: url template %q has an unterminated {{ placeholder", tmpl)
		}
		end += start

		name := strings.TrimSpace(rest[start+2 : end])
		v, ok := attrs[name]
		if !ok || !v.Known {
			return "", fmt.Errorf("gcprov: url template %q needs %q, which is not set", tmpl, name)
		}
		seg, err := urlSegment(v)
		if err != nil {
			return "", fmt.Errorf("gcprov: url template %q: %q: %w", tmpl, name, err)
		}
		b.WriteString(seg)

		rest = rest[end+2:]
	}
	return b.String(), nil
}

// urlSegment renders one resolved attribute as the literal text a
// placeholder expands to, escaped so a value containing "/" or another
// reserved character cannot be mistaken for an extra path segment.
func urlSegment(v value.Value) (string, error) {
	var raw string
	switch v.Kind {
	case value.KindString:
		raw, _ = v.Raw.(string)
	case value.KindInt:
		i, _ := v.Raw.(int64)
		raw = strconv.FormatInt(i, 10)
	case value.KindFloat:
		f, _ := v.Raw.(float64)
		raw = strconv.FormatFloat(f, 'f', -1, 64)
	case value.KindBool:
		bv, _ := v.Raw.(bool)
		raw = strconv.FormatBool(bv)
	default:
		return "", fmt.Errorf("cannot appear in a url (kind %s)", v.Kind)
	}
	return url.PathEscape(raw), nil
}
