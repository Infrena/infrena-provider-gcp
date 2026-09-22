// Package gcptest isolates tests from the developer's own GCP credentials.
//
// Without this a test that forgets to point at the fake reaches real GCP with
// whatever the developer happens to be logged in as. Every test in this
// repository that constructs a client calls Isolate.
package gcptest

import "testing"

// Isolate removes every way the oauth2/google Application Default Credentials
// chain can find a real identity: the explicit key file, the gcloud well-known
// file, and the metadata server.
func Isolate(t *testing.T) {
	t.Helper()
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "")
	t.Setenv("CLOUDSDK_CONFIG", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	// The metadata server is reached by hostname. Pointing it at a port nothing
	// listens on makes the lookup fail fast instead of hanging on a real GCE box.
	t.Setenv("GCE_METADATA_HOST", "127.0.0.1:1")
	t.Setenv("GOOGLE_CLOUD_PROJECT", "")
}
