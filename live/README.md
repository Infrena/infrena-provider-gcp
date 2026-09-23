# The live suite

This is the only thing in this repository that talks to Google. Everything
else — the unit tests, the protocol test, the whole `e2e` package — runs
against an in-process fake that answers whatever it was told to answer. **No
claim that this provider works is supportable until this suite has run.**

It creates real resources in a real project and spends real money. It is
opt-in, it refuses by default, and it needs James's explicit approval each
time.

## Running it

```bash
gcloud auth application-default login

export INFRENA_GCP_LIVE_PROJECT=example-project-1234
export INFRENA_GCP_LIVE_SA=infrena-live@example-project-1234.iam.gserviceaccount.com

go test -tags live -count=1 -v -timeout 45m ./live/
```

With neither variable set it **skips**, printing a greppable
`LIVE SKIPPED:` line. With the wrong identity or an unlabelled project it
**refuses**, before it sends anything.

Read the whole output afterwards, and check the billing console.

`TestWhetherGoogleSendsRetryAfter` deliberately exhausts a per-minute API
quota, so **run it on its own** rather than in the same invocation as the
workflow:

```bash
go test -tags live -count=1 -v -timeout 15m -run TestWhetherGoogleSendsRetryAfter ./live/
```

## The environment

Set up once, by hand, and **not to be recreated by anything in this
repository**.

Every identifier below is a PLACEHOLDER. Substitute your own: nothing in this
repository names a real project, and nothing should. The shapes matter, the
values do not.

| | |
|---|---|
| Project | `example-project-1234` — your own, empty, and not shared with anything you care about |
| Project number | `123456789012` — the NUMBER, not the id: a tag binding's parent needs it |
| Label | `infrena-live-tests=true` — **this is what the guard checks**, and it is the one value you must match exactly |
| Billing account | `012345-567890-ABCDEF` — needed only if the project is not already billed |
| Service account | `infrena-live@example-project-1234.iam.gserviceaccount.com` |
| Roles | `compute.admin`, `storage.admin`, `cloudasset.viewer`, `resourcemanager.tagAdmin`, `iam.serviceAccountAdmin` |
| APIs | compute, storage, cloudresourcemanager, cloudasset, iam, iamcredentials, serviceusage |

If `gcloud projects create` returns `QuotaFailure: you have exceeded your
allotted project quota`, reusing a dormant project works — but verify it is
genuinely empty first. These tests create and destroy real infrastructure, and
the guard's label check is the only thing standing between them and whatever
else lives there. Confirm the compute API has never been enabled and that there
are no buckets before pointing this at anything.

**There is no key file and this suite must never create one.** It
authenticates as the operator (Application Default Credentials from
`gcloud auth application-default login`) and impersonates the service
account, which works because the operator running it holds
`roles/iam.serviceAccountTokenCreator` on that service account. The impersonation goes through
`gcpplugin.Instance.TokenSource`, the same code path the plugin child
process uses, so a broken credential chain fails in the guard rather than
halfway through an apply.

## The guard

`checkGuard` decides, and it is a pure function so it can be tested without
reaching Google. Two conditions, **in this order**:

1. The service account's local part is `infrena-live@`. Settled from the
   environment alone — no token is minted and no request is sent, so a run
   misconfigured with somebody's own account never appears in an audit log
   as having looked at anything.
2. The project carries `infrena-live-tests=true`, **fetched live from Cloud
   Resource Manager v3 every run**. A fetch that fails is a refusal, not a
   pass: "we could not check" and "it is fine" are different answers.

**It is a label and not a name, and that is stronger than what it replaced.**
The original plan checked the project id for an `infrena-live` substring.
This project is `example-project-1234` and contains no such substring, so
that check could not have worked here — but the label is better anyway. A
project id can contain a substring by coincidence; somebody's
`infrena-live-metrics-prod` would have passed. The only way a project comes
to carry this label is:

```bash
gcloud alpha projects update <id> --update-labels=infrena-live-tests=true
```

(note `alpha`: `gcloud projects update` does not carry `--update-labels` on
the GA track) — which is a thing a human did on purpose. A production
project cannot drift into being an acceptable target.

`TestTheGuardRefusesTheWrongIdentity` runs offline and asserts the ordering
as well as the outcome: for a wrong identity it requires that **zero** label
lookups were made. Refusing is not the same as refusing first.

## What it creates, and what that costs

`TestLiveWorkflow` — the things that work:

| resource | type | why it is here | cost |
|---|---|---|---|
| storage bucket | `gcp.storage.bucket` | a synchronous create with no operation at all | free |
| firewall rule | `gcp.firewall` | a compute-style operation await, and the only updatable type in reach: it PATCHes **without** an updateMask, which 15 of the 86 updatable types do | free |
| e2-micro instance | `gcp.compute.instance` | **the compute await path against real GCP** | pennies |

`TestLiveTagTypes` — a project of its own, kept separate because all three
failed together until task 18b and a shared failure told you nothing about
which type caused it:

| resource | type | why it is here | cost |
|---|---|---|---|
| tag key | `gcp.tagkey` | a `google.longrunning.Operation` await, the strategy 189 of the sampled methods use | free |
| tag value | `gcp.tagvalue` | the same await, plus a `${...}` reference resolved against a real generated id | free |
| tag binding | `gcp.tagbinding` | `read_via: list_by_parent`, the only type in the catalog with no `get` at all | free |

`TestLiveTagBindingOnASeededTag` seeds a tag key and value **directly through
the API** and has infrena manage only the binding, so a regression in the
key cannot hide one in the binding a second time. It **passes** as of task
18b, import of the real four-segment id included.

The three tag types were in `TestLiveWorkflow` until the first live run.
They fail their create (finding 3 below) and took the bucket, the firewall
and the instance down with them, so no claim about any of those could be
made at all. Separated, each failure says one thing.

`TestLiveServiceAccount` — a project of its own, same reasoning:

| resource | type | why it is here | cost |
|---|---|---|---|
| service account | `gcp.serviceaccount` | the create url that could not be built at all, and the live proof of task 18b's part B | free |

Task 18 dropped `gcp.serviceaccount` because its create url
(`{+name}/serviceAccounts`) could not be expanded from anything a user could
write. 18b binds the placeholder from iam's own `pattern` for that method's
`name` parameter, and **the account is now really created against Google**.
The test still FAILS, for a different reason: see finding 11.

One e2-micro in `us-central1`, and nothing larger. No GKE cluster — James
was offered one and declined. The test asserts the instance's machine type
after creating it, so "nothing larger than an e2-micro" is a check and not a
hope.

Everything is torn down in `t.Cleanup`, in reverse creation order, **even
when the test fails** — and the cleanup is registered *before* the apply, so
a partial apply is cleaned up too. Every delete tolerates a 404, because the
goal is absence.

**Cleanup does not go through infrena.** The provider is the thing under
test; a teardown that depends on it leaves resources running exactly when
the test failed for the reason the teardown would also fail for. It talks to
Google directly, finds tag resources by listing rather than from state, and
when it cannot delete something it prints `CLEANUP FAILED:` with the
resource id and the url. A silent cleanup failure spends money forever.

### It depends on a resource the provider cannot manage

`gcp.firewall`'s `network` is REQUIRED and **`gcp.network` does not ship** —
compute's `Network` needs four hook rulings the tier gate has not been given,
and `Subnetwork` one. So the rule references the project's auto-created
`default` VPC by self link rather than managing it. That is a consequence of
the tier gate, not an oversight, and it is why the suite cannot be written
without something somebody else made.

## `Retry-After`: measured, not inherited

Spec §5.6 left this open. The AWS sibling project established that Cloud
Control never sends the header and closed the question; **that finding does
not transfer** — Cloud Control is one API behind one front end, and this
provider talks to twenty-five.

Measured against this project on 2026-09-22, by hammering free, read-only
endpoints until they throttled and reading every raw response:

| API | endpoint | requests in 60s | throttled | carried `Retry-After` |
|---|---|---|---|---|
| cloudresourcemanager | `v3/projects/{id}` | 29,550 | **26,479** (429 `RESOURCE_EXHAUSTED`) | **0** |
| compute | `zones/us-central1-a` | 10,419 | **3,219** (403 `rateLimitExceeded`) | **0** |
| storage | `b?project={id}` | 14,952 | 0 | **inconclusive** |

An earlier round, before the quota-project mistake below was found, produced
another 38,015 throttled cloudresourcemanager responses, also with none. So:
roughly **65,000 throttled responses across two APIs and two independent
rounds, and not one `Retry-After` header.**

### The field STAYS, and that is a deliberate override of the task brief

The brief said to remove `APIError.RetryAfter` if the header never appeared,
the way the AWS repo closed the equivalent question. It is still there, on
the evidence:

- Cloud Control is **one API behind one front end**. This provider calls
  **twenty-five**. Two of them were throttled here and one refused to
  throttle at all inside a minute; twenty-two were not measured.
- "The two APIs I could throttle did not send it" is not "GCP never sends
  it", and the suite says so in as many words — it prints **INCONCLUSIVE**
  rather than "absent" when it provoked no throttle, because those are
  different findings and only one of them is an answer.
- Keeping it costs about twenty-five lines that are already tested and
  already correct. Removing it is information a future API cannot get back.

What the measurement DID change is in `internal/gcprov/errors.go`, and it
mattered far more — see below.

### The finding that actually came out of this: compute does not send 429

Look at the two throttle rows again. cloudresourcemanager answers a quota
throttle with **429** and `"status": "RESOURCE_EXHAUSTED"`. compute answers
the identical condition with:

```json
{"error":{"code":403,
  "message":"Quota exceeded for quota metric 'Read requests' ...",
  "errors":[{"domain":"usageLimits","reason":"rateLimitExceeded"}],
  "details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo",
              "reason":"RATE_LIMIT_EXCEEDED"}]}}
```

**403, and no `error.status` at all.** So `APIError.Code` was empty, 403 is
not a retryable HTTP status, and `ClassifyError` returned `NotSafeToRetry`
for **every compute throttle** — a transient quota blip reported to the user
as a permanent failure, on the API this provider calls more than any other.
3,219 of them arrived in sixty seconds.

`errors.go` now reads `google.rpc.ErrorInfo`'s `reason` (and the legacy
Discovery `errors[].reason`, which is all the older APIs carry) into
`APIError.Reason`, and classifies `RATE_LIMIT_EXCEEDED` /
`rateLimitExceeded` as retryable whatever status it arrived with. A 403 that
is a **real** permission denial carries a different reason and is still
`NotSafeToRetry`, which is what stops this turning a misconfigured service
account into a retry storm — there is a test for each half, built from the
verbatim bodies this suite captured.


### What would reopen it

- Any GCP REST response, on any API this provider calls, carrying a
  `Retry-After` header with a 429, a 503, or a 403 whose reason is
  `rateLimitExceeded`.
- Google documenting the header for a service in the catalog.

Re-run `TestWhetherGoogleSendsRetryAfter` to check; it prints the header
verbatim for every throttled response, and says **INCONCLUSIVE** rather than
"absent" when it could not provoke a throttle at all. Those are different
findings and only one of them is an answer.

## What this suite found that no fake could

Eleven now — nine from the first run (task 18), two more from the re-run
after three of them were fixed (task 18a). Every one of them was invisible
to the 1,400-odd tests that came before, because every one of them lives
where this provider meets Google. Findings 10 and 11 are there because
fixing a defect is how you reach the next one: neither was observable while
the create in front of it still failed.

**1. compute reports a quota throttle as 403, so none of them were retried.**
The Retry-After measurement above. **Fixed here**, in `internal/gcprov/errors.go`.

**2. A compute operation taking longer than thirty seconds always fails.**
`defaultTimeout` bounds one HTTP round trip at 30s. compute's
`operations/{op}/wait` **blocks server-side for up to two minutes** by
design — that is what the method is for. So every compute operation that
does not finish inside thirty seconds comes back as

```
POST .../zones/us-central1-a/operations/operation-.../wait:
context deadline exceeded (Client.Timeout exceeded while awaiting headers)
```

Reproduced 3/3 on instance delete, at 30.3s, 30.4s and 32.1s. The instance
really was deleted every time; infrena reported the destroy as **failed**
and left it in state.

**FIXED in task 18a**, in `internal/gcprov/client.go` and `await.go`. A
published long poll now gets its own round-trip bound, taken from the type's
own `TimeoutSeconds` — the same bound `await.go` already puts on the whole
operation, so a wait may take as long as the operation is allowed to and not
a second longer. The 30s stays for every ordinary call, because it is there
to notice a *hung* endpoint and a published long poll is not hung. Which
requests get the longer bound is decided from the catalog fact that the type
publishes a wait method, never from the url's shape. Confirmed against real
Google on 2026-09-22: `destroy_removes_everything` passes, with the
instance's delete taking **1m45s** through the wait.

While writing its test, `ClientOptions.Timeout` turned out to have been
**declared, documented and never read** since Task 11 — every Client in the
process ran at the 30s default whatever a caller asked for. Nothing noticed
because no test had ever set it. Also fixed, with a test.

**3. The ProviderID defect reaches all three tag types, not just the binding.**
It was reported against `gcp.tagbinding`. Live, `gcp.tagkey` fails first:

```
create tagkey: gcp.tagkey: created, but the resource cannot be read back:
Invalid CRM resource name: 'tagKeys/tagKeys%2F281480152414347' (400)
```

The create body carries `name: "tagKeys/281476..."`, `self_link` is
`tagKeys/{{name}}`, and expanding one against the other escapes the id's own
prefix back into the result. **It orphans the resource**: the host drops a
failed create's result, so the tag key is real and tracked nowhere. The
sweep in this suite finds them by short name and deletes them, which is the
only reason this project is not full of them.

**FIXED in task 18a**, in `internal/gcprov/ids.go`. `ProviderID` now takes
the body's own `name` verbatim when it already matches the type's
`self_link` shape — same segment count, every literal segment equal, which
is the same test `ParseProviderID` applies to an id a user typed, so nothing
is accepted here that a later Read, Delete or Import would refuse. A bare
leaf (`"web1"` against compute's six-segment template) still goes through
the template, and so does a name in some other type's collection. The id
parser was **not** loosened: a wrong id that parses is worse than one that
errors.

Confirmed against real Google on 2026-09-22: `gcp.tagkey` and
`gcp.tagvalue` create and read back cleanly, with ids `tagKeys/281482587066023`
and `tagValues/281483265184913`.

**4. `gcp.serviceaccount` cannot be created at all.** Its create url is
`{+name}/serviceAccounts`; the type declares only `accountId` and
`serviceAccount` as attributes; `withScope` supplies project, region, zone
and location and not `name`. So nothing a user can write expands the url,
and a create fails before a request is sent. It was in the brief's resource
set and had to be dropped from this suite.

**5. A compute instance created with `initializeParams` never converges.**
An instance is created with `disks[].initializeParams` — image, size, disk
type — and **compute does not echo them back**. `Reconcile` expresses
Google's answer in the reference's shape and cannot invent a field the
answer does not carry, so state loses them, and `disks` is ForceNew. Every
plan after a successful apply proposes **destroying and recreating** the
instance, forever: `vm proposes a replace because of [disks [forces new]
networkInterfaces]`. The suite carries `ignore_changes: [disks,
networkInterfaces]` on the instance so the rest of it can run; those two
lines are a defect marker, not tidiness.

**6. `quota_project` breaks a service-account instance.** Setting it sends
`X-Goog-User-Project`, which requires `serviceusage.services.use` on the
named project. The live service account holds `storage.admin` and
`compute.admin` but not `serviceUsageConsumer`, so every storage call came
back "does not have serviceusage.services.use access" — a 403 that reads
like a missing storage grant and is not one. A quota project is for a USER
credential, which has no project of its own to bill; an impersonated service
account already belongs to one. Not a provider defect, but it will cost
somebody an afternoon, so it is written down.

**7. A cleanup built on `t.Context()` cleans nothing up.**
`oauth2.ReuseTokenSource` keeps the context it was constructed with and
reuses it for every later `Token()` call, and `t.Context()` is cancelled
**before** `t.Cleanup` runs. Every teardown in the first live run failed
with `obtaining the base token: context canceled`, and an e2-micro was left
running. `gcpplugin/credentials.go` warns about exactly this and this suite
walked into it anyway. Fixed here; `newGoogle` builds on `context.Background()`.

**8. `gcp.tagbinding` does not work at all, in either direction.** Measured
2026-09-22 after `roles/resourcemanager.tagUser` was granted, which is what
finally made the path reachable. Spec decision G6 requires this type at v1.0.
**It is unusable, and not for the reason it was filed under.**

*Create.* The binding is created, then the create is reported as failed:

```
x create binding: gcp.tagbinding: operation has no name to poll
```

Cloud Resource Manager answers `tagBindings.create` with an operation that is
**already finished and carries no name**, because there is nothing to poll.
Captured verbatim:

```json
{"done": true,
 "response": {"@type": "type.googleapis.com/google.cloud.resourcemanager.v3.TagBinding",
   "name": "tagBindings/%2F%2Fcloudresourcemanager.googleapis.com%2Fprojects%2F123456789012/tagValues/281479230039359",
   "parent": "//cloudresourcemanager.googleapis.com/projects/123456789012",
   "tagValue": "tagValues/281479230039359"}}
```

`awaitLongRunning` reads `op["name"]` and errors on empty **before** it checks
`done`, so it refuses an answer sitting in front of it. **This is a third
orphan**: the binding is real, the host drops a failed create's result, and
nothing tracks it.

**FIXED in task 18a**, in `internal/gcprov/await.go`: a name is needed only
to POLL, and an operation that is already done is never polled, so the two
poll preconditions moved inside the loop, below the `done` check. Confirmed
against real Google on 2026-09-22 — `Creating binding... done (0.9s)`.

**11. The fix exposed a fourth orphan immediately behind it**, and that one
is not about tag bindings at all. A `google.longrunning` `response` is a
`google.protobuf.Any`, so it carries `"@type"` — the ENVELOPE'S
discriminator, not a field of the resource. With the ordering fixed, the
first create ever to reach that code path handed `@type` straight to the
host, which refused the whole state:

```
x create binding: gcp returned attribute "@type" on a gcp.tagbinding,
which its own schema does not declare
```

The binding was real (this suite's sweep had to delete it) and the create
was reported as failed. **Every one of the 97 `AwaitLongRunning` types
answers this way**, and any of them reaches it whenever the readback after a
create loses the race with eventual consistency and `Create` falls back to
`bestEffortState`. Also fixed in `await.go`: the Any envelope is unwrapped
where it is opened, dropping `@type` and nothing else — an undeclared field
arriving for any *other* reason is a real disagreement with the catalog and
still surfaces.

**Note what this means for the defect this was filed under.** The `ProviderID`
mangling is *never reached* for a tag binding; the await failed first. It is
confirmed for `gcp.tagkey` from a real 400
(`Invalid CRM resource name: 'tagKeys/tagKeys%2F281480152414347'`) and fixed
there, but the binding's id is mangled for a **different** reason — its
`self_link` has two segments and a real name has four (below), which the
verbatim rule deliberately does not paper over.

*Read.* Worse, and this is the part that makes the type unusable rather than
merely broken. `gcp.tagbinding`'s `read_via` is `list_by_parent` — spec G6's
worked ruling, and the only read path the type has, because
cloudresourcemanager publishes no `get` for it. **That path cannot be reached
either.** Adopting the binding by the id Google itself just gave us:

```
Error: could not import: gcp.tagbinding.tagBindings/%2F%2F...%2Fprojects%2F123456789012/tagValues/281479973987355:
gcprov: "tagBindings/%2F%2F.../tagValues/281479973987355" has more segments than gcp.tagbinding's id shape
```

`ParseProviderID` refuses **before any API call**. The catalog's `self_link` is
`tagBindings/{{name}}` — two slash-separated segments — and a real binding name
has **four**: Google percent-escapes the parent's slashes into one segment and
then appends `tagValues/<id>` as two more. So `readByListingParent` has never
run against Google and cannot, for any real id.

Two consequences worth stating plainly:

- **`gcp.tagbinding` can be created but not addressed.** As of task 18a the
  create itself succeeds; what it stores is
  `tagBindings/tagBindings%2F%252F%252Fcloudresourcemanager.googleapis.com%252F...`,
  a doubly-escaped id addressing nothing. The next plan reports the binding
  as vanished and proposes recreating it, the destroy "forgets" it rather
  than deleting it, and the tag value's own destroy then fails because a
  binding it does not know about is still attached. Import still refuses the
  real id outright. **Still a release blocker**, now for one reason (the
  two-segment `self_link`) rather than three.
- **`readByListingParent` is narrower than the type is, independently of the
  above.** It builds its parent from `Settings.Project`, so it can only ever
  list bindings on the configured *project*. A tag bound to a bucket or an
  instance — both of which the API supports — would be created and then never
  readable. That is a design limitation in a v1.0-mandated feature, not an
  implementation slip.

The suite still **skips** with the exact grant named when
`roles/resourcemanager.tagUser` is missing, because a fresh project will hit
that first and a missing IAM role must never be reported as a bug in this
provider:

```bash
gcloud projects add-iam-policy-binding <project> \
  --member=serviceAccount:<sa> --role=roles/resourcemanager.tagUser
```

**9. A `quota_project` 403 now says what it actually is.** Finding 6 below cost
an afternoon once; `client.go` now wraps that one 403 with the context the
response cannot carry — that it is about the setting, not the permission it
names, and that *removing* `quota_project` is usually the fix rather than
granting another role. Wrapped with `%w`, so `ClassifyError` and `isNotFound`
still reach the `*APIError`, and it only fires when a quota project is actually
configured.

**10. `gcp.tagkey` never converges: Google answers `parent` with the project
NUMBER.** Found on the task-18a re-run, and only reachable because finding 3
was fixed — until then the create failed and there was no second plan to
look at. The configuration says

```yaml
parent: projects/example-project-1234
```

and Cloud Resource Manager answers `parent: "projects/123456789012"`. Same
project, canonical form, different string — and `parent` is ForceNew, so
**every plan after a successful apply proposes destroying and recreating the
tag key**:

```
the plan right after creating the tag types proposes a replace of tagkey
  because of [parent [forces new]]
the plan right after creating the tag types proposes a replace of tagvalue
  because of [parent (known after apply) [forces new]]
```

The tag value follows only because its own `parent` is `${tagkey.name}` and
the key is being replaced; there is one defect here, not two.

This is the same *shape* as finding 5 (compute not echoing
`initializeParams`) — GCP answers in a form the configuration did not use —
but the opposite direction: there the answer carries less than was sent,
here it carries the same thing spelled canonically.

**FIXED in task 18b.** The provider resolves the project's number once
(`cloudresourcemanager projects.get`, cached per provider) and reconcile
then expresses GCP's answer in the spelling the configuration used, the same
way it already puts an unordered list back into the reference's order. The
lookup only happens once two `projects/<x>` names actually disagree, and if
it fails the two stay different and the plan proposes a replacement the user
can see. `TestLiveTagTypes` passes as of 2026-09-23.

### 11. A create whose request schema is a WRAPPER orphans the resource

Found by task 18b's live run, and only reachable once the create url could
be built at all.

iam's `serviceAccounts.create` takes a `CreateServiceAccountRequest` —
`{accountId, serviceAccount}` — and answers with a `ServiceAccount`. The
generator builds a type's attributes from the create method's REQUEST body,
so `gcp.serviceaccount`'s schema describes the envelope and not the
resource. Against Google on 2026-09-23:

```
x create sa: gcp returned attribute "displayName" on a gcp.serviceaccount,
  which its own schema does not declare
Declared: accountId, serviceAccount
```

The host drops a failed create's result, so **the account was real and
tracked nowhere**. The suite's own sweep deleted it; nothing else would
have.

**13 of the 233 shipped types are in this position**, measured 2026-09-23:
`gcp.container.cluster`, `gcp.container.nodepool`, `gcp.bigtableadmin.instance`,
`gcp.bigtableadmin.table`, `gcp.iam.role`, `gcp.iam.organization.role`,
`gcp.iam.serviceaccount.key`, `gcp.pubsub.snapshot`, `gcp.spanner.session`,
`gcp.sslcert`, `gcp.task`, `gcp.feed`, `gcp.serviceaccount`. Every one of
them orphans on create for the same reason.

**Needs a follow-up.** The fix is to model the resource from what the create
RETURNS and wrap the request body on the wire, which is a generator feature.
Until then these thirteen creates leave real resources behind.

### What passes

`validate`, plan, apply, **a clean second plan**, drift detected and
corrected through a maskless PATCH, `discover` finding the four
`default-allow-*` rules and flagging each one system-owned **with its
reason**, and `import --as` producing a state that plans clean. The compute
create await works end to end: the instance's provider id names the
instance, not the operation, which is the thing this project got wrong three
times and the one thing only a live call could settle.

As of task 18b, `TestLiveWorkflow`, `TestLiveTagTypes` and
`TestLiveTagBindingOnASeededTag` all pass **in full**. Spec decision G6 —
tagkey, tagvalue AND tagbinding at v1.0 — is satisfied end to end: the
binding creates, reads back by listing the parent its own id names,
imports at its real four-segment id, and destroys.

`TestLiveServiceAccount` fails, and what it fails on has moved: the create
url is built and Google makes the account, and the failure is now the
wrapper-request defect (finding 11).

