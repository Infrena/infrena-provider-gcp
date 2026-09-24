package gcprov

import (
	"context"
	"fmt"
	"net/http"
	"slices"
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
	return p.awaitAs(ctx, ty, ty.Await, resp)
}

// awaitAs is await for a mutation whose completion is kind rather than the
// type's create-time Await: Delete passes ty.DeleteAwaitKind().
func (p *Provider) awaitAs(ctx context.Context, ty *catalog.Type, kind catalog.AwaitKind, resp map[string]any) (map[string]any, error) {
	if kind == catalog.AwaitNone {
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

	switch kind {
	case catalog.AwaitLongRunning:
		return p.awaitLongRunning(ctx, ty, resp)
	case catalog.AwaitComputeOperation:
		return p.awaitComputeOperation(ctx, ty, resp)
	default:
		return nil, fmt.Errorf("%s: unknown await strategy %d", ty.Name, kind)
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
	for attempt := 0; ; attempt++ {
		if done, _ := op["done"].(bool); done {
			// An operation that completed with an error is not an await failure,
			// it is a GCP failure, and its message is the only thing the user can
			// act on. Do not replace it with our own wording.
			if e, ok := op["error"].(map[string]any); ok {
				return nil, operationError(ty, e)
			}
			if r, ok := op["response"].(map[string]any); ok {
				return unwrapAny(r), nil
			}
			// Done, no error, no response: a delete, or a create whose result
			// must be read back. The caller decides which.
			return nil, nil
		}
		// EVERYTHING A POLL NEEDS IS CHECKED ONLY ON THE PATH THAT POLLS.
		// These two checks used to run before the loop, and refused an
		// answer already in front of them: Cloud Resource Manager answers
		// POST v3/tagBindings with an operation that is ALREADY DONE and
		// carries NO name, because there is nothing to poll -- captured
		// verbatim from real Google on 2026-09-22 (task-18 finding 8). The
		// binding was created, this returned "operation has no name to
		// poll", and the host dropped the failed create's result, leaving a
		// real binding tracked nowhere. The orphan rule, for a third time.
		//
		// op["name"], deliberately and unlike the compute-style path, which
		// fills the same placeholder from the operation's selfLink instead:
		// a google.longrunning.Operation's name IS its full relative
		// resource name, which is exactly what operations.get takes. See
		// operationResourcePath for what happens when the two are confused
		// for each other.
		name, _ := op["name"].(string)
		if name == "" {
			return nil, fmt.Errorf("%s: operation has no name to poll", ty.Name)
		}
		if ty.OperationPollPath == "" {
			return nil, fmt.Errorf("%s: no operation poll path for a longrunning await", ty.Name)
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

// unwrapAny returns a google.longrunning.Operation's `response` as the plain
// resource it holds, with the `@type` discriminator dropped.
//
// A longrunning response is a google.protobuf.Any, and "@type" is the
// ENVELOPE'S field, not the resource's -- it names which message the
// envelope carries, which the caller already knew. Everything else in the
// map is the resource itself.
//
// MEASURED AGAINST REAL GOOGLE, 2026-09-22, and it is the orphan rule for a
// FOURTH time. With the done-before-name ordering fixed above, a tag binding
// create reaches this for the first time, and the host refuses the state
// that comes out of it:
//
//	x create binding: gcp returned attribute "@type" on a gcp.tagbinding,
//	which its own schema does not declare
//
// The binding was real -- this suite's sweep had to delete it -- and the
// create was reported as failed, so nothing tracked it. Not a tag-binding
// problem: EVERY one of the 97 AwaitLongRunning types answers this way, and
// any of them reaches it whenever the readback after a create loses the race
// with eventual consistency and Create falls back to bestEffortState.
//
// A copy, never a delete in place: op is the caller's map and the
// response body is also what an error path may want to report verbatim.
// Only "@type" is removed; a field the schema does not declare for any other
// reason is still reported, because that is a real disagreement with the
// catalog and silently dropping it would hide it.
func unwrapAny(response map[string]any) map[string]any {
	if _, ok := response["@type"]; !ok {
		return response
	}
	out := make(map[string]any, len(response)-1)
	for k, v := range response {
		if k == "@type" {
			continue
		}
		out[k] = v
	}
	return out
}

// awaitComputeOperation polls a compute-style operation until status is DONE.
//
// It uses the operation's own published `wait` method when the type has one
// (ty.OperationWaitPath), which LONG-POLLS to a 2-minute deadline rather than
// returning immediately -- the difference between one request per slow
// operation and one per second against a quota the whole project shares.
// 7 of the 65 compute-style types (container, sqladmin) publish no wait
// method at all -- a wait call against them is a 404, not a slow poll,
// confirmed against their own Discovery documents -- and are polled with an
// ordinary GET on ty.OperationPollPath instead, same status/error body shape.
// Both paths are the API's own published templates, taken verbatim and
// expanded rather than reconstructed: see operationRequestURL.
func (p *Provider) awaitComputeOperation(ctx context.Context, ty *catalog.Type, op map[string]any) (map[string]any, error) {
	waits := ty.OperationWaitPath != ""
	for attempt := 0; ; attempt++ {
		if st, _ := op["status"].(string); st == "DONE" {
			// compute reports failure as error.errors[], a different shape from
			// longrunning's error object. Both reach the user as one message.
			if e, ok := op["error"].(map[string]any); ok {
				return nil, operationError(ty, e)
			}
			// An operation is not the thing it created. compute publishes the
			// created resource's own url in targetLink; the operation's
			// selfLink names the OPERATION, and ProviderID prefers selfLink,
			// so handing the operation back yielded an id addressing
			// ".../operations/operation-1740..." for all 65 compute-style
			// types -- and, for the 12 of them whose self_link ENDS in
			// "{{name}}" (the criterion the second failure mode turns on;
			// 13 CONTAIN a name placeholder somewhere), the operation's own
			// name merged over the caller's attributes on top of that.
			if target, _ := op["targetLink"].(string); target != "" {
				return map[string]any{"selfLink": target}, nil
			}
			// No targetLink: nothing here identifies the resource, so say
			// nothing rather than something wrong. Create falls back to the
			// caller's own attributes, which is exactly the right answer --
			// the same (nil, nil) awaitLongRunning returns for a
			// done-with-no-response operation.
			return nil, nil
		}
		method, url, err := p.operationRequestURL(ty, op)
		if err != nil {
			return nil, err
		}
		// A published wait already blocks server-side for up to two minutes,
		// so back off only between returns (never before the first), and
		// gently -- this is not a busy poll. An API with no wait method
		// returns immediately every time, so back off before every attempt,
		// the same way awaitLongRunning's ordinary GET poll does.
		if !(waits && attempt == 0) {
			if err := p.sleepBackoff(ctx, attempt); err != nil {
				return nil, fmt.Errorf("%s: waiting for operation: %w", ty.Name, err)
			}
		}
		// A PUBLISHED WAIT IS NOT AN ORDINARY REQUEST, and the difference is
		// decided here, from the catalog fact that chose the method above --
		// never inferred from the url's shape. compute's wait blocks
		// server-side for up to two minutes; the client's ordinary
		// round-trip bound is 30s; so before this every compute operation
		// slower than thirty seconds failed while GCP went on to complete
		// it (task-18 finding 2, measured 3/3 on an instance delete). The
		// bound is the type's own TimeoutSeconds, the same one bounding the
		// whole await above, so a wait can take as long as the operation is
		// allowed to and not a second more. The no-wait GET poll returns
		// immediately by design and keeps the ordinary bound: a GET that
		// hangs really is a hung endpoint.
		if waits {
			op, err = p.client.DoLongPoll(ctx, method, url, nil,
				time.Duration(ty.TimeoutSeconds)*time.Second)
		} else {
			op, err = p.client.Do(ctx, method, url, nil)
		}
		if err != nil {
			return nil, err
		}
	}
}

// operationRequestURL returns the http method and expanded url for the next
// poll of a compute-style operation: POST ty.OperationWaitPath (a genuine
// long-poll) when the API publishes one, otherwise GET ty.OperationPollPath
// (an ordinary poll -- see awaitComputeOperation). Both are the API's own
// published templates (verified against schemas/compute.json,
// internal/gen/build.go), taken verbatim rather than reconstructed from a
// scope word: three attempts at rebuilding this url from
// ty.OperationScope -- a bare "global"/"region"/"zone" -- were all wrong,
// because the wire's literal segment is always "operations", never
// "globalOperations"/"regionOperations"/"zoneOperations" (only Discovery's
// collection name for the scope). ty.OperationScope is no longer read here.
//
// The templates use ordinary {project}/{zone}/{region}/{operation}
// placeholders (compute's own wait path) or reserved {+name} (a no-wait
// API's poll path, e.g. container) -- ExpandURL handles whichever the
// template actually names, and errors by placeholder name by itself when one
// it does need is missing, so this does not duplicate that validation.
//
// What "name" is filled WITH is the one thing this cannot delegate: see
// operationResourcePath, and the refusal below for when even that has
// nothing that addresses an operation.
func (p *Provider) operationRequestURL(ty *catalog.Type, op map[string]any) (method, url string, err error) {
	name, _ := op["name"].(string)
	if name == "" {
		return "", "", fmt.Errorf("%s: operation has no name to poll", ty.Name)
	}

	tmpl := ty.OperationWaitPath
	method = http.MethodPost
	if tmpl == "" {
		tmpl = ty.OperationPollPath
		method = http.MethodGet
	}
	if tmpl == "" {
		return "", "", fmt.Errorf("%s: no operation wait or poll path", ty.Name)
	}

	attrs := map[string]value.Value{
		"operation": value.String(name, value.SourceProvider),
		"name":      value.String(operationResourcePath(ty, op), value.SourceProvider),
		"project":   value.String(p.settings.Project, value.SourceProvider),
	}
	// A template that captures the whole path under "name" is asking for the
	// operation's own relative resource name. If all we have is the bare id,
	// the url would expand to something like "v1/operation-1740000000000",
	// which addresses nothing: every poll 404s, and a 404 on a plausible url
	// is a worse diagnostic than a named error. So say what is missing.
	if placeholderNames(tmpl)["name"] && !addressesAnOperation(attrs["name"].Raw.(string)) {
		return "", "", fmt.Errorf("%s: %q needs the operation's own resource name, but the operation "+
			"reports only the id %q and no selfLink this type's api prefix can reduce",
			ty.Name, tmpl, name)
	}
	if zone := lastPathSegment(op["zone"]); zone != "" {
		attrs["zone"] = value.String(zone, value.SourceProvider)
	} else if p.settings.Zone != "" {
		attrs["zone"] = value.String(p.settings.Zone, value.SourceProvider)
	}
	if region := lastPathSegment(op["region"]); region != "" {
		attrs["region"] = value.String(region, value.SourceProvider)
	} else if p.settings.Region != "" {
		attrs["region"] = value.String(p.settings.Region, value.SourceProvider)
	}

	rel, err := ExpandURL(tmpl, attrs)
	if err != nil {
		return "", "", fmt.Errorf("%s: building the operation url: %w", ty.Name, err)
	}
	// APIBaseURL, never absURL: ty.PathPrefix has no business here. Both
	// templates are Discovery method paths taken verbatim, so each already
	// carries whatever version its own API puts in front of it, and
	// APIBaseURL + methodPath is Discovery's own identity. Measured over the
	// regenerated catalog: every type with a wait path is compute, whose
	// PathPrefix is empty (its servicePath carries the version), and every
	// type that falls back to the poll path -- container and sqladmin -- has
	// PathPrefix "v1/" AND a poll path already beginning "v1/", so adding the
	// prefix would send all 7 of them to a doubled ".../v1/v1/..." url. Same
	// reasoning as awaitLongRunning's own poll above.
	return method, ty.APIBaseURL + rel, nil
}

// operationResourcePath is what a compute-style operation template's "name"
// placeholder must be filled with: the operation's OWN relative resource
// name, taken from the selfLink the operation publishes for itself and
// reduced by this type's api prefix (reduceSelfLink, ids.go -- the same
// reduction a created resource's id goes through, not a hand-rolled slice).
//
// THIS IS WHERE THE TWO AWAIT STRATEGIES GENUINELY DIFFER, and the difference
// is not a style choice. For a google.longrunning.Operation, "name" IS the
// operation's full relative resource name, so awaitLongRunning fills its own
// "{+name}" from op["name"] and is correct to. A compute-style operation's
// "name" is something else entirely: compute, container and sqladmin all
// document it as the server-assigned ID -- "Output only. The server-assigned
// ID for the operation." (schemas/container.json), confirmed by container's
// own deprecated "operationId" parameter, described as "the server-assigned
// `name` of the operation". container's projects.locations.operations.get
// then demands its "name" parameter match
// "^projects/[^/]+/locations/[^/]+/operations/[^/]+$", which a bare id can
// never satisfy. Handing both strategies op["name"] gave gcp.container.cluster
// and gcp.container.nodepool a poll url of "v1/operation-1740000000000" and
// failed every GKE mutation at the polling step.
//
// Only 2 of the 65 compute-style types name "name" in their template at all
// (both container's); the other 63 fill {operation}/{project}/{zone}/{region}
// and never read this. All three APIs publish Operation.selfLink, so the
// reduction has something to work from in every case that needs it.
//
// The fallback to the bare id is deliberate but narrow: for the 63 templates
// that do not capture a path it is exactly right, and for the two that do,
// operationRequestURL refuses the url rather than sending a plausible one.
func operationResourcePath(ty *catalog.Type, op map[string]any) string {
	name, _ := op["name"].(string)
	if raw, _ := op["selfLink"].(string); raw != "" {
		if rel, err := reduceSelfLink(ty, raw); err == nil {
			return rel
		}
	}
	return name
}

// addressesAnOperation reports whether rel names an operation rather than
// merely being a string a template will happily swallow: a path of more than
// one segment, one of which is the literal "operations". That literal is the
// wire segment every operations collection in the corpus uses -- Discovery's
// "globalOperations"/"zoneOperations" are collection NAMES and never appear
// in a url (see operationRequestURL).
func addressesAnOperation(rel string) bool {
	segs := strings.Split(strings.Trim(rel, "/"), "/")
	if len(segs) < 2 {
		return false
	}
	return slices.Contains(segs, "operations")
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
// several -- their messages are ALL joined, since any of them may be the one
// that matters, but only the FIRST sub-error's code is kept (APIError.Code
// is a single field; a real multi-error compute failure is rare enough, and
// the joined message carries every sub-error's own text regardless, so
// nothing GCP said is lost -- only which single code represents a set of
// them is a simplification).
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
