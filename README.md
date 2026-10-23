# gateway

An OpenAI-compatible gateway in pure Go (`CGO_ENABLED=0`) that sits in front of
[vLLM](https://docs.vllm.ai) on a GPU cluster shared by several institutions.
It replaces LiteLLM for that role and adds the one thing no open-source gateway
provides: **fair-share scheduling with reservation and borrowing**, mapped onto
vLLM's own priority scheduler.

Built for a 4-node HGX B300 cluster (32 GPUs) shared by two institutions, A
(81.25 %, 26 GPUs) and B (18.75 %, 6 GPUs). vLLM stays the inference engine.

- each institution is guaranteed its share of the pool's concurrency;
- capacity the other institution does not use is borrowed, never left idle;
- when the owner comes back, vLLM preempts the borrowers, not the owner;
- organisation → team → project → key, like the Slurm account tree, with
  delegated administration (proxy, organisation and team admins, members,
  viewers) and team-managed model lists;
- virtual keys (hashed), per-key RPM / TPM / in-flight limits, budgets in
  GPU-hours on every scope (organisation, team, project, key, tag, per model),
  temporary increases and soft-budget alerts (webhook, e-mail);
- per-request accounting in PostgreSQL and spend reports (JSON, CSV, and
  pipe-delimited like `sreport --parsable2`) to reconcile with Slurm;
- Prometheus metrics; streaming pass-through without buffering.

```sh
go install github.com/go-llmflow/gateway/cmd/llmflow-gateway@latest
llmflow-gateway check-config -c examples/gateway.hcl   # validates the file and every secret file it names
llmflow-gateway serve -c examples/gateway.hcl
llmflow-gateway export -c examples/gateway.hcl --from 2026-10-01 --to 2026-11-01 > october.csv
llmflow-gateway export -c examples/gateway.hcl --from 2026-10-01 --to 2026-11-01 \
  --group-by org,team,project --format parsable2   # side by side with sreport --parsable2
```

## Architecture

```
clients (OpenAI SDKs, Authorization: Bearer llmf-…)
   │
   ▼
llmflow-gateway
   1 auth: virtual key → team (SHA-256, constant-time, cached)
   2 body ≤ max_body_bytes, JSON re-encoded, client priority fields stripped
   3 budget (GPU-hours per day / week / month), fail-closed by default
   4 per-key RPM / TPM / max in-flight
   5 fair share: within reservation → priority 0/1, borrowing → 10 and more
   6 route: least in-flight healthy upstream, retry before the first byte
   7 stream SSE event by event, capture usage
   8 cost → budget counter, fair-share usage, async PostgreSQL record
   │
   ▼  headers built from scratch: Authorization (internal key), X-Vllm-Priority
vLLM × N  (--scheduling-policy priority --api-key …, reachable only from the gateway)
```

Packages: `fairshare` (the algorithm, no I/O), `openai` (request rewrite, SSE
usage capture), `store` (memory and PostgreSQL), `gateway` (HTTP server),
`config`, `cmd/llmflow-gateway` (cobra CLI).

## The fair-share algorithm

### What vLLM gives us

vLLM accepts an integer `priority` per request, in the JSON body or in the
`X-Vllm-Priority` header, which overrides the body; lower runs earlier; non-zero
priorities need `--scheduling-policy priority`
([OpenAI-compatible server docs](https://docs.vllm.ai/en/latest/serving/online_serving/openai_compatible_server/)).
In the scheduler (`vllm/v1/core/sched/scheduler.py`, `request_queue.py`) the
waiting queue is a min-heap on `(priority, arrival_time)`; when a running
request runs out of KV blocks, the running request with the largest
`(priority, arrival_time)` is preempted and re-queued; a waiting request never
preempts a running one.

So the order of service and the choice of victim are both decided by the
priority number. The gateway's job is to choose that number.

### Reservation with borrowing

Each pool has a capacity **C**: the concurrency it sustains without preemption,
measured externally. Each organisation (institution) owns an integer
reservation, shared by all its teams and projects, obtained by
apportioning C over the shares with the largest-remainder (Hamilton) method:
`floor(share × C)` each, then the slots left up to `floor(Σ share × C)` to the
largest fractional parts. Reservations never sum to more than C (C = 256 gives
A 208 and B 48; C = 10 gives 8 and 2).

For every request:

| situation | priority sent to vLLM |
|---|---|
| the team has fewer within-share requests in flight than its reservation | `0` interactive, `1` batch |
| otherwise (borrowing) | `10 + 10 × rank + class`, rank ≤ 9 |

A within-share request is therefore admitted before any borrower and is never
the victim while a borrower runs. Borrowers fill whatever is idle.

The test counts within-share requests in flight, not all requests: a team whose
clients keep more requests in flight than its reservation would otherwise never
drop below it again, and everything it sends would borrow (the simulator showed
B getting 4 % instead of 18.75 % with that rule).

### Ordering borrowers: Slurm Fair Tree

Among borrowers, the rank is the dense rank of the team's **level fairshare**
`LF = S / U` ([Slurm Fair Tree](https://slurm.schedmd.com/fair_tree.html)):
`S` is the team's share normalised over the pool's teams, `U` its usage
normalised the same way. The most under-served team (largest LF, `+Inf` with no
usage) has rank 0; teams with equal LF share a rank, as sibling users do in Fair
Tree. The most over-served borrower thus gets the largest number: admitted last,
preempted first.

Usage is the **cost actually consumed** (GPU-hours, or millions of tokens when
no price is configured), charged when a request finishes and decayed with a
half-life like Slurm's `PriorityDecayHalfLife` (default 7 days,
[multifactor priority](https://slurm.schedmd.com/priority_multifactor.html)).
Slurm's code (`src/plugins/priority/multifactor/priority_multifactor.c`)
multiplies usage by `(1 - 0.693/half_life)^Δt`; the gateway uses the exact
`exp(-ln2·Δt/half_life)`, of which Slurm's factor is the first-order expansion.
Usage is not charged from in-flight time: a request waiting in vLLM's queue
consumes nothing, and charging it made a flooding team look as consumed as the
team it starved.

### What the simulator shows

`fairshare/sim_test.go` simulates vLLM's scheduler (waiting heap, KV blocks,
preemption of the largest `(priority, arrival)`) with closed-loop clients:

| scenario (C = 16, 64 clients per team) | result |
|---|---|
| A alone / B alone | KV utilisation 0.98 / 0.97; 40.5 / 39.0 requests running for reservations of 13 / 3 |
| both saturating, B arriving after A filled the pool | running fraction A 0.789, B 0.211 (shares 0.8125 / 0.1875) |
| preemptions | 24 614, every one a borrower |
| control: every request at priority 0 (no gateway priorities) | B gets 0.498, its fraction of clients, not its share |
| C = 10 (reservations 8 / 2) | A never holds more than 8 within-share requests; B's guaranteed requests never queue behind more than 8 of A's |

## Configuration

The configuration is written in [HCL2 native syntax](https://github.com/hashicorp/hcl/blob/main/hclsyntax/spec.md).
See [`examples/gateway.hcl`](examples/gateway.hcl) (parsed by the test suite,
so it stays valid):

```hcl
master_key_file     = "/etc/llmflow/master.key"
cache_salt_key_file = "/etc/llmflow/cache-salt.key" # per-institution prefix-cache salt

org "etabA" {
  share = 0.8125
  team "lab-nlp" {}
  team "lab-vision" {}
}
org "etabB" {
  share = 0.1875
  team "lab-chem" {}
}

pool "b300" { capacity = 256 }

model "llama-3.3-70b" {
  upstream_model = "meta-llama/Llama-3.3-70B-Instruct"
  pool           = "b300"
  class          = "interactive"
  key_file       = "/etc/llmflow/vllm.key"
  cost {
    input_per_mtok  = 0.02
    output_per_mtok = 0.20
    cached_per_mtok = 0.002
  }
  upstream "gpu01" { base_url = "http://10.0.0.11:8000" }
  upstream "gpu02" {
    base_url = "http://10.0.0.12:8000"
    key_file = "/etc/llmflow/gpu02.key"
  }
}
```

| block / attribute | meaning |
|---|---|
| `org "<name>" { share, team "<name>" {} … }` | an organisation (institution) with the fraction of every pool reserved for it (sum ≤ 1) and the teams (labs) declared under it; team names are global |
| `pool "<name>" { capacity }` | C, sustainable concurrency of the pool (1..1048576) |
| `model "<name>" { … }` | client model name → `upstream_model`, `pool`, `class` (`interactive` / `batch`), `key_file`, `cost { input_per_mtok, output_per_mtok, cached_per_mtok }` in GPU-hours per million tokens, and `upstream "<name>" { base_url, key_file }` blocks |
| `store { … }` | `kind` (`memory` / `postgres`), `dsn_file`, `fail_closed` (default true), `queue_size`, `batch_size` |
| `fairshare { half_life }` | usage decay half-life (default `"168h"`) |
| `limits { … }`, `health { … }` | body and header size, timeouts, maximum request duration and response size; health-check interval, timeout, cooldown, path |
| `tags { header, allowed }` | the request header clients may tag requests with, and the allowed tags |
| `cache_isolation`, `cache_salt_key_file` | prefix-cache tenant scope (`org` default, `team`, `key`, `none`) and the file holding the HMAC key (at least 32 bytes, required unless `none`); see Security |
| `alerts { webhook_url, webhook_token_file, smtp { server, from, to, username, password_file } }` | where soft-budget alerts go |

HCL design decisions:

- **Static evaluation.** The file is decoded with no evaluation context: no
  variables and no functions. `env.HOME`, `file("/etc/shadow")` or `"${x}"`
  are errors ("Variables not allowed", "Function calls not allowed"), so the
  configuration cannot read the environment or other files. Constant
  expressions (`0.5 + 0.25`, heredocs) still work.
- **Quoted strings are templates.** A literal `${` must be written `$${` and a
  literal `%{` must be written `%%{` (a lone `$` or `%` needs nothing). This
  applies to every value, URLs, paths and model names included; tests cover
  both the refusal and the escapes.
- **Labelled blocks** for the things that have names: `org`, `team`, `pool`,
  `model`, `upstream`. Duplicate labels are errors. Teams nest inside their
  organisation, mirroring the Slurm account tree.
- **Every diagnostic carries `file:line:column`**, from the parser, the
  decoder (unknown attributes, wrong types) and the semantic checks (share sum,
  capacity bounds, unknown pool, invalid URL, durations). `check-config` prints
  them one per line.
- **Secrets only by file.** `master_key_file`, `metrics_token_file`,
  `store.dsn_file`, and `key_file` on a model (default for its upstreams) or on
  an upstream (override). An upstream URL with credentials is rejected. Nothing
  is read from the environment or the command line.
- **Durations are strings** in Go syntax (`"30s"`, `"168h"`), positive.
- **Absent and zero differ**: `retries = 0` disables retries; leaving it out
  takes the default (2).
- The parser is fuzzed (`FuzzParse`, corpus in `config/testdata/fuzz`): the
  configuration is treated as untrusted input.

Cost of a request: `((prompt − cached) × input + cached × cached_price +
completion × output) / 10⁶` GPU-hours, from the `usage` vLLM reports. For
streams the gateway sets `stream_options.include_usage` and removes the usage
chunk again if the client did not ask for it.

## Budgets

A budget caps the GPU-hours of one scope per `day`, `week` or `month` (UTC
calendar), optionally for one model group only:

```json
{"kind": "team", "name": "lab-nlp", "model": "", "period": "month", "max_cost": 500, "soft_limit": 400}
```

| `kind` | `name` | set by |
|---|---|---|
| `org` | organisation | proxy admin |
| `team` | team | the organisation's admins |
| `project` | `team/project` | the team's admins |
| `key` | key ID (with `model`: a per-model budget of that key) | the team's admins |
| `tag` | tag | proxy admin |

A request is refused (429, the message names the scope) as soon as one
budget it falls under is exhausted: its organisation, team, project, key and
every tag it carries, each for all models and for its own model. Tags come
from the key (`"tags"` at key creation) and from one allow-listed request
header (`tags { header, allowed }` in the configuration; at most 8, others
refused with 400).

`soft_limit` triggers one alert per budget and period, to a webhook (JSON
POST, optional bearer token from a file) and/or by e-mail through an SMTP
relay (`alerts { … }`); the "sent" mark is stored with the budget, so several
gateways and restarts send it once. A temporary increase raises the limit
until its expiry, whatever the period; setting the budget again keeps it.

Spend is the stored records of the period plus what this gateway charged
since: a request counts as pending until its record is stored, then as
written until the next refresh query includes it.

## Admin API

Admin routes take `Authorization: Bearer <master key>` or a user's admin
token (`llma-…`, 256 random bits, stored as SHA-256, shown once). Users hold
roles on scopes that nest like the Slurm account tree (a role on an
organisation covers its teams, a role on a team covers its projects and keys):

| role | scope | may |
|---|---|---|
| `proxy_admin` | everything | everything the master key may |
| `proxy_viewer` | everything | read every listing, report and fair-share state |
| `org_admin` | one organisation | create and disable its teams, set their budgets, manage their projects and keys, create users, grant `org_admin` / `viewer` on the organisation and `team_admin` / `member` / `viewer` on its teams |
| `team_admin` | one team | manage its projects and keys, grant `member` / `viewer` on the team |
| `member` | one team | create keys (owned by the member), list and revoke their own keys |
| `viewer` | one organisation or team | read its teams, projects, keys (no secrets), budgets and spend |

Nobody can grant a role above their own scope: an `org_admin` cannot grant
proxy roles or roles in another organisation. Listings and reports are
filtered to what the caller may see; other writes answer 403. Failed admin
authentications count in the per-network failure limit.

| route | |
|---|---|
| `GET /admin/whoami` | the caller and their roles |
| `GET` / `POST /admin/users`, `DELETE /admin/users/{name}` | list (filtered), create `{"name"}`, disable (revokes their tokens; proxy admin only) |
| `GET` / `POST /admin/users/{name}/tokens`, `DELETE /admin/users/{name}/tokens/{id}` | a user's admin tokens (the user themselves or a proxy admin) |
| `GET` / `PUT /admin/users/{name}/roles`, `DELETE /admin/users/{name}/roles?role=&org=&team=` | list, grant `{"role","org","team"}`, remove |
| `GET` / `POST /admin/orgs`, `DELETE /admin/orgs/{name}` | list (with configured share), create `{"name"}` (an organisation created here has no share: it only borrows), disable (its teams, projects and keys too) |
| `GET /admin/teams?org=`, `POST /admin/teams`, `DELETE /admin/teams/{name}` | list, create `{"name","org"}`, disable (its projects and keys too) |
| `GET` / `POST /admin/teams/{team}/projects`, `DELETE /admin/teams/{team}/projects/{name}` | list, create `{"name"}`, disable (revokes its keys) |
| `GET` / `PUT /admin/teams/{team}/models` | the model groups the team may use: `{"models":[…]}` (team admin and above; empty list = every configured model). A hidden model answers 404 like an unknown one and is left out of `/v1/models` |
| `GET /admin/keys?team=`, `POST /admin/keys`, `DELETE /admin/keys/{id}` | list (no secrets), create `{"team","project","name","rpm","tpm","max_inflight"}` (`project` optional; the `llmf-…` secret is returned once), revoke |
| `GET /admin/budgets`, `PUT /admin/budgets`, `DELETE /admin/budgets?kind=&name=&model=` | budgets on any scope (see below), listed with their current `spent` and effective `limit` |
| `POST /admin/budgets/increase` | temporary increase `{"kind","name","model","amount","expires_at"}` (RFC 3339, within a year) |
| `GET /admin/spend?from=&to=&group_by=&format=` | spend report: requests, prompt / completion / cached tokens, GPU-hours and estimated requests, grouped by any of `org`, `team`, `project`, `key`, `model`, `pool`, `tag` (default `org,team`), as `json`, `csv` or `parsable2` (pipe-delimited like `sreport --parsable2`). Filtered to what the caller may see: an org admin sees their organisation, a member their own keys. A request with several tags counts once under each tag |
| `GET /admin/usage.csv?from=&to=` | the v0.1 export: `group_by=team,pool`, CSV |
| `GET /admin/fairshare` | reservations, in-flight, borrowing, usage, LF and rank per pool and organisation |

Client routes: `POST /v1/chat/completions`, `/v1/completions`,
`/v1/embeddings`, `GET /v1/models`. `GET /healthz` returns 503 when the budget
state is stale and fail-closed is on.

## Metrics

`GET /metrics` (Prometheus text format 0.0.4, optional bearer token):
`llmflow_inflight`, `llmflow_inflight_borrowing`, `llmflow_borrowed_fraction`,
`llmflow_reserved`, `llmflow_inflight_tokens`, `llmflow_fairshare_usage`,
`llmflow_fairshare_level` (per pool and organisation, label `org`); `llmflow_upstream_healthy`,
`llmflow_upstream_inflight`; `llmflow_requests_total`, `llmflow_tokens_total`,
`llmflow_gpu_hours_total`, `llmflow_denied_total`,
`llmflow_upstream_retries_total`; histograms `llmflow_overhead_seconds` and
`llmflow_upstream_seconds`; accounting queue length and written / failed /
dropped records.

## Why not LiteLLM, and what was measured

Measured on 2026-10-23 with LiteLLM 1.106.2 against a recording fake upstream:

- a client sending `extra_body: {"priority": 0}` **overrides** a priority pinned
  in the deployment's parameters: any user can promote itself;
- a pinned header wins only while client header forwarding is off;
- LiteLLM's own overhead header (`x-litellm-overhead-duration-ms`) read
  3.2–4.9 ms per request on an upstream answering instantly.

LiteLLM's reservation with borrowing is Enterprise-only ("Reserving TPM/RPM on
keys based on priority requires a LiteLLM Enterprise license",
[Dynamic TPM/RPM Allocation](https://docs.litellm.ai/docs/proxy/dynamic_rate_limit))
and limits TPM/RPM rather than GPU concurrency. We found no open-source gateway
(Bifrost, APISIX, Envoy AI Gateway, Portkey, Kong, vLLM production-stack) that
offers reservation with borrowing.

## Benchmark

`gateway/bench_test.go` interleaves, on the same client and keep-alive
connections, one request straight to an upstream that answers instantly and one
through the gateway (memory store; auth, budgets, limits, fair share,
accounting and metrics all on the path), and reports latency percentiles:

```sh
go test -run '^$' -bench Overhead -cpu 1 -benchtime 20000x ./gateway
```

Apple M4 Max, `-cpu 1`, 5 runs × 20 000 interleaved pairs each (ranges are
min–max over the runs):

| | v0.1.0 (load 7.8–8.1) | v0.2.0 (load 6.3–7.5) |
|---|---|---|
| direct to the fake upstream, p50 / p99 | 37.1–37.8 / 102.2–105.8 µs | 35.2–36.6 / 88.4–113.8 µs |
| through the gateway, p50 / p99 | 92.0–93.1 / 233.5–239.9 µs | 88.3–90.7 / 226.9–242.2 µs |
| **added by the gateway, p50** | **54.9–55.3 µs** | **53.2–54.5 µs** |
| **added by the gateway, p99** | **131.2–134.2 µs** | **128.4–143.9 µs** |

The v0.2.0 path adds the organisation hierarchy, team model lists, budget
checks on every scope and tags; the added latency did not move. An earlier
v0.1.0 run at load 15–16 gave 54.1–55.1 µs p50.
LiteLLM's own overhead header read 3.2–4.9 ms on the same kind of upstream,
about 60–90 times the gateway's added p50. The two numbers are not measured
the same way (LiteLLM's is self-reported per request, ours is the end-to-end
difference against a direct call), but both exclude the upstream's own time.

## Security

Enforced:

- **Clients cannot choose their priority.** The body is parsed and re-encoded
  (duplicate keys and escaped spellings such as `"priority"` cannot
  survive); `priority`, `extra_body`, `request_id`, `kv_transfer_params`,
  `ec_transfer_params` and `vllm_xargs` are removed; upstream headers are built
  from scratch, so client `X-Vllm-Priority`, `x-pass-*`, `Authorization` and
  `X-Forwarded-*` never reach vLLM. Integration tests assert this against a
  recording fake vLLM; the rewrite and the SSE copier are fuzzed (`FuzzRewrite`,
  `FuzzCopySSE`, corpus in `openai/testdata/fuzz`, fuzz smoke in CI).
- **Tenants do not share vLLM's prefix cache.** Prefix-cache reuse is a timing
  side channel: a caller can test whether a guessed prompt prefix is cached
  from the time to first token ([vLLM security guide](https://docs.vllm.ai/en/stable/usage/security/),
  CVE-2025-46570). The gateway removes any client `cache_salt` and always sets
  its own: `base64url(HMAC-SHA256(key, scope))`, 43 characters (256 bits), the
  key read from `cache_salt_key_file`, the scope set by `cache_isolation`
  (`org` by default, `team`, `key`, or `none` for a single tenant). vLLM mixes
  the salt into the hash of the first KV block, and every later block hash
  chains from it ([prefix caching design](https://docs.vllm.ai/en/stable/design/prefix_caching.html)),
  so different scopes never share cached blocks. The salt is stable per scope
  (a random salt per request would disable prefix caching); rotating the key
  changes every salt and starts every tenant with a cold cache.
  Multimodal `uuid` values of the message content parts are **scoped, not
  stripped**, as the vLLM guide recommends: each becomes
  `llmf-` + an HMAC of (scope, uuid), so a client keeps referring to media it
  sent earlier, while the same uuid presented by two tenants names two
  different cache entries.
- **vLLM must only be reachable from the gateway**: its `--api-key` covers only
  `/v1`, `/v2` and `/inference`, and `/invocations` exposes the same inference
  without authentication (vLLM docs above). Firewall the vLLM ports.
- Virtual keys are 256-bit random secrets, stored as SHA-256, compared in
  constant time, shown once; the master key is compared after hashing both
  sides (no length leak). Secrets come from files only and never appear in
  logs, errors or listings; an unparsable DSN is not echoed.
- No SSRF: upstreams come from the configuration only (http/https, no
  credentials, query or fragment); client input selects a model group, never a
  URL; upstream redirects are not followed (a test checks the internal key never
  reaches a redirect target); environment proxies are ignored.
- Limits: request body, header size, read-header / read / idle timeouts, maximum
  request duration, maximum upstream response size; admin JSON bodies ≤ 64 KiB
  with unknown fields rejected; bounded accounting queue.
- Key cache: valid keys and misses live in two separate bounded maps, never
  cleared in bulk, so a spray of random keys cannot evict valid keys and push
  legitimate traffic to PostgreSQL (test: 200 000 random keys, the valid key is
  still served from the cache). Failed authentications (client keys and the
  master key) are limited per client network, IPv4 address or IPv6 /64, taken
  from the connection, not from proxy headers: `auth_failures_per_minute`
  (default 60, 0 disables) answers 429 before any store lookup, except for a
  valid key already in the cache.
- Budgets fail closed: when PostgreSQL cannot be read for 3 refresh periods,
  every request gets 503 (`store.fail_closed: false` to fail open).
- CI: race tests on amd64 and arm64, PostgreSQL integration tests against a
  service container, fuzz smoke, `govulncheck`, `CGO_ENABLED=0` cross-builds
  for linux/amd64 and linux/arm64.

Open defects (v0.2.0):

1. **State is per process.** In-flight counts, rate limits and the key cache
   live in memory: run one gateway replica per pool. Several replicas would each
   grant the full reservation.
2. **Budgets lag.** Cost is known when a request ends, so a burst of long
   requests can overspend by what is in flight. Several gateways only see each
   other's spend at their next refresh.
3. **Priorities are fixed at admission.** A borrowing request already queued in
   vLLM keeps its priority when its organisation drops back under its
   reservation; teams inside an organisation share its reservation without
   sub-shares of their own (vLLM
   has no API to re-prioritise a queued request).
4. **Aborted requests are charged with an estimate.** vLLM's usage chunk comes
   last; when the client leaves before it (or before a non-streaming answer),
   the gateway stops reading (vLLM is not kept generating for nobody) and
   charges prompt bytes / 4 plus the content deltas already forwarded. The
   record is flagged `estimated` (column in PostgreSQL and in the CSV export,
   metric `llmflow_usage_estimated_total`); the estimate undercounts tool-call
   and reasoning tokens.
5. **Prefix cache isolation is per scope, not absolute.** With the default
   `cache_isolation = "org"`, users of the same institution share one prefix
   cache and can still probe each other's prompts by timing; choose `team` or
   `key` to narrow that, at the cost of less cache reuse. vLLM itself states
   that `cache_salt` is "not an isolation boundary": tenants that need hard
   isolation need their own vLLM instance.
6. **Accounting can drop records** when the queue is full or a batch write fails
   twice; both are counted in metrics; budgets count the cost until the next
   refresh only.
7. No TLS termination in the gateway (put it behind a TLS proxy); the admin API
   shares the client listener; `/v1/responses` is not proxied.
8. `C` is static configuration; the gateway does not measure or adapt it.
9. **Admin principals are read from the store on every admin request** (no
   cache): fine for admin traffic, but the admin API fails with the store.
   Role changes therefore apply at once.
10. **Soft-budget alerts are sent once and not retried**: a webhook or SMTP
    failure is logged, and the period is already marked as alerted.

## Development

```sh
go test -race ./...
LLMFLOW_TEST_PG_DSN=postgres://… go test ./store   # PostgreSQL contract test
go test -run '^$' -fuzz FuzzRewrite -fuzztime 1m ./openai
```

License: BSD-3-Clause.
