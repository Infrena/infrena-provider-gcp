package gcpplugin

import (
	"strings"
	"testing"

	"github.com/infrena/infrena/pkg/provider"
	"github.com/infrena/infrena/pkg/value"
)

func vals(kv map[string]string) map[string]value.Value {
	out := map[string]value.Value{}
	for k, v := range kv {
		out[k] = value.String(v, value.SourceExplicit)
	}
	return out
}

// TestAMisspelledKeyIsRefused is the important one. Ignoring it would mean an
// instance quietly running as the developer's own identity instead of the
// service account the user named.
func TestAMisspelledKeyIsRefused(t *testing.T) {
	_, err := ParseConfig(provider.Config{
		Instance: "prod",
		Values:   vals(map[string]string{"impersonate_service_acount": "x@y.iam.gserviceaccount.com"}),
	})
	if err == nil {
		t.Fatal("a misspelled key was accepted")
	}
	if !strings.Contains(err.Error(), "impersonate_service_acount") {
		t.Errorf("the error does not name the offending key: %v", err)
	}
	if !strings.Contains(err.Error(), "impersonate_service_account") {
		t.Errorf("the error does not list the accepted keys, so the user cannot see the typo: %v", err)
	}
}

func TestEveryAcceptedKeyIsRead(t *testing.T) {
	in, err := ParseConfig(provider.Config{
		Instance: "prod",
		Values: vals(map[string]string{
			"project": "p", "region": "us-central1", "zone": "us-central1-a",
			"credentials_file": "/k.json", "impersonate_service_account": "sa@p.iam.gserviceaccount.com",
			"quota_project": "q",
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if in.Project != "p" || in.Region != "us-central1" || in.Zone != "us-central1-a" ||
		in.CredentialsFile != "/k.json" || in.Impersonate != "sa@p.iam.gserviceaccount.com" ||
		in.QuotaProject != "q" {
		t.Errorf("a key was read into the wrong field or not at all: %+v", in)
	}
}
