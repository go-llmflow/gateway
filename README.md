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
| `trusted_proxies` | CIDR prefixes of the reverse proxies whose `X-Forwarded-For` is read; see Security |
| `routes { inference, admin, metrics, healthz { allow = [...] } }` | networks allowed per route class; `metrics` and `healthz` default to loopback |
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
- **Secrets only by reference.** Every `*_file` attribute (`master_key_file`,
  `metrics_token_file`, `store.dsn_file`, `cache_salt_key_file`, `key_file`,
  `alerts.webhook_token_file`, `smtp.password_file`) is a secret reference:
  a path (or `file:/path`), or a field of an OpenBao / HashiCorp Vault KV v2
  secret, `vault:<mount>/<path>#<field>` with an optional `?version=N`. Vault
  references need a `vault { address, token_file, ca_file, namespace,
  cache_ttl }` block: `https` only (plain http is accepted for loopback
  test servers), the token itself from a file, no redirect followed, reads
  cached for `cache_ttl` (default 5 minutes) and bounded in size. The read is
  `GET /v1/<mount>/data/<path>` with `X-Vault-Token`, the fields under
  `data.data` (OpenBao KV v2 API, "Read secret version"). Mount, path
  segments and field are restricted to `[A-Za-z0-9._-]`, so a reference
  cannot leave the mount. An upstream URL with credentials is rejected.
  Nothing is read from the environment or the command line.
- **Access tokens** (optional), besides virtual keys; see Security:

  ```hcl
  jwt {
    issuer   = "https://sso.example.org/realms/research"  # exact iss, https
    audience = "llmflow"                                   # required
    type     = "at+jwt"           # optional: refuse ID tokens
    # jwks_url, username_claim, groups_claim, clock_skew ("1m"), min_refresh ("1m")
    rpm = 60                      # limits per person, like a key's
    map {                         # first match wins
      value = "lab-physics"       # claim defaults to "groups"
      team  = "etabA"
    }
    map {
      claim   = "department"
      value   = "chemistry"
      team    = "etabB"
      project = "eval"
    }
  }
  ```
- **Durations are strings** in Go syntax (`"30s"`, `"168h"`), positive.
- **Absent and zero differ**: `retries = 0` disables retries; leaving it out
  takes the default (2).
- **Every name must be declared** (since v0.5.1): an organisation or team
  named in a `logging`, `policy`, `jwt` map, `sso` role or `scim` group block
  must be declared by an `org` block, or `check-config` and `serve` refuse the
  file with a diagnostic at the misspelt name. A typo would otherwise switch
  a request log, a policy or a role off without a word. A team created only
  through the admin API must be declared in an `org` block before the
  configuration names it (declaring it is harmless: `serve` ensures it
  exists). Projects are created through the admin API, not declared, so a
  `project` in a `jwt` map is checked for its syntax only. Model names are
  only declared in the configuration, never referenced by another block.
- The parser is fuzzed (`FuzzParse`, corpus in `config/testdata/fuzz`): the
  configuration is treated as untrusted input.

Cost of a request: `((prompt − cached) × input + cached × cached_price +
completion × output) / 10⁶` GPU-hours, from the `usage` vLLM reports. For
streams the gateway sets `stream_options.include_usage` and removes the usage
chunk again if the client did not ask for it.

### Request policies

A policy refuses inference requests that lack given fields, and replaces the
global size limits, for the teams it names and for keys created with it
(`POST /admin/keys {"policy": "…"}`, by a team admin; a member's key follows
its team). A key's own policy wins over its team's.

```hcl
policy "attributed" {
  teams        = ["etabA"]
  require      = ["user", "metadata.project"]   # body fields: a name or a dotted path
  require_tags = true                            # at least one request tag
}
policy "long-context" {
  max_body_bytes     = 33554432    # replaces limits.max_body_bytes (up to 1 GiB)
  max_response_bytes = 134217728   # replaces limits.max_response_bytes (non-streamed answers)
}
```

A field counts as present when it exists and is not `null`, an empty or
blank string, an empty list or an empty object. A refused request gets a 400
naming everything missing — `policy attributed requires: user, a request tag`
— and is counted in `llmflow_denied_total{reason="policy"}`. The field paths
and their evaluation are fuzzed (`FuzzRequiredFields`, and `FuzzParse` checks
that parsed policies are usable).

### Per-team request logs

By default **no request log is written for anyone**: the spend metadata goes
to the accounting store, the prompts and completions are forwarded and
forgotten, and the gateway's own log never holds them. A `logging` block
sends the requests of the teams it names to an export (see the `export` block
under [Audit log](#audit-log)):

```hcl
logging "research" {
  teams             = ["etabA", "etabB"]   # a team is in at most one block
  destination       = "archive"            # an export block
  content           = false                # default: metadata only
  max_content_bytes = 1048576              # per body, when content = true (at most 16 MiB)
  batch_records     = 500                  # an object every 500 records…
  batch_interval    = "1m"                 # …or every minute, and at shutdown
  lock              = false                # true: Object Lock retention on the objects
}
```

Each request is one JSON line: the spend record (time, org, team, project,
key id, model, pool, upstream, prompt / completion / cached tokens, cost,
latency, status, priority, borrowing, stream, estimated, tags) and its
`request_id`. Only a block with `content = true` adds a `content` object:
the request the client sent (as JSON, or as a string when cut), the answer
(the raw SSE for a stream), each cut at `max_content_bytes` with
`request_truncated` / `response_truncated` set. Lines are batched into gzipped
objects `requests/<block>/YYYY/MM/DD/<unix-nanos>-<n>.jsonl.gz` (UTC), also cut
at 8 MiB compressed. They go through the export's bounded queue: a batch
dropped or given up is counted and alerted like any export write
(`export_dropped`, `export_failed`).

### Guardrails

Guardrails check prompts before vLLM (stage `pre`) and completions after it (stage `post`, streams included). A request gets the guardrails of its key's organisation, of its team, and of the key itself. A key can add guardrails, `POST /admin/keys {"guardrails": ["pii"]}`, but it can never remove one.

```hcl
guardrail "secrets" {
  type  = "hide_secrets"                 # tokens and URL credentials, via go-gitsafe/gitsafe redact
  orgs  = ["etabA"]                      # and/or teams = [...]
}
guardrail "pii" {
  type    = "pii"                        # via go-llmflow/pii
  teams   = ["lab-med"]
  action  = "mask"                       # default for every kind
  actions = { iban = "block", nir = "block", phone = "log" }
}
guardrail "safety" {
  type               = "llama_guard"
  model              = "llama-guard"     # a model block with class = "guard"
  teams              = ["lab-med"]
  action             = "block"
  actions            = { S6 = "log" }    # per category, S1…S14
  stream_check_bytes = 1024
  timeout            = "2s"
  on_error           = "block"           # fail-closed (default); "allow" is fail-open
}
guardrail "injection" {
  type      = "prompt_guard"             # stage pre only
  model     = "prompt-guard"
  threshold = 0.9
}
guardrail "vendor" {                     # off unless written, and then only with:
  type           = "http"
  url            = "https://guard.example.org/check"
  token_file     = "vault:secret/llmflow#guard"
  allow_external = true                  # prompts and completions leave the site
}

model "llama-guard" {
  upstream_model = "meta-llama/Llama-Guard-3-8B"
  pool           = "guard"
  class          = "guard"               # not callable by clients, not in /v1/models
  key_file       = "vault:secret/llmflow#vllm"
  upstream "g0" { base_url = "http://gpu09:8000" }   # vllm serve meta-llama/Llama-Guard-3-8B
}
```

**Order.** The masking guardrails (`hide_secrets`, `pii`) run first, in configuration order, on every text of the request:

- message contents and their text parts;
- tool-call arguments;
- the completions `prompt` and the embeddings `input`.

The classifying guardrails then run **concurrently**, each under its own `timeout`, on the **masked** request. So no classifier ever sees what was masked, and an external one least of all. vLLM gets the masked prompt, and so does the content log. Post-call works the same way on the answer.

**Actions.**

- `mask` replaces the match with `[KIND]`, for example `[TOKEN]`, `[EMAIL]` or `[IBAN]`.
- `block` refuses with `400 guardrail_violation`, naming the guardrail and the categories.
- `log` only records the finding.

A guardrail that errs or times out refuses with `503` when it is fail-closed (the default). When it is `on_error = "allow"`, the request goes through and the error is counted.

**Audit and metrics.** Every finding and every error is written to the audit log, kind `guardrail`, action `guardrail.<mask|block|log|allow>`. The entry holds the guardrail, the stage, the category, the count and the request id, never the text. A request that passes every guardrail is only counted, since auditing it would put a database write on every request. The metrics are:

- `llmflow_guardrail_decisions_total{guard,stage,action}`;
- `llmflow_guardrail_errors_total{guard,outcome}`;
- `llmflow_guardrail_seconds`;
- `llmflow_denied_total{reason="guardrail"}`.

The retention kind `guardrail` applies to these entries.

**Streams.** Masking uses a bounded hold-back of 256 bytes per choice. It holds the last 256 bytes and any match still growing, and releases everything before them, masked. The full answer is never buffered. The latency added is the time to generate the first 256 bytes, about 64 tokens, once at the start of the stream; after that, each chunk releases as much as it adds.

Classifiers on a stream gate the text: the completion is judged every `stream_check_bytes` new bytes, and those bytes are released only once accepted. The text therefore arrives in bursts of that size, each one delayed by one classifier call. A refusal ends the stream with an error event (`guardrail_violation`) and `[DONE]`. Text already released stays released, but nothing that was not checked has been sent.

Guarded answers keep only the fields the guardrails know. Log probabilities, reasoning text and unknown fields are removed (see limitation 15).

**Guard models are local.** Llama Guard is served by vLLM as a chat model whose chat template builds the classification prompt. Prompt Guard 2 is served on vLLM's `/classify` endpoint (a pooling model); input longer than its 512 tokens is sent as up to 64 segments of 1500 bytes. The gateway reaches both through its own routing (upstream choice, retries, cooldown) at **vLLM priority 0**.

They do **not** go through the fair-share pool. A guard call is the gateway checking the team's work, not more of that work, and queueing it behind the team's own requests would add their wait to every one. Their **cost is charged to the team**, though: a spend record and its budgets, under the guard model's name, so `GET /admin/spend?group_by=team,model` shows it.

**HTTP adapter.** It POSTs `{"guardrail", "stage", "messages", "completion"}`, receives the already-masked text, and expects `{"action": "allow"|"block"|"log", "categories": [...]}`. Answers are capped at 64 KiB and redirects are not followed. It is refused at configuration time without `allow_external = true`, and `serve` logs a warning at start that prompts leave the site.

All guard answers are parsed defensively and fuzzed (`FuzzParseGuardAnswers`), as are the hold-back (`FuzzHoldback`: streamed in any pieces equals masked whole), the SSE rewriter (`FuzzStreamFilter`: no secret in any released string) and the request mapping (`FuzzMapRequest`).

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

### Single sign-on for administrators

With an `sso` block, administrators log in with OpenID Connect in a browser
instead of holding an admin token. The login (discovery, authorization code
with PKCE S256, `state`, `nonce`, ID token verification) is
[`github.com/go-authn/rp`](https://github.com/go-authn/rp) v0.1.1; the gateway
keeps the session and the role mapping.

```hcl
sso {
  issuer             = "https://login.example.org"
  client_id          = "llmflow-admin"
  client_secret_file = "/etc/llmflow/sso.secret"   # a secret reference; absent: a public client
  redirect_url       = "https://llm.example.org/admin/callback"
  scopes             = ["openid", "profile", "groups"]  # default openid, profile, email
  session_ttl        = "8h"                              # at most 168h
  role {                                               # groups -> role bindings
    value = "llm-admins"                               # claim defaults to "groups"
    role  = "proxy_admin"
  }
  role {
    value = "lab-physics-leads"
    role  = "team_admin"
    team  = "etabA"
  }
}
```

- `GET /admin/login` redirects to the provider; `GET /admin/callback` ends
  the login and redirects to `GET /admin/session`, which returns the user,
  their roles and the session's `csrf_token`. `POST /admin/logout` ends the
  session and returns the provider's `end_session_url`.
- The person is `preferred_username` (else a verified `email`, else `sub`).
  Their roles are the `role` blocks their claims match, **plus** the roles of
  the gateway user of the same name, read from the store on every request (so
  revoking a role or disabling the user takes effect at once). Somebody with
  neither is refused at the callback.
- The session is a random 256-bit value in a `__Host-llmflow-session` cookie
  (Secure, HttpOnly, SameSite=Lax, Path=/), kept server-side as its SHA-256.
  A login in progress is bound to the browser that started it by a
  `__Host-llmflow-login` cookie, its `state` is usable once and for 10
  minutes.
- **CSRF**: a state-changing request authenticated by the session cookie must
  carry the session's token in `X-CSRF-Token` and must not be marked
  cross-site (`Sec-Fetch-Site`) or come from another `Origin` than
  `redirect_url`'s. Requests with a bearer token are unaffected.
- Sessions live in the memory of the replica that created them: behind
  several replicas, use sticky sessions (open: sessions in PostgreSQL).

**SAML federations (RENATER, eduGAIN)** go through
[`go-authn/bridge`](https://github.com/go-authn/bridge) (`authn-bridge`), an
OpenID provider in front of a SAML federation, rather than SATOSA. Register
the gateway as one of its clients and point `sso.issuer` at the bridge:

```hcl
# authn-bridge configuration
client "llmflow-admin" {
  secret_file     = "/etc/authn-bridge/llmflow-admin.secret"
  redirect_uris   = ["https://llm.example.org/admin/callback"]
  id_token_claims = true   # the bridge's ID tokens carry no claims about the person otherwise
}
```

and ask for `scopes = ["openid", "profile", "groups"]`: the bridge's
`groups` are entitlements held to the IdP that asserted them, so a `role`
block can match `urn:mace:…` or `urn:geant:…` values.

### Provisioning with SCIM 2.0

With a `scim` block, an identity provider (Entra ID, Okta, Keycloak…)
provisions users and groups at `/scim/v2` (RFC 7643/7644). The protocol —
filters, PATCH, paging, versions, errors, discovery — is
[`github.com/go-authn/scim`](https://github.com/go-authn/scim); the gateway
stores what is pushed and derives its own state from it:

```hcl
scim {
  token_file = "vault:secret/llmflow#scim"      # the provider's bearer token, at least 32 bytes
  base_url   = "https://llm.example.org/scim/v2"
  group "Lab Physics" { team = "etabA" }         # members get member on etabA
  group "Lab Physics Leads" {
    team = "etabA"
    role = "team_admin"                          # member (default), team_admin or viewer
  }
}
```

- A provisioned user is a gateway user of the same `userName` (letters,
  digits, `_ . - @ +`, at most 128), disabled while `active` is false or
  after deletion, which revokes their admin tokens. A renamed user is a new
  gateway user; the old name is disabled.
- The members of a mapped group (display names compare without case) get the
  block's role on its team; leaving the group, deactivation or deletion takes
  it away. Only those (role, team) pairs of provisioned users are managed:
  roles granted by hand stay. Nested groups are not expanded.
- The state is reconciled after every change and at start-up, so a change
  interrupted half-way is repaired.
- The bearer token is compared in constant time and failures count in the
  per-network limit; `/scim/v2` obeys the `routes.admin` networks. With SSO,
  the provisioned roles apply to the person's session at once.

### Audit log

Every state-changing admin request is recorded, accepted or refused: the
actor and their roles, the action (`PUT /admin/budgets`), the target (path and
query), the state **before** and **after** (the key, team, budgets, roles…
the route acts on, read from the store) with the request body, the status, the
client address and a request id (the client's `X-Request-Id` when it is a
plain token, else a generated one, echoed in the response). Events the gateway
raises itself go to the same log: SSO logins and logouts, key rotations
(manual and scheduled), SCIM reconciliation. Key secrets and tokens never
appear: responses are not recorded. Reads are not audited.

The log is append-only in PostgreSQL (a trigger refuses `UPDATE`) and
**hash-chained**: entry *n* stores `SHA-256(0x00 ‖ hash(n−1) ‖ entry)`, so
altering, inserting, reordering or deleting an entry breaks every hash after
it. Appends take a transaction-scoped advisory lock, so several replicas
write one chain. With a signing key, **checkpoints** sign the newest hash at
an interval (and at shutdown):

```hcl
audit {
  signing_key_file    = "vault:secret/llmflow#audit"   # an Ed25519 seed; llmflow-gateway audit keygen -o FILE
  checkpoint_interval = "5m"
  anchor              = "archive"                      # an export block: where checkpoints also go
  anchor_max_lag      = "15m"                          # default 3 × checkpoint_interval
}

export "archive" {
  kind            = "s3"                               # s3, file; gcs and azure answer "not implemented"
  endpoint        = "https://minio.example.org:9000"   # https (http only to loopback)
  bucket          = "llmflow-audit"                    # create it with Object Lock enabled
  path_style      = true                               # MinIO, Ceph RGW
  access_key_file = "vault:secret/llmflow#s3_access"
  secret_key_file = "vault:secret/llmflow#s3_secret"
  prefix          = "gateway-1/"
  lock_mode       = "COMPLIANCE"                       # or GOVERNANCE
  retention       = "8760h"
  queue_size      = 1024
}
```

**Anchors.** Every checkpoint, and the entries it covers (gzipped JSON
lines that recompute to the same hashes), is also written to the anchor
export: `audit/checkpoints/<seq>.json` and `audit/entries/<from>-<to>.jsonl.gz`.
On S3 the objects go under Object Lock retention when the bucket has Object
Lock enabled (`GET /?object-lock` is read at start; MinIO and Ceph RGW require
it to be enabled when the bucket is created, with versioning); a `file`
export refuses to overwrite a file but does not stop its owner. At start the
gateway catches up with the checkpoints the export lacks. The age of the
newest exported checkpoint is `llmflow_audit_anchor_age_seconds`; a
checkpoint still unexported after `anchor_max_lag` raises one alert
(`audit_anchor_lag`, to the log and the alert channels). Export writes are
queued (bounded) and retried; an object dropped for want of room or given up
after its retries is counted (`llmflow_export_dropped_total`,
`llmflow_export_failed_total`) and alerted (`export_dropped`,
`export_failed`), never silent.

**Verifying.** `llmflow-gateway audit verify -c CONFIG --public-key KEY
--anchor export` recomputes the chain, checks every checkpoint, and checks the
database against the newest checkpoint in the export (`--anchor file:PATH`
takes one you kept yourself): the anchored entry must still be there with the
anchored hash, and the database's checkpoints must be at least as new. Give
the public key from somewhere the gateway's operator cannot change (`audit
pubkey` prints it).

**What this protects against — honestly.** Entries and checkpoints live in
the same database. Without an anchor, the log is tamper-evident only against
edits in the middle: someone with write access to the database can delete
the tail together with the checkpoints that cover it, restore an older
consistent backup, or delete everything, and `audit verify` without
`--anchor` says OK (it prints a warning). With an anchor kept outside, those
three fail verification, and so does a log rewritten and re-signed with a
stolen key **up to the anchor**. What happened after the newest exported
checkpoint (at most `checkpoint_interval` plus the export lag) can still be
removed without trace, and whoever holds the signing key can sign new history
after the anchor. Failed appends are logged and counted
(`llmflow_audit_failures_total`).

The canonical form hashed is version 2: every field's raw bytes, each
preceded by its length (big-endian uint64). Version 1, the JSON of the
fields, folded invalid UTF-8 to U+FFFD so two different values could hash
alike; rows written as version 1 still verify.

**Retention.** Entries are kept for ever unless a retention is set per kind
(`admin`, `auth` for SSO, `scim`, `system` for what the gateway does itself):

```hcl
audit {
  signing_key_file = "vault:secret/llmflow#audit"
  retention        = { admin = "61320h", auth = "8760h", scim = "8760h", system = "8760h" }  # at least 24h
  prune_interval   = "24h"
}
```

Every `prune_interval` the gateway deletes the longest prefix of the log
that ends at a signed checkpoint and whose every entry is past its kind's
retention — whole segments, so what remains starts from a signed checkpoint
and still verifies. A kind without a retention stops the cut. It never
prunes past the newest checkpoint the anchor export holds (what is deleted
in the database is kept there, under Object Lock on S3), and never a log
that does not verify (`audit_prune_refused` is raised instead). Each prune
is itself an `audit.pruned` entry.

*Why a chain and not a Merkle tree*: RFC 6962 §2.1 (Certificate
Transparency, obsoleted by RFC 9162 with the same tree hashing) builds a tree
so that a log can prove inclusion and append-only growth to a third party in
O(log n). The verifier here reads the whole log, for which a chain gives the
same guarantee with one hash per entry, and it prunes by cutting at a
checkpoint. The tree's domain separation is kept: the `0x00` prefix for
entries, and checkpoints sign a self-describing message
(`llmflow-audit-checkpoint/v1`).

| route | |
|---|---|
| `GET /admin/whoami` | the caller and their roles |
| `GET` / `PUT /admin/allowlists` | IP allowlists `{"kind":"org"\|"team"\|"key","name","cidrs":[…]}` (empty list removes); organisation lists by its admins, team and key lists by the team's admins |
| `GET` / `POST /admin/users`, `DELETE /admin/users/{name}` | list (filtered), create `{"name"}`, disable (revokes their tokens; proxy admin only) |
| `GET` / `POST /admin/users/{name}/tokens`, `DELETE /admin/users/{name}/tokens/{id}` | a user's admin tokens (the user themselves or a proxy admin) |
| `GET` / `PUT /admin/users/{name}/roles`, `DELETE /admin/users/{name}/roles?role=&org=&team=` | list, grant `{"role","org","team"}`, remove |
| `GET` / `POST /admin/orgs`, `DELETE /admin/orgs/{name}` | list (with configured share), create `{"name"}` (an organisation created here has no share: it only borrows), disable (its teams, projects and keys too) |
| `GET /admin/teams?org=`, `POST /admin/teams`, `DELETE /admin/teams/{name}` | list, create `{"name","org"}`, disable (its projects and keys too) |
| `GET` / `POST /admin/teams/{team}/projects`, `DELETE /admin/teams/{team}/projects/{name}` | list, create `{"name"}`, disable (revokes its keys) |
| `GET` / `PUT /admin/teams/{team}/models` | the model groups the team may use: `{"models":[…]}` (team admin and above; empty list = every configured model). A hidden model answers 404 like an unknown one and is left out of `/v1/models` |
| `GET /admin/keys?team=`, `POST /admin/keys`, `DELETE /admin/keys/{id}` | list (no secrets), create `{"team","project","name","rpm","tpm","max_inflight"}` (`project` optional; the `llmf-…` secret is returned once), revoke |
| `POST /admin/keys/{id}/rotate` | new secret for the same key `{"grace_period":"24h"}` (body optional; at most `720h`): returns `{"id","key","previous_expires_at"}` once. The key keeps its ID, so limits, budgets, allowlists and reports continue; the replaced secret stays valid until the grace period ends (absent or `0s`: revoked at once). Who may: the team's admins, or the member who owns the key. Logged as an `audit` event `key.rotated`, without the secret. On other replicas a replaced secret can outlive its deadline by at most `key_cache_ttl` |
| `PUT /admin/keys/{id}/rotation` | scheduled rotation `{"every":"720h","grace_period":"24h","target":"vault:secret/llmflow/keys/<team>/<name>#key"}` (`every` between `1h` and `8784h`; `grace_period` above 0 and shorter than `every`; `{"every":"0s"}` clears it). Needs a `key_rotation { vault_prefix = "secret/llmflow/keys", check_interval = "1h" }` block and a `vault` block whose token may `patch` and `create` under the prefix. Every `check_interval`, a due key gets a new secret, written into that KV v2 field (merge patch, so other fields stay) where the client reads it; the old secret stays valid for the grace period. The target must lie under `vault_prefix/<the key's team>/`, so a team admin writes only where the team's secrets live. Replicas rotate a key once (the rotation is conditional on the current secret); if the write fails the rotation is undone, the old secret keeps working and the key is retried at the next check. Audit events `key.rotation_scheduled`, `key.rotated`, `key.rotation_failed` |
| `GET /admin/budgets`, `PUT /admin/budgets`, `DELETE /admin/budgets?kind=&name=&model=` | budgets on any scope (see below), listed with their current `spent` and effective `limit` |
| `POST /admin/budgets/increase` | temporary increase `{"kind","name","model","amount","expires_at"}` (RFC 3339, within a year) |
| `GET /admin/spend?from=&to=&group_by=&format=` | spend report: requests, prompt / completion / cached tokens, GPU-hours and estimated requests, grouped by any of `org`, `team`, `project`, `key`, `model`, `pool`, `tag` (default `org,team`), as `json`, `csv` or `parsable2` (pipe-delimited like `sreport --parsable2`). Filtered to what the caller may see: an org admin sees their organisation, a member their own keys. A request with several tags counts once under each tag |
| `GET /admin/usage.csv?from=&to=` | the v0.1 export: `group_by=team,pool`, CSV |
| `GET /admin/fairshare` | reservations, in-flight, borrowing, usage, LF and rank per pool and organisation |

Client routes: `POST /v1/chat/completions`, `/v1/completions`,
`/v1/embeddings`, `/v1/responses`, `/v1/messages`, `GET /v1/models`. `GET /healthz` returns 503 when the budget
state is stale and fail-closed is on.

### Responses API

`POST /v1/responses` is proxied to vLLM's own Responses endpoint (served by vLLM natively; v0.32.0 checked), through the same pipeline as chat:

- authentication;
- the policy, budget and rate checks;
- guardrails;
- fair-share priority;
- the tenant's `cache_salt`;
- accounting from the Responses usage (`input_tokens`, `input_tokens_details.cached_tokens`, `output_tokens`), which a stream carries in its final `response.completed` event.

`priority`, `request_id`, `cache_salt` and the other engine fields are stripped, as they are for chat.

**Stateless only.** vLLM keeps every response in the memory of the instance that produced it (`store` defaults to true there). Anyone holding the id can then read it back, continue it (`previous_response_id`) or cancel it. Behind a gateway, that state would be shared by every tenant and would live on one upstream among several. So the gateway works as follows:

- it forces `"store": false`;
- it refuses `previous_response_id`, `background`, `conversation` and stored `prompt` with a 400 that says so;
- it does not route `GET /v1/responses/{id}` or `.../cancel`.

Clients send the whole conversation in `input`, as they do with chat completions.

**Guardrails.** Guardrails read the request's `instructions` and its `input` items:

- message text and parts (`input_text`, `output_text`);
- function-call arguments and outputs.

Input items of any other type are refused, since the guardrails cannot read them. In the answer, output message text and function-call arguments are masked. Reasoning items, annotations, log probabilities and every unknown field are removed.

A streamed Responses request under post-call guardrails is refused (400, ask with `"stream": false`).

### Anthropic Messages API

`POST /v1/messages` is proxied to **vLLM's own Anthropic endpoint**, with no translation in the gateway. vLLM v0.32.0 serves `/v1/messages` natively (`vllm/entrypoints/anthropic`): it converts each request to a chat completion with the same raw request, so the gateway's `X-Vllm-Priority` and `cache_salt` apply.

Coding agents that speak the Messages API can therefore use local models. Point them at the gateway, with a virtual key as the bearer token.

**Accounting.** It follows vLLM's `AnthropicUsage`. There `input_tokens` excludes the cached prompt, so the gateway charges `input_tokens + cache_read_input_tokens + cache_creation_input_tokens` as prompt, of which `cache_read_input_tokens` are cached.

In a stream, the input counts come from `message_start` and the output count from the final `message_delta`.

`/v1/messages/count_tokens` is not routed.

Errors carry both shapes: OpenAI's `{"error": {...}}` and Anthropic's top-level `"type": "error"`.

**Guardrails.** The request is mapped in full:

- `system` (a string or text blocks);
- text blocks and `thinking` blocks in the history;
- `tool_result` content;
- every string inside `tool_use` inputs.

Blocks the guardrails cannot read are refused, an Anthropic `document` among them.

**Streams are guarded** with an Anthropic event filter that works like the chat one:

- text blocks (`text_delta`) and tool calls (`input_json_delta`, masked in the JSON text) go through the hold-back and the classifier gate;
- `thinking` blocks are removed, and the remaining blocks are renumbered from 0;
- `message_start` and `message_delta` keep only their known fields;
- an upstream `error` event keeps its type, but not its message;
- any other event stops the stream with an `error` event.

Masking inside tool JSON reads the JSON text as is, so a value written with `\u` escapes is not unescaped first. Its events repeat the text in several shapes (`.delta`, `.done`, the final response) that the stream filter does not rewrite yet. Without post-call guardrails, streams pass through unchanged.

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

**Independent comparison, measured the same way.** A separate harness (not
this repository's) ran llmflow v0.1.0 (`GOMAXPROCS=1`) and LiteLLM 1.106.2 in
front of the same instant upstream, with 2 000 interleaved rounds per run, 8
runs, at a 1-minute load of 3.3–3.94, and took the overhead of each as the
end-to-end difference against a direct call:

| added latency | llmflow v0.1.0 | LiteLLM 1.106.2 |
|---|---|---|
| p50 | **117 µs** | 4.86 ms |
| p99 | **239 µs** | 6.21 ms |

v0.2.0–v0.4.0 were not re-measured with that harness; the in-repository
benchmark above did not move between v0.1.0 and v0.2.0, and the v0.3.0 and
v0.4.0 paths add a HMAC per request (cache salt) and, for JWT access tokens, a
cache lookup per request (a signature check on a cache miss).

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
- **Client addresses and network restrictions.** The client is the
  connection's address, unless the connection comes from one of
  `trusted_proxies`: then `X-Forwarded-For` is read **from the right**,
  skipping trusted proxies, and the first other address is the client (MDN,
  X-Forwarded-For, "trusted proxy list"; the leftmost entries are written by
  the client and never used). An unparsable or zoned entry, or a chain of more
  than 64 hops, stops the walk at the last trusted hop. Each route class can
  be restricted to networks (`routes { … }`): `/metrics` and `/healthz` answer
  only loopback by default (`/metrics` also with its token from anywhere).
  IP allowlists on an organisation, a team or a key (admin API) must all
  match the client, else 403 with one generic message; they are loaded with
  the budget state, so the request path never queries the store. The address
  parser is fuzzed (`FuzzClientAddr`).
- **OpenID Connect access tokens on the inference API.** With a `jwt { … }`
  block, a bearer token that has the JWS compact shape (three dot-separated
  segments) is verified by [`github.com/go-authn/oidc`](https://github.com/go-authn/oidc)
  v0.4.2 (fuzzed in its own repository; v0.4.2 refuses an `nbf` that wrapped
  past the int64 range and RSA exponents below 3) instead of being looked up
  as a virtual key. That library does the
  JOSE work and its refusals: no `alg: none`, no HMAC algorithm (so the
  issuer's public key cannot be used as an HS256 secret), `iss` compared
  whole, `aud` and `exp` required, bounded clock skew, an optional `typ`
  check (`type = "at+jwt"` refuses ID tokens, RFC 9068 §4), keys fetched over
  https only, and at most one key-set fetch per `min_refresh` however many
  unknown `kid`s arrive. Discovery happens at start-up: a gateway whose issuer
  does not answer does not start. The gateway only maps the verified identity:
  the `map` blocks are tried in order, the first whose `claim` (default
  `groups`; a string or a list of strings) holds `value` gives the team and
  optional project. Each person is one synthetic key, `jwt-` + a hash of
  (issuer, subject), so rate limits (`rpm`, `tpm`, `max_inflight` of the
  block), budgets, IP allowlists and spend reports apply as to a key. Every
  refusal is the same 401 body; the reason (expired, wrong audience, unmapped
  …) is logged on the server only. A verified token is cached until its
  `exp` or `key_cache_ttl`, whichever comes first. Integration tests run the
  gateway against a fake provider (alg none, HS256 with the public key,
  expired, wrong audience or issuer, unmapped group, an unknown-`kid` flood,
  an ID token where an access token is required), and `FuzzJWTBearer`
  fuzzes the bearer path with the verifier in the loop.
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
   shares the client listener; `/v1/responses` is served statelessly only
   (see [Responses API](#responses-api)).
8. `C` is static configuration; the gateway does not measure or adapt it.
9. **Admin principals are read from the store on every admin request** (no
   cache): fine for admin traffic, but the admin API fails with the store.
   Role changes therefore apply at once.
10. **Soft-budget alert retries live in memory**: each channel (webhook,
    SMTP) is retried on its own, up to 6 attempts with exponential backoff
    (2 s doubling, 2 min cap, 25 % jitter), a 4xx other than 408/429 is not
    retried, every webhook attempt carries the same `Idempotency-Key`
    (budget and period) so a receiver can de-duplicate, and give-ups and
    retries are counted (`llmflow_alert_failures_total`,
    `llmflow_alert_retries_total`). A gateway stopped mid-retry does not
    resume: the period is already marked as alerted.

11. **SSO sessions live in one replica's memory**: behind several replicas,
    use sticky sessions.
12. **A person holding a JWT cannot be revoked individually**: disable the
    team, change the `map` blocks, or revoke at the identity provider; a token
    already verified stays cached until its `exp` or `key_cache_ttl`.
13. **SCIM does not expand nested groups**, and a provisioned role on a team
    that does not exist is logged, not created.
14. **The audit log's guarantees end at its newest anchor**: without an
    anchor export, truncation, rollback and deletion of the database go
    undetected; with one, what happened after the newest exported checkpoint
    can still be removed, and a stolen signing key can sign new history after
    it. Keep the key in OpenBao/Vault, verify with a public key kept
    elsewhere, and keep an anchor on write-once storage.
15. **What guardrails do not inspect**, and how each gap is handled:
    - Streamed tool-call arguments cannot be masked, so a guarded stream that carries them is stopped. Ask for tool calls without `stream`.
    - Reasoning text (`reasoning_content`), refusals and any field the guards do not know are removed from guarded answers, not inspected. The same goes for log probabilities.
    - A secret whose start appears more than 256 bytes before it can match is not seen in a stream. One example is a URL userinfo longer than about 250 bytes before its `@`.
    - A span still open after 64 KiB is released masked as far as it has reached. Its tail goes out in clear if that tail no longer matches on its own.
    - Audit entries of guardrail decisions are written asynchronously. When the queue is full they are dropped, counted (`llmflow_guardrail_audit_dropped_total`) and alerted.
16. **Phone numbers have no checksum**: a PII guardrail that masks them also masks 10-digit references that start with 0. This was measured in go-llmflow/pii.

## References

Opened while building v0.1.0–v0.4.0, and cited where used above:

- vLLM: [security guide](https://docs.vllm.ai/en/stable/usage/security/),
  [prefix caching design](https://docs.vllm.ai/en/stable/design/prefix_caching.html),
  priority scheduling (`--scheduling-policy priority`).
- Slurm: Fair Tree and the multifactor priority plugin (`PriorityDecayHalfLife`),
  `sreport`.
- HCL2 native syntax specification (`hclsyntax/spec.md`).
- OpenBao / HashiCorp Vault KV v2 API: "Read secret version", "Create/Update
  secret", "Patch secret".
- MDN, `X-Forwarded-For` (trusted proxies, reading from the right).
- [go-authn/oidc](https://github.com/go-authn/oidc) and
  [go-authn/rp](https://github.com/go-authn/rp) documentation (OIDC Core,
  RFC 7636 PKCE, RFC 9068 `at+jwt`, as those packages implement them);
  [go-authn/bridge](https://github.com/go-authn/bridge) README ("What an
  application gets").
- [RFC 7643](https://www.rfc-editor.org/rfc/rfc7643) and
  [RFC 7644](https://www.rfc-editor.org/rfc/rfc7644) (SCIM), through
  [go-authn/scim](https://github.com/go-authn/scim).
- [RFC 6962](https://www.rfc-editor.org/rfc/rfc6962) §2.1 (Merkle hash trees,
  domain separation) and §3.5 (signed tree heads);
  [RFC 9162](https://www.rfc-editor.org/rfc/rfc9162), which obsoletes it.
- Amazon S3 API Reference: PutObject (Object Lock headers, `Content-MD5`),
  GetObjectLockConfiguration, ListObjectsV2; MinIO object locking and
  immutability; Ceph RGW bucket operations (`?object-lock`). Implemented in
  [go-volumes/s3](https://github.com/go-volumes/s3) v0.2.0, which this uses.
- OWASP CSRF prevention cheat sheet (synchronizer token, Fetch Metadata).
- LiteLLM: [Enterprise features](https://docs.litellm.ai/docs/enterprise),
  [Dynamic TPM/RPM Allocation](https://docs.litellm.ai/docs/proxy/dynamic_rate_limit),
  [virtual keys](https://docs.litellm.ai/docs/proxy/virtual_keys) (key
  regeneration and grace period).
- Guardrails (v0.6.0):
  - Meta, the [Llama Guard 3 model card](https://huggingface.co/meta-llama/Llama-Guard-3-8B) (categories S1–S14) and its [prompt format](https://dev.meta.ai/llama/docs/model-cards-and-prompt-formats/llama-guard-3/). The classifier judges the last `User` or `Agent` turn. It answers `safe`, or `unsafe` followed on the next line by the comma-separated categories.
  - The [Llama Prompt Guard 2 86M model card](https://huggingface.co/meta-llama/Llama-Prompt-Guard-2-86M): an mDeBERTa sequence classifier with labels benign and malicious and a 512-token context. Longer input is split into segments.
  - vLLM: the [OpenAI-compatible server](https://docs.vllm.ai/en/latest/serving/online_serving/) and the [classification API](https://docs.vllm.ai/en/latest/models/pooling_models/classify/), `/classify` with `{"model", "input": [...]}` and response `data[].label`/`probs`.
  - [go-gitsafe/gitsafe](https://github.com/go-gitsafe/gitsafe) `redact`, v0.17.0. `Find` was added there for this gateway.
  - [go-llmflow/pii](https://github.com/go-llmflow/pii) v0.1.0, whose README cites ISO 13616, INSEE, ISO/IEC 7812-1, E.164, RFC 5322, RFC 5321 and RFC 4291.

## Development

```sh
go test -race ./...
LLMFLOW_TEST_PG_DSN=postgres://… go test ./store   # PostgreSQL contract test
go test -run '^$' -fuzz FuzzRewrite -fuzztime 1m ./openai
```

License: BSD-3-Clause.
