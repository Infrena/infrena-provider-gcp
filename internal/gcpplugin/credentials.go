package gcpplugin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// cloudPlatformScope is the one scope every type in the catalog needs: every
// GCP REST API this plugin calls accepts it, and asking for anything
// narrower would mean maintaining a per-API scope list that has to be kept in
// sync with the catalog by hand.
const cloudPlatformScope = "https://www.googleapis.com/auth/cloud-platform"

// TokenSource resolves the OAuth2 token source this instance authenticates
// with: Application Default Credentials, honouring CredentialsFile when the
// instance names one, wrapped in an impersonated token source when
// Impersonate is set.
//
// It never logs a token, a credential path's contents, or an assertion --
// only ever the instance name and, on failure, the error ADC or the IAM
// Credentials API returned, which google's own libraries already scrub of
// key material.
func (i *Instance) TokenSource(ctx context.Context) (oauth2.TokenSource, error) {
	base, err := i.baseTokenSource(ctx)
	if err != nil {
		return nil, err
	}
	if i.Impersonate == "" {
		return base, nil
	}
	return newImpersonatedTokenSource(ctx, base, i.Impersonate), nil
}

// baseTokenSource resolves Application Default Credentials: the instance's
// own CredentialsFile when it names one, or the ambient ADC chain (the
// GOOGLE_APPLICATION_CREDENTIALS environment variable, the gcloud CLI's own
// credentials, or the metadata server) otherwise.
func (i *Instance) baseTokenSource(ctx context.Context) (oauth2.TokenSource, error) {
	if i.CredentialsFile == "" {
		creds, err := google.FindDefaultCredentials(ctx, cloudPlatformScope)
		if err != nil {
			return nil, fmt.Errorf("provider %q: no Application Default Credentials: %w", i.name, err)
		}
		return creds.TokenSource, nil
	}
	data, err := os.ReadFile(i.CredentialsFile)
	if err != nil {
		return nil, fmt.Errorf("provider %q: credentials_file: %w", i.name, err)
	}
	creds, err := google.CredentialsFromJSON(ctx, data, cloudPlatformScope)
	if err != nil {
		return nil, fmt.Errorf("provider %q: credentials_file is not a credential Google recognizes: %w", i.name, err)
	}
	return creds.TokenSource, nil
}

// impersonatedTokenSource exchanges the base token source's own token for one
// belonging to a target service account, via the IAM Credentials API's
// generateAccessToken.
//
// This is a direct HTTP call rather than a pull of google.golang.org/api's
// impersonate package: that package brings in gRPC, OpenTelemetry and the
// rest of the generated Google API client stack for what is otherwise one
// REST call this plugin already knows how to make (crud.go, Task 10 on,
// calls REST APIs the same way). The load-cost gate this task measures is
// exactly what that dependency weight would have worked against.
type impersonatedTokenSource struct {
	ctx    context.Context
	base   oauth2.TokenSource
	target string
}

func newImpersonatedTokenSource(ctx context.Context, base oauth2.TokenSource, target string) oauth2.TokenSource {
	// ReuseTokenSource caches the token until shortly before it expires, so
	// Token is only actually called -- and only actually reaches the network
	// -- once per hour (IAM Credentials access tokens default to a one hour
	// lifetime), not once per API call.
	return oauth2.ReuseTokenSource(nil, &impersonatedTokenSource{ctx: ctx, base: base, target: target})
}

type generateAccessTokenRequest struct {
	Scope []string `json:"scope"`
}

type generateAccessTokenResponse struct {
	AccessToken string `json:"accessToken"`
	ExpireTime  string `json:"expireTime"`
}

func (s *impersonatedTokenSource) Token() (*oauth2.Token, error) {
	baseTok, err := s.base.Token()
	if err != nil {
		return nil, fmt.Errorf("impersonating %s: obtaining the base token: %w", s.target, err)
	}

	reqBody, err := json.Marshal(generateAccessTokenRequest{Scope: []string{cloudPlatformScope}})
	if err != nil {
		return nil, fmt.Errorf("impersonating %s: %w", s.target, err)
	}
	url := fmt.Sprintf("https://iamcredentials.googleapis.com/v1/projects/-/serviceAccounts/%s:generateAccessToken", s.target)
	req, err := http.NewRequestWithContext(s.ctx, http.MethodPost, url, bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("impersonating %s: %w", s.target, err)
	}
	req.Header.Set("Content-Type", "application/json")
	baseTok.SetAuthHeader(req)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("impersonating %s: %w", s.target, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("impersonating %s: reading the response: %w", s.target, err)
	}
	if resp.StatusCode != http.StatusOK {
		// The IAM Credentials API's error body is a diagnostic Google
		// generates for the caller; it is not a credential and is safe to
		// include, unlike the request that produced it.
		return nil, fmt.Errorf("impersonating %s: generateAccessToken returned %s: %s", s.target, resp.Status, body)
	}
	var out generateAccessTokenResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("impersonating %s: parsing the response: %w", s.target, err)
	}
	expiry, err := time.Parse(time.RFC3339, out.ExpireTime)
	if err != nil {
		return nil, fmt.Errorf("impersonating %s: parsing the token's expiry: %w", s.target, err)
	}
	return &oauth2.Token{AccessToken: out.AccessToken, TokenType: "Bearer", Expiry: expiry}, nil
}
