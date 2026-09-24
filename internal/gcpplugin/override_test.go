package gcpplugin

import (
	"strings"
	"testing"

	"github.com/infrena/infrena-provider-gcp/internal/catalog"
)

// TestTheOverrideLeavesNoGoogleHost is the override's one promise, checked
// over the real catalog: nothing reaches the real cloud. A type's regional
// endpoint template is a Google host that the rehosting of APIBaseURL never
// touched, so a test under the override would have sent regional secrets to
// secretmanager.<location>.rep.googleapis.com.
func TestTheOverrideLeavesNoGoogleHost(t *testing.T) {
	c, err := catalog.Load()
	if err != nil {
		t.Fatal(err)
	}
	var templated int
	for _, ty := range c.Types {
		if ty.EndpointTemplate != "" {
			templated++
		}
	}
	if templated == 0 {
		t.Fatal("no type in the real catalog has an endpoint template, so this test guards nothing")
	}
	redirected, _, err := redirect(c, "http://127.0.0.1:1", "test")
	if err != nil {
		t.Fatal(err)
	}
	for _, ty := range redirected.Types {
		for what, u := range map[string]string{"APIBaseURL": ty.APIBaseURL, "EndpointTemplate": ty.EndpointTemplate} {
			if strings.Contains(u, "googleapis.com") {
				t.Errorf("%s: %s still points at Google under the override: %s", ty.Name, what, u)
			}
		}
	}
}
