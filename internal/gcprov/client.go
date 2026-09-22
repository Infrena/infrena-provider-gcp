// Package gcprov is the runtime GCP provider: the generic REST client, url
// expansion, error classification and backoff this task builds, and the
// await/CRUD/updateMask/reconciliation/discovery logic later tasks add on
// top of them (spec §5).
package gcprov

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"

	"github.com/infrena/infrena-provider-gcp/internal/catalog"
	"github.com/infrena/infrena/pkg/provider"
)

const (
	// defaultMaxAttempts bounds how many times Do tries one call, including
	// the first, when ClientOptions.MaxAttempts is unset.
	defaultMaxAttempts = 5
	// defaultTimeout bounds one HTTP round trip when ClientOptions.Timeout is
	// unset. Belt to the per-call context's braces, same reasoning as
	// gcpplugin's impersonatedTokenSource: whichever bound is shorter wins.
	defaultTimeout = 60 * time.Second
	// maxResponseBytes caps how much of one response body Do will read, so a
	// misbehaving endpoint cannot exhaust memory.
	maxResponseBytes = 32 << 20
)

// ClientOptions configures a Client. The zero value is a usable,
// unauthenticated-quota-project, unlimited-rate, default-timeout client.
type ClientOptions struct {
	// QuotaProject, when set, is sent as X-Goog-User-Project on every
	// request: the project billed and rate-limited for the call, which is
	// not necessarily the same project as the resource being called about
	// (gcpplugin's quota_project instance setting).
	QuotaProject string
	// QPS is the requests-per-second budget PER (project, API) pair. GCP
	// issues quota that way, not per account (spec §5.6), so the limiter is
	// keyed the same way -- see limiterFor. Zero means unlimited: no
	// pre-emptive shaping, relying on GCP's own 429s plus Do's retry/backoff.
	QPS float64
	// MaxAttempts bounds how many times Do tries one call, including the
	// first. Zero uses defaultMaxAttempts.
	MaxAttempts int
	// Timeout bounds one HTTP round trip. Zero uses defaultTimeout.
	Timeout time.Duration
}

// limiterKey is how the client cache's rate limiters are keyed: GCP quota is
// per API per project per minute, a different shape from AWS's per-account
// throttling, so the limiter must be keyed the same two-dimensional way
// rather than, say, per Client or per host alone.
type limiterKey struct {
	project string
	api     string
}

// Client is the generic GCP REST client every resource type's Read, Create,
// Update, Delete and Discover call goes through (later tasks). It holds the
// http.Client, the token source (via oauth2.Transport) and the per-(project,
// API) limiters.
//
// The limiter cache is what gives the limiter a life longer than one
// request: a bucket built fresh for every call would never accumulate the
// rate history a token bucket exists to carry (task-11 brief). Callers that
// want that persistence across many Provider method calls must keep one
// Client alive for the provider instance's whole lifetime rather than
// calling NewClient per call -- Task 13's NewProvider is expected to do
// exactly that.
type Client struct {
	httpClient *http.Client
	// base is used only when Do is given a relative url; every call this
	// package's own CRUD code makes passes a full "https://...googleapis.com/..."
	// url built from the catalog type's own api_base_url, so base exists
	// mainly so NewClient's signature is useful on its own (e.g. in a test
	// that never touches the catalog) rather than requiring an absolute url
	// every time.
	base string
	opts ClientOptions

	mu       sync.Mutex
	limiters map[limiterKey]*limiter
}

// NewClient builds a Client. ts supplies the Authorization header via
// oauth2.Transport; base is the default host+path prefix for a relative url
// passed to Do.
func NewClient(ts oauth2.TokenSource, base string, opts ClientOptions) *Client {
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	return &Client{
		httpClient: &http.Client{
			Timeout:   timeout,
			Transport: &oauth2.Transport{Source: ts},
		},
		base:     base,
		opts:     opts,
		limiters: make(map[limiterKey]*limiter),
	}
}

// limiterFor returns the shared token bucket for one (project, api) pair,
// constructing it on first use. Sharing across every call within a Client's
// lifetime -- rather than building one per request -- is the entire point:
// see the Client doc.
func (c *Client) limiterFor(project, api string) *limiter {
	key := limiterKey{project: project, api: api}

	c.mu.Lock()
	defer c.mu.Unlock()
	l, ok := c.limiters[key]
	if !ok {
		l = newLimiter(c.opts.QPS, nil)
		c.limiters[key] = l
	}
	return l
}

// Do sends one GCP REST call, retrying per ClassifyError with exponential
// full-jitter backoff (honouring APIError.RetryAfter when the previous
// attempt's response carried one), rate limited per (project, API).
//
// url may be absolute (the common case: a catalog type's own api_base_url
// plus its expanded url template) or relative to the Client's base. body is
// marshalled as the request's JSON body when non-nil; a nil body sends no
// request body (a GET, or a mutation whose whole request is the url, e.g.
// delete).
//
// Never logs a token, an Authorization header or a request body: errors
// mention only the method and url, which for every catalog type is a
// resource path, never a credential.
func (c *Client) Do(ctx context.Context, method, reqURL string, body any) (map[string]any, error) {
	full := reqURL
	if !strings.HasPrefix(full, "http://") && !strings.HasPrefix(full, "https://") {
		full = c.base + full
	}

	project, api := parseProjectAndAPI(full)
	lim := c.limiterFor(project, api)

	maxAttempts := c.opts.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = defaultMaxAttempts
	}

	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			if err := sleepBeforeRetry(ctx, attempt, lastErr); err != nil {
				return nil, err
			}
		}
		if err := lim.Wait(ctx); err != nil {
			return nil, err
		}

		result, err := c.doOnce(ctx, method, full, body)
		if err == nil {
			return result, nil
		}
		lastErr = err
		if ClassifyError(err) != provider.SafeToRetry {
			return nil, err
		}
	}
	return nil, lastErr
}

// sleepBeforeRetry waits between attempt-1 and attempt, honouring
// lastErr's Retry-After when it named one, or ctx ending, whichever comes
// first.
func sleepBeforeRetry(ctx context.Context, attempt int, lastErr error) error {
	wait := nextBackoff(attempt - 1)
	if ae, ok := lastErr.(*APIError); ok && ae.RetryAfter > 0 {
		wait = ae.RetryAfter
	}
	if wait <= 0 {
		return nil
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// doOnce sends one attempt: marshal, request, decode. It never retries.
func (c *Client) doOnce(ctx context.Context, method, fullURL string, body any) (map[string]any, error) {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("gcprov: encoding the request body: %w", err)
		}
		reader = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, fullURL, reader)
	if err != nil {
		return nil, fmt.Errorf("gcprov: building the request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.opts.QuotaProject != "" {
		req.Header.Set("X-Goog-User-Project", c.opts.QuotaProject)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		// err may wrap the request (method, url, timeout) but never the
		// Authorization header or body, which net/http never includes in a
		// transport error.
		return nil, fmt.Errorf("gcprov: %s %s: %w", method, fullURL, err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("gcprov: reading the response: %w", err)
	}

	if resp.StatusCode >= 300 {
		return nil, decodeAPIError(resp.StatusCode, data, resp.Header)
	}
	if len(data) == 0 {
		return map[string]any{}, nil
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("gcprov: parsing the response: %w", err)
	}
	return out, nil
}

// parseProjectAndAPI reads the (project, api) pair a request's own url
// identifies, for the rate limiter: GCP quota is issued per API per project
// per minute (spec §5.6), and Do has no other way to learn either value
// since its own signature carries only the expanded url.
//
// api is the API's own host label (e.g. "compute" from
// compute.googleapis.com, "storage" from storage.googleapis.com) -- every
// generated catalog type's api_base_url follows that <service>.googleapis.com
// convention (verified against the embedded catalog while building this
// task's test fixtures). project is read from a literal "projects/<id>/"
// path segment, GCP's own REST convention for every project-scoped resource
// name, not something this package invents. A url with no such segment (an
// organization- or billing-account-scoped call, e.g. accessPolicies) uses an
// empty project, sharing one limiter per API across those calls.
func parseProjectAndAPI(rawURL string) (project, api string) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", ""
	}
	host := u.Hostname()
	if i := strings.Index(host, "."); i > 0 {
		api = host[:i]
	} else {
		api = host
	}
	segs := strings.Split(u.Path, "/")
	for i, s := range segs {
		if s == "projects" && i+1 < len(segs) && segs[i+1] != "" {
			project = segs[i+1]
			break
		}
	}
	return project, api
}

// Provider is one configured GCP provider instance: the generic Client and
// the catalog it serves, plus the location defaults and discovery scope
// resolved from the instance's own configuration.
//
// Declared here, with only the fields the foundations and this package's own
// shared test helpers need, so Tasks 12 through 16 have one type to add
// methods to (await, Read/Create/Update/Delete/Import, BuildMask's Update,
// Discover) as they build them; NewProvider and the remaining
// provider.Provider methods arrive with Task 13's provider.go.
//
// Fields are plain values, never a *gcpplugin.Instance: gcpplugin.Plugin.New
// constructs a Provider (Task 17), which means gcpplugin already imports
// gcprov, so gcprov importing gcpplugin back -- even just to name
// *gcpplugin.Instance in a struct field or a constructor parameter -- would
// be an import cycle. Task 13's NewProvider must take gcpplugin.Instance's
// resolved fields some other way (plain parameters, or a small local struct
// gcpplugin converts into at its one call site), not the concrete type
// itself. Flagged in task-11-report.md for whoever picks up Task 13.
type Provider struct {
	client  *Client
	catalog *catalog.Catalog

	project string
	region  string
	zone    string

	quotaProject     string
	discoverTypes    []string
	discoverProjects []string
}
