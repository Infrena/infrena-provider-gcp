package gcpplugin

import (
	"fmt"
	"sort"

	"github.com/infrena/infrena/pkg/provider"
	"github.com/infrena/infrena/pkg/value"
)

// known is every configuration key an instance accepts.
//
// Unknown keys are REFUSED, not ignored. A misspelled `impersonate_service_acount`
// silently ignored is an instance quietly running as the wrong identity, which is
// the failure a user is least able to see.
var known = map[string]bool{
	"project": true, "region": true, "zone": true,
	"credentials_file": true, "impersonate_service_account": true, "quota_project": true,
	"discover_types": true, "discover_projects": true,
}

// Instance is one configured provider instance.
type Instance struct {
	Project string
	Region  string
	Zone    string

	CredentialsFile string
	Impersonate     string
	QuotaProject    string

	DiscoverTypes    []string
	DiscoverProjects []string

	name string
}

// ParseConfig reads one instance's configuration.
func ParseConfig(cfg provider.Config) (*Instance, error) {
	var unknown []string
	for k := range cfg.Values {
		if !known[k] {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		accepted := make([]string, 0, len(known))
		for k := range known {
			accepted = append(accepted, k)
		}
		sort.Strings(accepted)
		return nil, fmt.Errorf("provider %q: unknown configuration %v; accepted keys are %v",
			cfg.Instance, unknown, accepted)
	}

	in := &Instance{name: cfg.Instance}
	str := func(key string) (string, error) {
		v, ok := cfg.Value(key)
		if !ok {
			return "", nil
		}
		if v.Kind != value.KindString {
			return "", fmt.Errorf("provider %q: %s must be a string, got %s", cfg.Instance, key, v.Kind)
		}
		s, _ := v.Raw.(string)
		return s, nil
	}
	list := func(key string) ([]string, error) {
		v, ok := cfg.Value(key)
		if !ok {
			return nil, nil
		}
		if v.Kind != value.KindList {
			return nil, fmt.Errorf("provider %q: %s must be a list, got %s", cfg.Instance, key, v.Kind)
		}
		items, _ := v.Raw.([]value.Value)
		out := make([]string, 0, len(items))
		for _, it := range items {
			s, _ := it.Raw.(string)
			out = append(out, s)
		}
		return out, nil
	}

	var err error
	for _, f := range []struct {
		key string
		dst *string
	}{
		{"project", &in.Project}, {"region", &in.Region}, {"zone", &in.Zone},
		{"credentials_file", &in.CredentialsFile},
		{"impersonate_service_account", &in.Impersonate},
		{"quota_project", &in.QuotaProject},
	} {
		if *f.dst, err = str(f.key); err != nil {
			return nil, err
		}
	}
	if in.DiscoverTypes, err = list("discover_types"); err != nil {
		return nil, err
	}
	if in.DiscoverProjects, err = list("discover_projects"); err != nil {
		return nil, err
	}
	return in, nil
}
