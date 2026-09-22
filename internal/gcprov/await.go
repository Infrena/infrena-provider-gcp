package gcprov

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/infrena/infrena-provider-gcp/internal/catalog"
	"github.com/infrena/infrena/pkg/value"
)

// await blocks until a mutation has actually taken effect, and returns the
// resource's state as GCP reports it.
//
// resp is whatever the mutating call returned: the resource itself for a
// synchronous type, or an operation envelope for the other two. Which of the
// three it is was decided at generation time from the Operation SCHEMA, not
// from the API's name -- container, dns and sqladmin all use compute-style
// operations without being compute.
func (p *Provider) await(ctx context.Context, ty *catalog.Type, resp map[string]any) (map[string]any, error) {
	if ty.Await == catalog.AwaitNone {
		// The mutation returned the resource. Polling anything here would be a
		// request against a quota that belongs to the whole project, for an
		// answer we already hold.
		return resp, nil
	}

	// ONCE A MUTATION IS SENT, ABANDONING IT LEAVES SOMETHING THAT EXISTS AND IS
	// TRACKED NOWHERE. Cancellation is checked BEFORE sending (in crud.go), never
	// after. The bound from here on is the type's own timeout, not the caller's.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx),
		time.Duration(ty.TimeoutSeconds)*time.Second)
	defer cancel()

	switch ty.Await {
	case catalog.AwaitLongRunning:
		return p.awaitLongRunning(ctx, ty, resp)
	case catalog.AwaitComputeOperation:
		return p.awaitComputeOperation(ctx, ty, resp)
	default:
		return nil, fmt.Errorf("%s: unknown await strategy %d", ty.Name, ty.Await)
	}
}

// awaitLongRunning polls a google.longrunning.Operation by name until `done`.
//
// The shape is {"name": "...", "done": false} becoming either
// {"done": true, "response": {...}} or {"done": true, "error": {...}}. The
// poll target is ty.OperationPollPath (e.g. "v1/{+name}") expanded against
// the operation's own name -- the exact url the operation's own API
// publishes for it (its Discovery document's operations.get method path),
// taken verbatim rather than reconstructed from APIBaseURL plus a guessed
// version segment: 58 of the 97 real AwaitLongRunning types record no API
// version anywhere else in the catalog, so a reconstruction would have been
// wrong for most of them (see task-12-report.md). "{+name}" is RFC 6570
// reserved expansion -- an operation name is a path containing "/" -- which
// ExpandURL (Task 11) already handles unescaped.
func (p *Provider) awaitLongRunning(ctx context.Context, ty *catalog.Type, op map[string]any) (map[string]any, error) {
	name, _ := op["name"].(string)
	if name == "" {
		return nil, fmt.Errorf("%s: operation has no name to poll", ty.Name)
	}
	if ty.OperationPollPath == "" {
		return nil, fmt.Errorf("%s: no operation poll path for a longrunning await", ty.Name)
	}
	for attempt := 0; ; attempt++ {
		if done, _ := op["done"].(bool); done {
			// An operation that completed with an error is not an await failure,
			// it is a GCP failure, and its message is the only thing the user can
			// act on. Do not replace it with our own wording.
			if e, ok := op["error"].(map[string]any); ok {
				return nil, operationError(ty, e)
			}
			if r, ok := op["response"].(map[string]any); ok {
				return r, nil
			}
			// Done, no error, no response: a delete, or a create whose result
			// must be read back. The caller decides which.
			return nil, nil
		}
		if err := p.sleepBackoff(ctx, attempt); err != nil {
			return nil, fmt.Errorf("%s: waiting for %s: %w", ty.Name, name, err)
		}
		pollPath, err := ExpandURL(ty.OperationPollPath, map[string]value.Value{
			"name": value.String(name, value.SourceProvider),
		})
		if err != nil {
			return nil, fmt.Errorf("%s: building the operation poll url: %w", ty.Name, err)
		}
		op, err = p.client.Do(ctx, http.MethodGet, ty.APIBaseURL+pollPath, nil)
		if err != nil {
			return nil, err
		}
	}
}

// awaitComputeOperation polls a compute-style operation until status is DONE.
//
// It uses the operation collection's `wait` method, which LONG-POLLS to a
// 2-minute deadline rather than returning immediately. That is the difference
// between one request per slow operation and one per second against a quota
// the whole project shares. ty.OperationScope names the location axis, a
// bare word -- "global", "region" or "zone" (internal/gen/build.go) -- not
// the operations collection itself: the collection is that word plus
// "Operations" (globalOperations/regionOperations/zoneOperations), which
// operationWaitURL builds.
func (p *Provider) awaitComputeOperation(ctx context.Context, ty *catalog.Type, op map[string]any) (map[string]any, error) {
	for attempt := 0; ; attempt++ {
		if st, _ := op["status"].(string); st == "DONE" {
			// compute reports failure as error.errors[], a different shape from
			// longrunning's error object. Both reach the user as one message.
			if e, ok := op["error"].(map[string]any); ok {
				return nil, operationError(ty, e)
			}
			return op, nil
		}
		url, err := p.operationWaitURL(ty, op)
		if err != nil {
			return nil, err
		}
		if attempt > 0 {
			// `wait` already blocks server-side, so back off only between
			// returns, and gently -- this is not a busy poll.
			if err := p.sleepBackoff(ctx, attempt); err != nil {
				return nil, fmt.Errorf("%s: waiting for operation: %w", ty.Name, err)
			}
		}
		if op, err = p.client.Do(ctx, http.MethodPost, url, nil); err != nil {
			return nil, err
		}
	}
}

// operationWaitURL builds the compute-style `wait` url:
// <APIBaseURL>projects/<project>/<scope segment>/<OperationScope>Operations/<op name>/wait.
//
// The scope segment ("zones/<zone>", "regions/<region>", or "global") is
// read from the operation's own "zone" or "region" field when present --
// real compute operations carry those as full urls, so only the last path
// segment is used -- and falls back to the type's own Settings-configured
// zone/region otherwise. A wait url built without the right scope 404s,
// which reads as "the operation vanished" rather than "we asked the wrong
// collection".
func (p *Provider) operationWaitURL(ty *catalog.Type, op map[string]any) (string, error) {
	name, _ := op["name"].(string)
	if name == "" {
		return "", fmt.Errorf("%s: operation has no name to poll", ty.Name)
	}
	if ty.OperationScope == "" {
		return "", fmt.Errorf("%s: no operation scope to build a wait url", ty.Name)
	}

	var scopeSegment string
	switch ty.Scope {
	case catalog.ScopeZonal:
		zone := lastPathSegment(op["zone"])
		if zone == "" {
			zone = p.settings.Zone
		}
		if zone == "" {
			return "", fmt.Errorf("%s: no zone to poll a zone operation", ty.Name)
		}
		scopeSegment = "zones/" + zone
	case catalog.ScopeRegional:
		region := lastPathSegment(op["region"])
		if region == "" {
			region = p.settings.Region
		}
		if region == "" {
			return "", fmt.Errorf("%s: no region to poll a region operation", ty.Name)
		}
		scopeSegment = "regions/" + region
	default:
		scopeSegment = "global"
	}

	return fmt.Sprintf("%sprojects/%s/%s/%sOperations/%s/wait",
		ty.APIBaseURL, p.settings.Project, scopeSegment, ty.OperationScope, name), nil
}

// lastPathSegment returns v's trailing "/"-delimited segment, or v itself
// when it carries no slash -- compute's own zone/region fields come back as
// full urls (".../zones/us-central1-a"), but a hand-built operation (e.g. a
// test) may give the bare zone name directly, and both must resolve the
// same way.
func lastPathSegment(v any) string {
	s, _ := v.(string)
	if i := strings.LastIndex(s, "/"); i >= 0 {
		return s[i+1:]
	}
	return s
}

// operationError renders either failure shape GCP reports for a completed
// operation into one *APIError, carrying GCP's own text through: a wrapper
// that says "the operation failed" and drops the reason leaves the user
// nothing to act on.
//
// google.longrunning reports {"code": <int>, "message": <string>} (the
// grpc numeric code, google.rpc.Status); compute reports
// {"errors": [{"code": <string>, "message": <string>}, ...]}, possibly
// several -- their messages are joined, since any of them may be the one
// that matters.
func operationError(ty *catalog.Type, e map[string]any) *APIError {
	if rawErrors, ok := e["errors"].([]any); ok {
		var code string
		var msgs []string
		for _, re := range rawErrors {
			em, ok := re.(map[string]any)
			if !ok {
				continue
			}
			if c, ok := em["code"].(string); ok && code == "" {
				code = c
			}
			if m, ok := em["message"].(string); ok && m != "" {
				msgs = append(msgs, m)
			}
		}
		return &APIError{Code: code, Message: fmt.Sprintf("%s: %s", ty.Name, strings.Join(msgs, "; "))}
	}

	msg, _ := e["message"].(string)
	var code string
	if c, ok := e["code"].(float64); ok {
		code = fmt.Sprintf("%d", int(c))
	}
	return &APIError{Code: code, Message: fmt.Sprintf("%s: %s", ty.Name, msg)}
}

// sleepBackoff waits out one polling interval, or returns ctx's error if
// ctx ends first. attempt is 0-based: the wait before the first re-poll is
// sleepBackoff(ctx, 0). Reuses backoffDelay's full-jitter schedule (no
// Retry-After to honour here -- an operation poll is not a failed request)
// so a polling loop backs off the same way Client.Do's own retries do,
// rather than inventing a second schedule.
func (p *Provider) sleepBackoff(ctx context.Context, attempt int) error {
	wait := backoffDelay(attempt, 0)
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
