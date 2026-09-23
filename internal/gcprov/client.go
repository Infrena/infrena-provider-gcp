// Package gcprov is the runtime GCP provider: the generic REST client, url
// expansion, error classification and backoff this task builds, and the
// await/CRUD/updateMask/reconciliation/discovery logic later tasks add on
// top of them (spec §5).
package gcprov

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	// defaultTimeout bounds one HTTP round trip when ClientOptions.Timeout and
	// ClientOptions.HTTPClient are both unset. 30s, matching what this
	// repository's own gcpplugin/credentials.go settled on for the same
	// reason: this client is a child process infrena launches over stdio, and
	// http.DefaultClient (no timeout at all) leaves it wedged on a hung
	// endpoint with nothing to read and no way out. Belt to the per-call
	// context's braces -- whichever bound is shorter wins.
	defaultTimeout = 30 * time.Second
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
	// Timeout bounds one HTTP round trip. Zero uses defaultTimeout. Ignored
	// when HTTPClient is set with its own non-zero Timeout.
	//
	// It was declared, documented and NEVER READ until 2026-09-22: every
	// Client in the process ran at defaultTimeout whatever this said, and
	// nothing noticed because no test ever set it. Found while building
	// TestAComputeWaitOutlivesTheOrdinaryRoundTripBound, which needs a
	// round-trip bound short enough to measure -- the test passed against
	// the unfixed code, which is what exposed the dead knob rather than the
	// defect it was aimed at. NewClient now honours it; see there.
	Timeout time.Duration
	// HTTPClient, when set, supplies the base transport and timeout Do's
	// requests run over (its Transport is wrapped with oauth2 auth; its
	// Timeout is kept as-is, falling back to defaultTimeout if it too is
	// zero). nil means NewClient builds a sane default WITH a Timeout --
	// never http.DefaultClient, which has none at all.
	HTTPClient *http.Client
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
// The limiter cache (c.limiters, below) is what gives the limiter a life
// longer than one request: a bucket built fresh for every call would never
// accumulate the rate history a token bucket exists to carry. DO NOT
// "SIMPLIFY" IT AWAY -- from the outside a Client with no in-flight request
// looks stateless, and a reader who does not know why the map is there will
// be tempted to. Callers that want that persistence across many Provider
// method calls must keep one Client alive for the provider instance's whole
// lifetime rather than calling NewClient per call -- Task 13's NewProvider is
// expected to do exactly that.
type Client struct {
	httpClient *http.Client
	// longPollClient is httpClient with its round-trip bound REMOVED, for
	// DoLongPoll. It shares the same (auth-wrapped) Transport, so it shares
	// the connection pool and the token source too; only the Timeout field
	// differs. A per-request context deadline is what bounds a long poll
	// instead -- see DoLongPoll for why the fixed bound cannot be reused and
	// why removing it here is not the same as having none.
	longPollClient *http.Client
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
//
// opts.HTTPClient, when given, supplies the base transport and timeout: its
// Transport is wrapped (never replaced) with oauth2 auth, and its own
// Timeout is kept, so a caller can still inject a shorter timeout or a
// custom transport (e.g. for a test) while auth is still attached. Left
// nil, NewClient builds a default *http.Client with defaultTimeout --
// never the bare, timeout-less http.DefaultClient.
func NewClient(ts oauth2.TokenSource, base string, opts ClientOptions) *Client {
	hc := opts.HTTPClient
	if hc == nil {
		hc = &http.Client{}
	} else {
		cp := *hc
		hc = &cp // never mutate a *http.Client the caller still holds
	}
	// opts.Timeout second, so an HTTPClient carrying its own Timeout still
	// wins (that is what its doc promises), and defaultTimeout last.
	if hc.Timeout <= 0 {
		hc.Timeout = opts.Timeout
	}
	if hc.Timeout <= 0 {
		hc.Timeout = defaultTimeout
	}
	baseTransport := hc.Transport
	if baseTransport == nil {
		baseTransport = http.DefaultTransport
	}
	hc.Transport = &oauth2.Transport{Source: ts, Base: baseTransport}

	// The same client, same Transport, with only the round-trip bound taken
	// off. Copied AFTER the Transport is wrapped, so the long-poll client is
	// authenticated identically rather than needing its own wrapping.
	lp := *hc
	lp.Timeout = 0

	return &Client{
		httpClient:     hc,
		longPollClient: &lp,
		base:           base,
		opts:           opts,
		limiters:       make(map[limiterKey]*limiter),
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
	return c.do(ctx, method, reqURL, body, 0)
}

// DoLongPoll is Do for a request THE API ITSELF ANSWERS SLOWLY BY DESIGN:
// compute's operations/{op}/wait blocks server-side for up to two minutes
// and returns when the operation finishes, which is the entire point of the
// method and the reason await.go uses it instead of one GET per second
// against a quota the whole project shares.
//
// Do's round-trip bound is the wrong bound for such a request. 30s exists to
// notice a HUNG endpoint -- one with nothing to read and no way out -- and a
// published long poll is not hung, it is doing what it published. Measured
// against real Google on 2026-09-22 (task-18 finding 2): every compute
// operation that had not finished inside thirty seconds failed with
// "context deadline exceeded (Client.Timeout exceeded while awaiting
// headers)", 3 times out of 3 on an instance delete. The instance really was
// deleted and infrena reported failure, which is the orphan rule seen from
// the other end -- something real that state says is still there.
//
// bound is the request's own round-trip limit, and the caller supplies it
// from the type's own TimeoutSeconds, exactly as await.go bounds the whole
// operation. So a long poll may take as long as the operation it is waiting
// on is allowed to take, and no longer: a hung wait still ends, at the same
// moment the await itself would have given up anyway. Nothing becomes
// unbounded.
//
// EXPLICIT AT THE CALL SITE, NEVER INFERRED FROM THE URL. A url ending in
// "/wait" is a guess about a naming convention; whether the type publishes a
// wait method is a fact the catalog records, and awaitComputeOperation
// already reads it to decide the HTTP method. Deciding here instead would
// give a longer bound to any endpoint whose path happened to look right.
//
// A bound of zero or less is Do: no caller should ask for a long poll with
// no limit, and silently granting one would be the wedged plugin
// defaultTimeout exists to prevent.
func (c *Client) DoLongPoll(ctx context.Context, method, reqURL string, body any, bound time.Duration) (map[string]any, error) {
	return c.do(ctx, method, reqURL, body, bound)
}

func (c *Client) do(ctx context.Context, method, reqURL string, body any, longPollBound time.Duration) (map[string]any, error) {
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

		result, err := c.doOnce(ctx, method, full, body, longPollBound)
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
// lastErr's Retry-After when it named one (unwrapped with errors.As, same
// reason as ClassifyError: lastErr may be wrapped), or ctx ending, whichever
// comes first.
func sleepBeforeRetry(ctx context.Context, attempt int, lastErr error) error {
	var ra time.Duration
	var ae *APIError
	if errors.As(lastErr, &ae) {
		ra = ae.RetryAfter
	}
	wait := backoffDelay(attempt-1, ra)
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
//
// longPollBound > 0 selects the long-poll client -- the one with no Timeout
// of its own -- and bounds this attempt with a context deadline instead.
// It has to be both: http.Client.Timeout is a ceiling the request context
// cannot raise, so a longer deadline alone would still be cut off at 30s.
func (c *Client) doOnce(ctx context.Context, method, fullURL string, body any, longPollBound time.Duration) (map[string]any, error) {
	httpClient := c.httpClient
	if longPollBound > 0 {
		httpClient = c.longPollClient
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, longPollBound)
		// Safe to cancel on return: the response body is read to completion
		// below, before this function's only successful exit.
		defer cancel()
	}

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

	resp, err := httpClient.Do(req)
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
		return nil, c.explainQuotaProject(decodeAPIError(resp, data))
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

// userProjectDenied is the google.rpc.ErrorInfo reason GCP answers with when
// X-Goog-User-Project names a project the caller may not bill to.
const userProjectDenied = "USER_PROJECT_DENIED"

// explainQuotaProject adds the ONE piece of context the caller cannot get
// from the response: that this 403 is about the quota_project setting and
// not about the permission it appears to name.
//
// Measured against live GCP, 2026-09-22. A service account holding
// roles/storage.admin, sending X-Goog-User-Project, is told:
//
//	infrena-live@...iam.gserviceaccount.com does not have
//	serviceusage.services.use access to the Google Cloud project
//
// which reads as "your storage grant is wrong". It is not. Setting a quota
// project requires serviceusage.services.use ON THAT PROJECT, a permission
// nothing about storage would lead anyone to look for, and the remediation
// is usually to REMOVE the setting rather than to add a role: a quota
// project exists for a USER credential, which has no project of its own to
// bill, and an impersonated service account already belongs to one.
//
// Wrapped with %w, never replaced: ClassifyError, isNotFound and every other
// caller reach the *APIError through errors.As, and swallowing it here would
// silently downgrade the whole error taxonomy to "unrecognised".
//
// Only when a quota project is actually set, so an instance that never
// configured one cannot be told its problem is a setting it does not use.
func (c *Client) explainQuotaProject(ae *APIError) error {
	if c.opts.QuotaProject == "" || ae.Status != http.StatusForbidden {
		return ae
	}
	if ae.Reason != userProjectDenied && !strings.Contains(ae.Message, "serviceusage.services.use") {
		return ae
	}
	return fmt.Errorf("%w -- this is the quota_project setting (%q), not the permission it names: "+
		"sending it requires serviceusage.services.use on that project. An impersonated service "+
		"account or a key already bills to its own project, so removing quota_project is usually "+
		"the fix; granting roles/serviceusage.serviceUsageConsumer is the other",
		ae, c.opts.QuotaProject)
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

// Settings is gcprov's OWN view of a provider instance's configuration:
// location defaults and discovery scope, nothing else.
//
// Deliberately not *gcpplugin.Instance. gcpplugin.Plugin.New (Task 17)
// constructs a Provider via NewProvider (Task 13), so gcpplugin imports
// gcprov; gcprov importing gcpplugin back -- even just to name
// *gcpplugin.Instance in a parameter type -- would be an import cycle.
// gcpplugin converts its own Instance into a Settings at that one call site
// instead. QuotaProject is deliberately NOT here: it already lives on the
// Client (ClientOptions.QuotaProject), which every Provider method reaches
// through p.client, so duplicating it here would be two sources of truth for
// one value.
type Settings struct {
	Project string
	Region  string
	Zone    string

	DiscoverTypes    []string
	DiscoverProjects []string

	// AssetInventoryBaseURL is where Cloud Asset Inventory lives. Empty means
	// CloudAssetBaseURL, the real endpoint, which is what every configured
	// instance uses.
	//
	// It is a field rather than a constant because CAI is the one API this
	// provider calls that is NOT in the catalog for the type being
	// discovered: every other request url comes from a catalog type's own
	// APIBaseURL, which a test redirects at the fake by rewriting the catalog
	// (pointCatalogAt). Discovery has no catalog type to take a host from --
	// the asset search is about the project, not about any one type -- so
	// without this a discovery test would send its search to the real
	// cloudasset.googleapis.com.
	AssetInventoryBaseURL string
}

// Provider is one configured GCP provider instance: the generic Client and
// the catalog it serves, plus the Settings resolved from the instance's own
// configuration.
//
// Declared here, with only the fields the foundations and this package's own
// shared test helpers need, so Tasks 12 through 16 have one type to add
// methods to (await, Read/Create/Update/Delete/Import, BuildMask's Update,
// Discover) as they build them; NewProvider and the remaining
// provider.Provider methods arrive with Task 13's provider.go, which adds to
// this same type rather than redeclaring it.
type Provider struct {
	client  *Client
	catalog *catalog.Catalog

	settings Settings
}
