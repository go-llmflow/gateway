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
- an MCP gateway (`/mcp`, revision 2026-07-28 and 2025-11-25) in front of
  configured tool servers, with per-team tool allow-lists and every call
  audited;
- a server-rendered admin web UI (no JavaScript) behind SSO;
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
| `model "<name>" { … }` | client model name → `upstream_model`, `pool`, `class` (`interactive` / `batch`), `key_file`, `cost { input_per_mtok, output_per_mtok, cached_per_mtok, unit, price_date }` per million tokens, in GPU-hours unless `unit` names a currency (see [Cost units](#cost-units-since-v0120)), and `upstream "<name>" { base_url, key_file }` blocks |
| `store { … }` | `kind` (`memory` / `postgres`), `dsn_file`, `spool_dir` (required with postgres, see [Durable accounting](#durable-accounting)), `spool_max_bytes` (default 256 MiB), `fail_closed` (default true), `queue_size`, `batch_size` |
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
    # jwks_url, username_claim, groups_claim, clock_skew ("1m"),
    # min_refresh ("1m", at least "5s"; see "Key rotation" under Single sign-on)
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

**Every JSON layer is decoded first** (since v0.7.1). The finders read the text the model will read, not its encoding:

- A text that is itself a JSON document is parsed, and each key and string is read in turn. Tool-call arguments, tool inputs and tool results are such documents.
- `\uXXXX` and the other JSON escapes are decoded.

Masks land on the decoded form, and the request is re-encoded. A secret written `ghp_…` in tool arguments, or an IBAN escaped in a tool result, is found and masked like the plain form.

In v0.7.0 these layers were read encoded, which was a guardrail bypass. `FuzzEscapedFindings` now checks that escaping any part of a request never finds less than the plain form. In a Messages stream, a tool call's input is held whole until its block ends, then masked through every layer and released at once.

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

## Project attribution, labs and users (v0.13.0)

Coding clients do not say which project a request belongs to:

- Claude Code sends its session, agent, prompt and request-class headers, but no project. It does send whatever is set in `ANTHROPIC_CUSTOM_HEADERS` (see Claude Code's [LLM gateway connection guide](https://code.claude.com/docs/en/llm-gateway-connect.md)).
- Continue sends the headers listed in `requestOptions.headers`.

So a request can name its project in a header:

```sh
export ANTHROPIC_CUSTOM_HEADERS="X-Llmflow-Project: climate-sim"   # Claude Code
```

```hcl
attribution {
  project_header  = "X-Llmflow-Project"  # the default
  default_project = "general"            # optional
}
org "etabA" {
  share = 0.5
  team "physics-1" { lab = "physics" }   # optional lab: org → lab → team → project
}
```

**What the gateway accepts in the header:**

- The value is a project **name**, looked up in the key's own team only, so another team's or organisation's project cannot even be named.
- The project must exist and be enabled.
- A key pinned to a project accepts only that project.
- For a key with an owner, the owner must still hold a stored `member`, `team_admin`, `org_admin` or `proxy_admin` role on the team. For a JWT, its team comes from its verified `map`.
- Every refusal is the same 403, "project not allowed for this key", counted in `llmflow_denied_total{reason="project"}`.

**Without the header,** the key's project applies, then `default_project` if the team has a project of that name, else none.

**What happens to the effective project:** it is charged, budgeted, written to the request log and recorded. The lookups read state loaded with the budgets, so the request path does not read the store.

**SSO roles do not count.** A role that comes only from SSO ID-token claims is not stored, so it does not count for this check: grant the role in the gateway, or through SCIM.

**Reports gain two dimensions:**

- `lab` is optional and set as an attribute on a configured team. It is an accounting level only: RBAC does not use it. It is stamped on each record at request time, so moving a team to another lab does not rewrite history.
- `user` is the key owner, or a JWT's username when it is a valid gateway user name, otherwise the JWT's stable synthetic key id. It is never a token.

A lab is an attribute rather than an `org { lab { team {} } }` nesting so that every existing configuration stays valid and a team keeps a single declaration.

## Durable accounting

Usage is billed between institutions, so a record must survive a PostgreSQL outage and a crash of the gateway. With `kind = "postgres"`, `store.spool_dir` is therefore required (since v0.10.0):

```hcl
store {
  kind            = "postgres"
  dsn_file        = "/etc/llmflow/pg.dsn"
  spool_dir       = "/var/lib/llmflow/spool"
  spool_max_bytes = 268435456 # 256 MiB per process
}
```

**What a record goes through:**

1. Every batch of usage records is appended to this process's spool file and fsync'd before it is inserted. Batches are flushed every second, or every `batch_size` records.
2. When the store refuses, the batch stays pending, in order, and is retried every second. `llmflow_accounting_spooled` shows how many records wait.
3. Once everything is stored, the file is emptied.
4. A process that stops cleanly removes its empty file and keeps a non-empty one.
5. A process that dies (`kill -9`, OOM, power loss) leaves its file locked by nobody. The next process on the directory adopts it, at start and then every 10 s, and stores what it holds. `llmflow_accounting_replayed_total` counts those records.

**No double billing.** Every record has an ID (the request id), and PostgreSQL stores a record ID at most once (`ON CONFLICT DO NOTHING`). A batch stored just before a crash, and replayed after it, is billed once.

**Bounded.** One process's spool holds at most `spool_max_bytes`. Beyond that, a batch is still sent to the store once. If the store refuses it, the batch is lost: counted in `llmflow_accounting_failed_total` and alerted as `accounting_records_lost`.

**Untrusted after a crash.** The spool is read defensively, and the reader is fuzzed (`FuzzRead`):

- each frame carries a CRC-32C, its length is bounded, and its payload must be a record;
- a torn last frame (an interrupted write) is ignored;
- anything else stops the reading, and the file is kept as `*.corrupt` for an operator, with an alert.

**Permissions.**

- Spool files are created with mode 0600. The directory is created with mode 0700.
- A directory writable by group or others is refused at start, since another user could plant records there.
- Each process holds an exclusive `flock` on its own file. Several gateways may share a directory: on Unix, a file is adopted only when its owner is gone.

**What is not covered.** Records still in the in-memory queue when the process dies are lost: at most the last second of traffic.

**Measured.**

- `TestChaosAccountingOutage`: 21 of 21 records stored once after a PostgreSQL outage. v0.9.0 lost all 21.
- `TestCrashWithSpooledRecordsIsReplayed`: 20 records were spooled while PostgreSQL was unreachable, then the gateway was killed with SIGKILL and restarted. 21 of 21 records were stored, with 21 distinct IDs.

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

### Cost units (since v0.12.0)

A cost has a **unit**. vLLM models cost GPU-hours by default (`unit`
omitted or `"gpu_hours"`). A model can be priced in a currency instead, as
an external provider is, with the date its prices were read:

```hcl
cost {
  unit            = "USD"          # letters, digits, _; at most 16
  price_date      = "2026-10-26"   # required with a currency
  input_per_mtok  = 2
  output_per_mtok = 10
  cached_per_mtok = 0.1
}
```

**GPU-hours and a currency are never added together.**
- **Budgets.** A budget carries a `unit` (`"gpu_hours"` by default) and counts only the requests of models priced in that unit. A scope can hold one budget per unit, for example `{"kind":"org","name":"etabA","unit":"USD","period":"month","max_cost":500}` beside its GPU-hours budget. A request is checked against the budgets of its model's unit only. `PUT /admin/budgets` refuses a unit no model costs in, since such a budget could never fill. `DELETE /admin/budgets?…&unit=USD` and `/admin/budgets/increase` take the unit too.
- **Reports.** A spend report is split by unit whatever the `group_by`. Each JSON row has `cost` and `unit`, and the top-level `unit` is the common one or `"per_row"`. `gpu_hours` keeps its v0.11 meaning and is 0 on a currency row. CSV and parsable2 gain two trailing columns, `cost` and `unit`, so the earlier columns keep their positions.
- **Metrics.** Currency costs go to `llmflow_cost_total{team,pool,unit}`; `llmflow_gpu_hours_total` stays GPU-hours only.
- **Fair share.** It ranks by the usage of a pool, so the models of a pool must share one unit: `check-config` refuses a pool that mixes them.

Upgrade: PostgreSQL gains a `unit` column on `llmflow_usage` and on `llmflow_budgets_v2`, whose primary key becomes (kind, name, model, unit). Existing rows are GPU-hours (`''`). The migration runs at start-up, once.

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
  min_refresh        = "1m"                              # key set refetch bound, at least 5s
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

- **Key rotation (`min_refresh`, both the `sso` and the `jwt` blocks).** The provider's key set is read at start-up. A token with a `kid` it lacks triggers one fetch, at most once per `min_refresh` (default 1 minute). A provider that rotates its keys more often than that will have logins refused right after a rotation, until `min_refresh` has passed; dex rotating every 30 s showed this (#60). Lower `min_refresh` for such a provider. The trade-off: a stream of forged `kid`s can make the gateway fetch the key set once per `min_refresh`, which is why the value is bounded below at 5 s.
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

### Admin web UI

With an `sso` block, `/ui/` serves a web console for administrators
(`/admin/login?next=ui` signs in and lands there). It is server-rendered
HTML with **no JavaScript** at all, under a strict policy on every `/ui/`
response:

```
Content-Security-Policy: default-src 'none'; style-src 'self'; img-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'
```

plus `X-Content-Type-Options: nosniff`, `Referrer-Policy: no-referrer` and
`Cache-Control: no-store`. The one stylesheet is served by the gateway; no
third-party asset is loaded.

| page | |
|---|---|
| Keys | list per team; create (the secret is shown **once**, on a page not cached), rotate with a grace period, revoke |
| Budgets | budgets with spent and limit; set and delete |
| Spend | GPU-hours (or a provider's currency), requests and tokens over a period, grouped by org, team, project, key, model, pool or tag |
| Reports (since v0.13.0) | see below |
| Fair share | live state per pool: reservation, in flight, borrowing, LF and rank |
| Audit | entries page by page, and a button that verifies the chain (status of the checkpoints and the anchor) |

**Reports** (`/ui/reports`) is the finance view of the spend report:

- a period (date pickers, and links for the last 7 and 30 days, this month,
  last month, this year), in UTC with the end excluded, shown on the page;
  at most 366 days;
- a grouping over every report dimension (`org`, `lab`, `team`, `project`,
  `user`, `key`, `model`, `pool`, `tag`, `day`);
- drill-down: each row of `org` links to its labs, a lab to its teams, then
  projects, users and models; filters are listed and each can be removed;
- the previous period of the same length, side by side (amount and change);
- server-rendered SVG charts: the share per group (pie; the seven largest,
  the rest as "other"), this period against the previous one (bars), and the
  amount per day (time series).

Every amount shows its unit, one section per unit: **GPU-hours** for the
local pools, the **currency** of an external provider (for example `USD`) on
its own; the two are never added together. The page reads
`GET /admin/spend` like every other page, so an organisation administrator
sees their organisation only. The charts are inline SVG with presentation
attributes only (no `style` attribute, no script), so the policy above is
unchanged; every label is escaped. The page's queries are fuzzed
(`FuzzReportsQuery`). `GET /admin/spend` also accepts `group_by=day` (and
`lab`, `user`), and every report now runs under a 60-second deadline in the
store.

**Exports** (since v0.13.0), from the Reports page (CSV, XLSX, PDF links) or
the API:

```
GET /admin/export?from=2026-10-01&to=2026-11-01&format=csv|xlsx|pdf[&group_by=org,lab,team,project,user]
```

| format | content |
|---|---|
| `csv` | the spend report (default grouping `org,lab,team,project,user,model,pool`), with `cost` and `unit` columns |
| `xlsx` | a **Summary** sheet (period dates, totals per unit against the previous period), a **Reconciliation** sheet (inference and Slurm batch GPU-hours per organisation), then one sheet per grouping (`org`, `lab`, `team`, `project`, `user`, `model`, `pool`, `tag`, `day`); numbers are typed, each cost column names its unit (`Cost (GPU-hours)`, `Cost (USD)`) and a unit has its own column |
| `pdf` | a management summary: period, totals per unit, totals per level (org, lab, team, project; ten largest) with pie charts, the daily trend, and a reconciliation table of inference against Slurm batch GPU-hours per organisation |

What an export shows is what the spend report shows the same person (an
organisation administrator: their organisation only). Every download is
written to the audit log with kind `export` (actor, roles, query, format,
size, status), refusals included. Bounds: at most 366 days, at most 100 000
rows at the finest grouping (413 beyond), the 60-second store deadline.

Injection: a CSV cell that begins with `=`, `+`, `-`, `@`, a tab or a
carriage return is prefixed with `'`; the XLSX writer (go-ooxml v0.6.0) has
no formula API at all and marks such strings `quotePrefix`; the PDF
(go-pdfkit v0.19.0, one embedded font, Atkinson Hyperlegible) draws every
label as glyphs, so no data reaches the PDF syntax, and it carries no
action, link, script or attachment. Labels lose their control and format
characters (bidi overrides included) and are clipped. The file name is
built from the dates only. The writers are fuzzed (`FuzzExportWriters`).
go-pdfkit/html2pdf was considered and set aside: it puts an HTML and CSS
engine (go-webengine) in front of the same writer, and has no release.

**Slurm reconciliation** (since v0.13.0, optional): batch GPU-hours per
organisation next to inference GPU-hours, on the Reports page and in the
XLSX and PDF exports. The input is the output of

```
sreport -P cluster AccountUtilizationByUser -T gres/gpu -t Hours start=2026-10-01 end=2026-11-01
```

either a file the gateway reads when a report is built, or a file uploaded
with an export (`POST /admin/export?…&format=xlsx|pdf` with the report as a
`text/plain` body, or the form on the Reports page):

```hcl
slurm {
  sreport_file = "/var/lib/llmflow/sreport.txt"   # optional; absolute path
  accounts     = { "phys-a" = "etabA" }            # optional; Slurm account -> organisation
}
```

The parser (package `slurm`) reads the delimited layouts (`-P`, `-p`, or a
comma-separated file with the same header) and sreport's default
fixed-width table (cut at its dashed rule; prefer `-P`, the fixed width
truncates long names with `+`). Only account totals count (rows without a
login), for the organisation `accounts` names or, failing that, the
organisation of the same name. The unit comes from sreport's "Usage
reported in …" line (minutes by default, hours, seconds); a report in
percent is refused, rows of another TRES are skipped. The period sreport
printed is shown next to the table, so a mismatch is visible. The input is
untrusted: at most 1 MiB, 50 000 lines of 4 KiB, 32 columns, finite
non-negative usage, UTF-8; every refusal is an error (400 for an upload,
a generic 500 and a log line for a configured file that cannot be read,
whose path is never shown). It is fuzzed (`FuzzSreport` in `./slurm`). An
organisation's batch usage is shown to whoever may view that
organisation (org viewers and administrators, proxy roles); team-level
roles see none.

The UI holds no privilege of its own: every page calls the admin API's
handlers in-process with the signed-in person, so it sees and changes exactly
what the API lets their roles (an organisation administrator sees their
organisation only), and every change is audited as through the API. Only an
SSO session is accepted (no bearer token, no session: redirected to the
login); without an `sso` block `/ui/` answers 404. Every form carries the
session's CSRF token, forms must be `application/x-www-form-urlencoded`, at
most 16 KiB, and the `Origin` checks of the API apply. The form handlers are
fuzzed (`FuzzUIForms`: no 5xx, the policy on every answer, no script tag).

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
(`admin`, `auth` for SSO, `scim`, `system` for what the gateway does itself,
`guardrail` for guardrail decisions, `tool` for MCP tool calls):

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
| `GET` / `PUT /admin/teams/{team}/tools` | the MCP tools the team may call: `{"tools":["github.*","docs.search","*"]}` (set by organisation admins and above, read by anyone who sees the team; empty list = no tool). Returns the list and the configured servers. See [MCP tools](#mcp-tools) |
| `GET /admin/keys?team=`, `POST /admin/keys`, `DELETE /admin/keys/{id}` | list (no secrets), create `{"team","project","name","rpm","tpm","max_inflight"}` (`project` optional; the `llmf-…` secret is returned once), revoke |
| `POST /admin/keys/{id}/rotate` | new secret for the same key `{"grace_period":"24h"}` (body optional; at most `720h`): returns `{"id","key","previous_expires_at"}` once. The key keeps its ID, so limits, budgets, allowlists and reports continue; the replaced secret stays valid until the grace period ends (absent or `0s`: revoked at once). Who may: the team's admins, or the member who owns the key. Logged as an `audit` event `key.rotated`, without the secret. On other replicas a replaced secret can outlive its deadline by at most `key_cache_ttl` |
| `PUT /admin/keys/{id}/rotation` | scheduled rotation `{"every":"720h","grace_period":"24h","target":"vault:secret/llmflow/keys/<team>/<name>#key"}` (`every` between `1h` and `8784h`; `grace_period` above 0 and shorter than `every`; `{"every":"0s"}` clears it). Needs a `key_rotation { vault_prefix = "secret/llmflow/keys", check_interval = "1h" }` block and a `vault` block whose token may `patch` and `create` under the prefix. Every `check_interval`, a due key gets a new secret, written into that KV v2 field (merge patch, so other fields stay) where the client reads it; the old secret stays valid for the grace period. The target must lie under `vault_prefix/<the key's team>/`, so a team admin writes only where the team's secrets live. Replicas rotate a key once (the rotation is conditional on the current secret); if the write fails the rotation is undone, the old secret keeps working and the key is retried at the next check. Audit events `key.rotation_scheduled`, `key.rotated`, `key.rotation_failed` |
| `GET /admin/budgets`, `PUT /admin/budgets`, `DELETE /admin/budgets?kind=&name=&model=` | budgets on any scope (see below), listed with their current `spent` and effective `limit` |
| `POST /admin/budgets/increase` | temporary increase `{"kind","name","model","amount","expires_at"}` (RFC 3339, within a year) |
| `GET /admin/spend?from=&to=&group_by=&format=` | spend report: requests, prompt / completion / cached tokens, GPU-hours and estimated requests, grouped by any of `org`, `lab`, `team`, `project`, `user`, `key`, `model`, `pool`, `tag` (default `org,team`), as `json`, `csv` or `parsable2` (pipe-delimited like `sreport --parsable2`). Filtered to what the caller may see: an org admin sees their organisation, a member their own keys. A request with several tags counts once under each tag |
| `GET /admin/usage.csv?from=&to=` | the v0.1 export: `group_by=team,pool`, CSV |
| `GET /admin/fairshare` | reservations, in-flight, borrowing, usage, LF and rank per pool and organisation |
| `GET /admin/audit?from=&limit=` | audit entries with sequence ≥ `from` (default 1), at most `limit` (default 50, ≤ 200), as a JSON list; proxy admins and viewers only |
| `POST /admin/audit/verify` | recomputes the chain and checks the signed checkpoints with the gateway's own key, and the newest anchored checkpoint when an anchor export is configured: `{"ok","first","last","entries","checkpoints","unsigned","anchored","anchor_seq","signed","problems"}`. One verification at a time (429 otherwise); proxy admins and viewers only. For an independent check use `llmflow-gateway audit verify` with a public key kept elsewhere |

Client routes: `POST /v1/chat/completions`, `/v1/completions`,
`/v1/embeddings`, `/v1/responses`, `/v1/messages`, `/v1/messages/count_tokens`, `/v1/rerank`, `/v2/rerank`,
`/v1/audio/transcriptions`, `/tokenize`, `/detokenize`, `GET /v1/models`, and `POST /mcp` with `mcp_server` blocks. `GET /healthz` returns 503 when the budget
state is stale and fail-closed is on.

### Fallbacks

A model group can name fallback groups, tried in order:

```hcl
model "llama-70b" {
  pool        = "b300"
  fallbacks   = ["llama-8b"]                  # at most 4; not a guard model, not itself
  fallback_on = ["unavailable", "saturated"]  # default ["unavailable"]
  saturation_waiting = 4                      # optional: also read vLLM's own queue
  # …
}
```

**When: `"unavailable"` (the default).** A fallback is taken before anything has reached the client, in either of two cases:

- no upstream of the group took the request (all down, failing or cooling down, after the usual retries);
- vLLM answered `429` or `503`.

**When: `"saturated"` (since v0.11.0, #58).** vLLM never answers `429`/`503` when it is saturated: it queues the request, as the real-vLLM run of go-llmflow/bench showed. So the gateway decides saturation itself.

- **The pool is saturated** when it already had `capacity` (C) or more requests in flight when this request arrived. With `saturation_waiting = N`, it is also saturated when the group's upstreams report N or more requests waiting in `vllm:num_requests_waiting`.
  - That metric is scraped from each upstream's `/metrics` at the health interval, with the health timeout, and cached.
  - An upstream that cannot be read counts as unknown, never as saturated.
  - vLLM serves `/metrics` without authentication: keep its ports internal.
- **Only a borrowing request moves.** A request within its organisation's share keeps its reservation and queues in its own pool.
- **Only into room.** The request moves to the first fallback whose pool has room, meaning fewer than C in flight. The check and the admission there happen in one step.
- **Once at most.** A moved request is never moved again. If no fallback has room, it simply queues where it is, so two saturated groups that name each other cannot ping-pong.

The request then moves to the next fallback that its team is allowed to use and whose budgets it has not exceeded. Any other answer, a `400` for instance, is final.

**What it keeps.** Fair share and accounting stay exact:

- the request leaves the first pool and **acquires in the target pool**, so it gets its organisation's priority there;
- the body is rewritten for the target's upstream model;
- the request is **charged, budgeted and recorded under the model that served it**, at that model's prices and in its pool.

**Visibility.** The response carries `X-Llmflow-Model: <model used>`, and `llmflow_fallbacks_total{from,to,reason}` counts the moves (`reason` is `unavailable` or `saturated`). Guardrails have already run on the prompt and run on the answer, whichever model produced it.

### Responses API

`POST /v1/responses` is proxied to vLLM's own Responses endpoint (served by vLLM natively; v0.32.0 checked), through the same pipeline as chat:

- authentication;
- the policy, budget and rate checks;
- guardrails;
- fair-share priority;
- the tenant's `cache_salt`;
- accounting from the Responses usage (`input_tokens`, `input_tokens_details.cached_tokens`, `output_tokens`), which a stream carries in its final `response.completed` event.

`priority`, `request_id`, `cache_salt` and the other engine fields are stripped, as they are for chat.

**Stateless only.** Storing responses is opt-in in vLLM: it stores nothing unless the server runs with `VLLM_ENABLE_RESPONSES_API_STORE=1` (vLLM `entrypoints/openai/responses/serving.py`, and observed in the real-vLLM run of go-llmflow/bench). With the store enabled, vLLM keeps every response in the memory of the instance that produced it. Anyone holding its id can then read it back, continue it (`previous_response_id`) or cancel it. Behind a gateway, that state would be shared by every tenant and would live on one upstream among several. So the gateway works as follows, and a deployment that enables the store stays safe:

- it forces `"store": false`;
- it refuses `previous_response_id`, `background`, `conversation` and stored `prompt` with a 400 that says so;
- it does not route `GET /v1/responses/{id}` or `.../cancel`.

Clients send the whole conversation in `input`, as they do with chat completions.

**Guardrails.** Guardrails read the request's `instructions` and its `input` items:

- message text and parts (`input_text`, `output_text`);
- function-call arguments and outputs.

Input items of any other type are refused, since the guardrails cannot read them. In the answer, output message text and function-call arguments are masked. Reasoning items, annotations, log probabilities and every unknown field are removed.

A streamed Responses request under post-call guardrails is refused (400, ask with `"stream": false`). Its events repeat the text in several shapes (`.delta`, `.done`, the final response) that the stream filter does not rewrite yet. Without post-call guardrails, streams pass through unchanged.

### Rerank and tokenization

Four more routes are proxied to vLLM's own endpoints.

`POST /v1/rerank` and `POST /v2/rerank` cover score models (Jina/Cohere v1 and Cohere v2 shapes).
- They go through the full pipeline, with the tenant's `cache_salt`.
- They are charged on the usage's `prompt_tokens`, or `total_tokens` when that is absent.
- Guardrails mask the `query`, the `documents` (strings) and the documents echoed in `results`. Document objects the guardrails cannot read are refused.

`POST /tokenize` and `POST /detokenize` are authenticated, allow-listed and rate limited like every route.
- They are **not charged**: no model runs, only vLLM's tokenizer.
- They are **not guarded**: masking the text would make the token count wrong, and only token ids come back.

A client `chat_template` is stripped from every request. vLLM already refuses one unless the server trusts request templates; the gateway removes it first.

### Audio transcription

`POST /v1/audio/transcriptions` serves Whisper-style ASR models on vLLM. The request is a `multipart/form-data` form (RFC 7578) with these fields:

- `file`;
- `model`, `language`, `prompt`, `response_format`, `temperature`;
- `stream`, `stream_include_usage`;
- `timestamp_granularities[]`, `max_completion_tokens`.

**How the form is handled.** The gateway reads it once, within bounds:

- the file is limited by `limits { max_audio_bytes }`, 25 MiB by default, which is vLLM's own default;
- at most 32 parts;
- each other field at most 8 KiB;
- one file per request.

Before forwarding, the gateway rebuilds the form for vLLM:

- **only** the fields above are kept; `priority`, `request_id` and anything else are dropped;
- the filename is reduced to its base name in a safe alphabet;
- the part's media type is checked.

The form's fields, without the audio, go through the shared pipeline as a JSON view: model resolution, policies, guardrails on `prompt`, and the content log. The content log therefore never holds audio.

**Accounting.** vLLM reports a JSON transcription's usage as a duration (`{"type": "duration", "seconds": N}`). The gateway charges it with the model's `cost { audio_per_hour = … }` (GPU-hours per hour of audio). A stream's token usage, which the gateway always requests, is charged with the token prices.

`response_format = "text"` carries no usage, so the gateway asks vLLM for JSON and answers the plain text itself.

**Guardrails.** They mask the `prompt` field and the transcript. With post-call guardrails, segments and words (which repeat the text piece by piece) are removed from the answer. A streamed transcript goes through the chat stream filter.

The audio itself is not inspected.

### Anthropic Messages API

`POST /v1/messages` is proxied to **vLLM's own Anthropic endpoint**, with no translation in the gateway. vLLM v0.32.0 serves `/v1/messages` natively (`vllm/entrypoints/anthropic`): it converts each request to a chat completion with the same raw request, so the gateway's `X-Vllm-Priority` and `cache_salt` apply.

Coding agents that speak the Messages API can therefore use local models. Point them at the gateway, with a virtual key as the bearer token.

**Accounting.** It follows vLLM's `AnthropicUsage`. There `input_tokens` excludes the cached prompt, so the gateway charges `input_tokens + cache_read_input_tokens + cache_creation_input_tokens` as prompt, of which `cache_read_input_tokens` are cached.

In a stream, the input counts come from `message_start` and the output count from the final `message_delta`.

`/v1/messages/count_tokens` (since v0.12.1) goes to vLLM's own endpoint, which renders the chat template and counts the prompt's tokens, so the count is exact (`{"input_tokens": N}`). Claude Code calls it, and falls back to a character-based estimate without it. It is authenticated, allow-listed and rate limited like `/tokenize`, and it is not charged and not guarded. `cache_salt` and the engine fields are stripped; `chat_template_kwargs` is kept, so the count matches the `/v1/messages` request that carries the same fields.

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

A tool call's input is held until its block ends, so that every JSON layer can be decoded and masked (bounded at 1 MiB). It then arrives as one delta.

### Anthropic as an upstream (opt-in, since v0.12.0)

Several institutions can share **one Anthropic organisation's API usage**
through the gateway, with the gateway's keys, budgets, fair share, audit and
guardrails. The only path is an **Anthropic API key under the Commercial
Terms**:
- pooling Claude Pro/Max subscriptions is forbidden by the Consumer Terms (no credential sharing; no scripts except through API keys; no resale);
- Claude Code's documentation forbids routing requests through Free/Pro/Max credentials on behalf of users ([legal and compliance](https://code.claude.com/docs/en/legal-and-compliance)).

The gateway therefore never relays a subscription. It drops the OAuth
`anthropic-beta` capability and never forwards a client's `Authorization`
or `x-api-key`. This is not legal advice: check your institution's
contracts with your DPO.

```hcl
model "claude-sonnet" {
  provider       = "anthropic"
  upstream_model = "claude-sonnet-5-5"
  pool           = "anthropic"          # a pool of its own; capacity = concurrency allowed at Anthropic
  allow_external = true                 # required: prompts leave the site
  orgs           = ["etabA", "etabB"]   # explicit allow-list (orgs and/or teams); others get 404
  anthropic_betas = ["context-management-2025-06-27"]   # forwarded betas; "*" = any (never an OAuth one)
  # anthropic_versions = ["2023-06-01", "2026-10-21"]   # default; the first is sent otherwise
  cost {                                # USD per MTok, from the price list, with its date
    unit                    = "USD"
    price_date              = "2026-10-26"
    input_per_mtok          = 2
    output_per_mtok         = 10
    cached_per_mtok         = 0.1        # cache reads
    cache_write_5m_per_mtok = 2.5
    cache_write_1h_per_mtok = 4
    # long_context { above_input_tokens = 100000, input_per_mtok = …, … }   # e.g. Claude Haiku 5.5
  }
  upstream "api" { base_url = "https://api.anthropic.com" }
  org_key "etabA" { key_file = "vault:secret/llmflow/anthropic#etabA" }   # one workspace per institution
  org_key "etabB" { key_file = "vault:secret/llmflow/anthropic#etabB" }
  fallbacks   = ["llama-3.3-70b"]       # optional: a LOCAL model on 429/529/outage
  fallback_on = ["unavailable"]
}
guardrail "pii"     { type = "pii",          orgs = ["etabA", "etabB"] }   # required by default
guardrail "secrets" { type = "hide_secrets", orgs = ["etabA", "etabB"] }
```

**Cross-institution isolation.** Anthropic's prompt cache is isolated per
**workspace** on the Claude API: "Caches are isolated by workspace, ensuring
data separation between workspaces within the same organization"
([prompt caching](https://platform.claude.com/docs/en/build-with-claude/prompt-caching)).
This is the timing side channel CVE-2025-46570 describes for vLLM, which
the gateway closes there with `cache_salt`; Anthropic accepts no salt.
- `check-config` requires an `org_key` of its own (a workspace and API key) for every organisation that may use the model, when more than one may and `cache_isolation` is not `"none"`. Two organisations naming the same key file are refused.
- At run time the gateway reads `anthropic-workspace-id` on every answer. If two organisations ever resolve to the same workspace, both are refused with 503 until the configuration is fixed. One request may have shared the cache before that detection.
- With `cache_isolation = "none"`, one key may serve everybody. The residual risk is then a shared cache between institutions: a prompt-cache hit is observable as a shorter time to the first token.

**What is checked before a prompt leaves.**
- `allow_external = true`, an explicit `orgs`/`teams` allow-list, and https (http only to a loopback address, for tests).
- An external model is never a fallback target: prompts do not leave because a local model was busy. It never shares a pool with vLLM models.
- `external_requires_guardrails` (top level, default `true`): every allowed organisation or team needs pre-call `pii` and `hide_secrets` guardrails, masking or blocking and fail-closed. They are checked at configuration time and again on every request, which answers 503 when they are missing.

**Routes.**
- `/v1/messages` (JSON and streaming) only. `/v1/chat/completions` and the other routes answer 400 for an Anthropic model, because Anthropic's OpenAI-compatibility layer "is not considered a long-term or production-ready solution" ([OpenAI SDK compatibility](https://platform.claude.com/docs/en/cli-sdks-libraries/libraries/openai-sdk)).
- `/v1/models` lists the model to the allowed scopes only.
- `/v1/messages/count_tokens` (since v0.12.1) is sent to Anthropic's endpoint with the organisation's key and the same filtered headers. It is not charged: token counting "is free to use" ([Token counting](https://platform.claude.com/docs/en/build-with-claude/token-counting)). It is rate limited per key like every route. The pre-call `pii` and `hide_secrets` guardrails still mask the prompt, because a count also sends it off the site; the count is then that of the prompt `/v1/messages` actually sends. Anthropic rate-limits counting separately from message creation, so a 429 on a count is passed through without starting the organisation's messages back-off, and a messages back-off does not stop the counts. A shared workspace refuses both. An organisation not allowed gets 404.
- A client may authenticate with `Authorization: Bearer llmf-…` or `x-api-key: llmf-…`, as Anthropic SDKs and Claude Code do.

**What reaches Anthropic.** Headers are built from scratch:
- `x-api-key` is the organisation's key;
- `anthropic-version` is the client's if allow-listed, the first allowed one otherwise;
- `anthropic-beta` carries the allow-listed values only (one comma-separated header, which [beta headers](https://platform.claude.com/docs/en/api/beta-headers) accepts);
- no priority, request id or trace context is sent.

The body goes through the same rewrite as for vLLM, so `priority`, `cache_salt`, `vllm_xargs` and the other engine fields are removed, and no salt is added. Back to the client go the provider's `request-id` and `retry-after`, but not its `anthropic-ratelimit-*` headers, which describe the shared organisation's usage. Claude Code's gateway guide asks gateways to forward `anthropic-beta` verbatim ([gateway protocol](https://code.claude.com/docs/en/llm-gateway-protocol)). The allow-list is stricter on purpose: add the betas your users need, or `"*"`.

**Limits and back-off.** The pool's `capacity` is the concurrency the institutions share at Anthropic, apportioned by their shares as for GPUs. A 429 or 529 ([errors](https://platform.claude.com/docs/en/api/errors), [rate limits](https://platform.claude.com/docs/en/api/rate-limits)) starts a back-off **for that organisation**, for `retry-after` seconds (at most 10 minutes; 5 seconds when the header is absent, as on the spend-cap 429). During it, requests are not sent: they go to the model's `fallbacks` (a local vLLM group) when `fallback_on` includes `"unavailable"`, or get 429 with `Retry-After`. Other organisations are not affected, and the upstream is never cooled down for an HTTP status.

**Accounting.** Cost uses Anthropic's usage fields: `input_tokens` (after the last cache breakpoint), `cache_read_input_tokens`, `cache_creation_input_tokens` split by lifetime (`cache_creation.ephemeral_5m/1h_input_tokens`), and `output_tokens`. Each is priced at its own rate, in the model's currency, which a [cost unit](#cost-units-since-v0120) keeps apart from GPU-hours. A stream is charged from `message_start` and the last `message_delta`. Set USD budgets (`"unit":"USD"`) per organisation, team or key.

**Data retention.** Under Anthropic's standard commercial policy, prompts and outputs are retained for a period. Zero data retention is an arrangement per Anthropic organisation, requested from Anthropic's sales team ("ZDR is enabled per organization"). Prompt caching is ZDR-eligible: KV cache representations are "held in memory for the cache TTL and promptly deleted after expiry" ([API and data retention](https://platform.claude.com/docs/en/manage-claude/api-and-data-retention)). Features such as code execution are not covered. Decide with your DPO whether the pre-call masking above is enough for your data.

References opened on 2026-10-26:
- platform.claude.com/docs: api/messages, api/versioning, api/beta-headers, api/errors, api/rate-limits, api/models/list, build-with-claude/streaming, build-with-claude/prompt-caching, about-claude/pricing, cli-sdks-libraries/libraries/openai-sdk, manage-claude/api-and-data-retention;
- code.claude.com/docs: llm-gateway, llm-gateway-protocol.

## MCP tools

`POST /mcp` is an [MCP](https://modelcontextprotocol.io/specification/2026-07-28) server in front of the tool servers of the configuration. It speaks the Streamable HTTP binding of revision **2026-07-28** (per-request `_meta`, mirrored headers, `server/discover`). For older clients it also speaks the initialize handshake of **2025-11-25**, 2025-06-18 and 2025-03-26. A client authenticates with its virtual key, like any other request.

```hcl
mcp_server "github" {
  url        = "https://mcp.example.org/github/mcp"
  token_file = "vault:secret/llmflow/mcp#github"   # bearer sent to the server, from the secret source
  protocol   = "2026-07-28"                        # or 2025-11-25 / 2025-06-18 / 2025-03-26
  timeout    = "60s"                               # per call, 1s to 10m
  max_response_bytes = 8388608                     # per response, 1 KiB to 64 MiB
}
```

- **Tools are named `<server>.<tool>`** (server names are `[A-Za-z0-9_-]{1,32}`). A tool whose name is outside `[A-Za-z0-9_.-]{1,128}`, or whose `x-mcp-header` annotations break the specification's constraints, is left out of the list and logged.
- **Per-team allow-lists, closed by default.** A team with no list sees no tool. An organisation administrator sets the list with `PUT /admin/teams/{team}/tools` `{"tools":["github.*","docs.search"]}`. A pattern is `*`, `<server>.*` or `<server>.<tool>`, and it must name a configured server. Team administrators can read the list but cannot widen it. `tools/list` returns only the allowed tools. A call to any other tool answers `-32602 Unknown tool`, exactly like a tool that does not exist, and the refusal is audited (`tool.denied`).
- **Every call is audited before it is made.** A `tool.call` entry (kind `tool`) names the key, its organisation, team and project, the tool, and the SHA-256 and size of the canonical arguments; the arguments themselves are never stored. If that entry cannot be written, the call is not made (503). A `tool.result` entry follows with the outcome (`ok`, `tool_error`, `input_required`, `server_error`, `unavailable`, `malformed`) and the duration. `llmflow_tool_calls_total{team,server,status}` counts the calls.
- **No server outside the configuration is reachable.** Clients name tools, never URLs. The gateway sends requests only to the `url` of an `mcp_server` block and does not follow redirects. It does not use an environment proxy, and it never fetches anything a server returns (icons, resource links). A URL with credentials, a query or a fragment is refused at load time, and a `token_file` needs https (http only to loopback).
- **Tokens to the tool servers** come from the secret source (`token_file`: a file or an OpenBao/Vault KV v2 field) and are sent as `Authorization: Bearer`. The client's key is never forwarded.
- Tool calls count against the key's RPM and in-flight limits; they cost no GPU-hours.
- The binding is enforced. For a modern request, `MCP-Protocol-Version`, `Mcp-Method` and `Mcp-Name` (Base64 sentinel decoded) must match the body; otherwise the answer is 400 `-32020`. An unsupported version gets 400 `-32022` with the supported list. A browser `Origin` gets 403, and GET or DELETE gets 405. The gateway mirrors `x-mcp-header` parameters into `Mcp-Param-*` headers toward modern servers. Arguments are re-encoded from one parse, so duplicate keys cannot be read differently on each side.
- **Multi round-trip requests** pass through. An `input_required` result goes back to a modern client, and its retry, with `inputResponses` and `requestState`, is forwarded. A legacy client gets an error instead, because it cannot answer.
- Answers are single JSON objects (no SSE toward clients, so no progress notifications); a server's SSE response stream is read to its final response. A server's tool list is cached for a minute, the teams' allow-lists for 5 seconds (a change on the same replica applies at once).

Parsers and the endpoint are fuzzed: `FuzzParseMessage`, `FuzzHeaderValue` (sentinel round trip, no control character in a header), `FuzzHeaderParams`, `FuzzReadResponse` (JSON and SSE), and `FuzzMCPEndpoint`, which checks that the tool server only ever sees allowed tools.

Not covered: OAuth 2.1 discovery (protected resource metadata, RFC 9728). Clients use their virtual key as a static bearer token, which the specification allows as a custom strategy. Also not covered: resources and prompts, and guardrails on tool arguments and results.

## Traces

OpenTelemetry traces, with the official Go SDK, exported over OTLP/HTTP (`/v1/traces`, port 4318 by convention) or OTLP/gRPC (port 4317):

```hcl
tracing {
  endpoint             = "https://otel.example.org:4318"   # grpc: "otel.example.org:4317"
  protocol             = "http"                            # or "grpc"
  sample_ratio         = 0.1                               # parent-based, ratio of new traces
  token_file           = "vault:secret/llmflow#otel"       # optional bearer for the collector
  insecure             = false                             # true allows http:// and plaintext gRPC
  trust_client_context = false
}
```

A request is one server span, `llmflow <route>`. Its children are:

- `auth`;
- `guardrails.pre` and `guardrails.post`, with one `guardrail <name>` span per classifier call;
- `upstream`, the call to vLLM and its response, with a `fallback` event when one occurs.

The **W3C trace context of the upstream span is sent to vLLM** (`traceparent`/`tracestate`, which vLLM's tracing reads). vLLM's own spans therefore join the trace when vLLM runs with `--otlp-traces-endpoint`.

**Client trace context.** By default it is not continued. Every request starts a new trace, as the W3C Trace Context specification advises for an entry point: callers cannot force sampling, and requests cannot be correlated across tenants. `trust_client_context = true` continues it.

**What spans carry.** Only metadata:

- organisation, team and key id;
- model and pool;
- priority and borrowing;
- status;
- token counts and whether they were estimated;
- guardrail types, verdicts and categories.

**Never a prompt or an answer.** A test asserts this. Without a `tracing` block, the tracer is a no-op.

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
  different cache entries. This holds on every route whose content parts
  vLLM parses: chat and completions messages, embeddings (messages, batched
  messages, chat messages in `input`), the Responses `input`, rerank `query`
  and `documents`, and tokenize messages (`FuzzScopeEveryRoute`).
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
  v0.4.3 (fuzzed in its own repository; since v0.4.2 it refuses an `nbf` that wrapped
  past the int64 range and RSA exponents below 3) instead of being looked up
  as a virtual key. That library does the
  JOSE work and its refusals: no `alg: none`, no HMAC algorithm (so the
  issuer's public key cannot be used as an HS256 secret), `iss` compared
  whole, `aud` and `exp` required, bounded clock skew, an optional `typ`
  check (`type = "at+jwt"` refuses ID tokens, RFC 9068 §4), keys fetched over
  https, or over http from a loopback address only (go-authn/oidc's
  deliberate exemption: a loopback connection crosses no network link, which
  is also what lets a provider run on the same host; any other http URL,
  including one reached by a redirect, is refused), and at most one key-set
  fetch per `min_refresh` however many
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
  with unknown fields rejected; bounded accounting queue. Every response has a
  write deadline at `max_request_duration` + 5 s, so a client that stops
  reading cannot hold its answer, slot and grant forever. Store reads on the
  request path give up after 5 s (503), a budget refresh after 30 s, an admin,
  UI or SCIM request after 60 s. Export queues hold at most 256 MiB as well as
  `queue_size` objects.
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

Open defects (v0.9.0; each entry re-checked against the code during the
security audit of 2026-10-26):

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
6. **Accounting drops records only when its bounds are reached**.
   - A record is dropped when the in-memory queue (`queue_size`) is full.
   - A record is lost when the durable spool (`spool_max_bytes`) is full or cannot be written, and the store refuses it too. This is counted in `llmflow_accounting_failed_total` and alerted as `accounting_records_lost`.
   - A record still in the queue when the process dies (at most about 1 s of traffic) is lost. Everything that reached the spool survives a store outage and a crash (see [Durable accounting](#durable-accounting)).
   - Until v0.9.0, a store outage lost every record served meanwhile: 21 of 21 in `TestChaosAccountingOutage`.
7. No TLS termination in the gateway (put it behind a TLS proxy); the admin API
   shares the client listener; `/v1/responses` is served statelessly only
   (see [Responses API](#responses-api)).
8. `C` is static configuration; the gateway does not measure or adapt it.
9. **Admin principals are read from the store on every admin request** (no
   cache): fine for admin traffic, but the admin API fails with the store
   (503, within 60 s at most). Role changes therefore apply at once.
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
    team (this cuts their cached tokens at once on the replica that serves
    the request), change the `map` blocks, or revoke at the identity
    provider; otherwise a token already verified stays cached until its `exp`
    or `key_cache_ttl`.
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
17. **Fallback on saturation is the gateway's estimate.** vLLM queues instead of answering `429`/`503`, so `fallback_on = ["saturated"]` (v0.11.0) decides from two signals: the pool's in-flight count against C, and, optionally, vLLM's `num_requests_waiting` scraped at the health interval. A burst shorter than that interval is judged on the first signal only.
18. **The PDF export links more than it uses.** go-pdfkit/pdfkit v0.19.0's root package imports go-widgets/toolkit for its widget bridge (`Page.AddWidget`), so the gateway binary also carries go-widgets/toolkit, go-crdt/collab, richdoc, and the JPEG 2000 and JBIG2 decoders: 33.8 MB without the exports, 38.0 MB with them (darwin/arm64, `CGO_ENABLED=0 -trimpath`). None of that code is reached from the export: the gateway only draws paths and text. The fix belongs upstream (moving the bridge to a subpackage); until then `govulncheck` also scans those modules.

## Security audit 2026-10-26

An independent, adversarial review of v0.8.0 (commit `b23064a`) by an agent
that did not write the code, with chaos tests on the parts that keep state.

**Scope.** Every package of the module (`gateway`, `openai`, `guard`, `mcp`,
`store`, `audit`, `export`, `secret`, `config`, the CLI) and the modules it
pulls for authentication and storage: go-authn/oidc v0.4.2, go-authn/rp
v0.1.2, go-authn/scim v0.1.0, go-volumes/s3 v0.2.0. The fixes of earlier
reviews were rechecked and hold, with their tests: client priority override,
reservation rounding, aborted-stream billing, key-cache wipe, audit
truncation and rollback (anchors), the hold-back UTF-8 leak, the `\u`-escape
guardrail bypass, Responses statefulness (`store` forced false), the
go-authn/oidc `nbf` overflow.

**Method.**

- Read the threat model and the code, then attacked it. The vLLM sources
  were opened where the gateway relies on how vLLM parses a request
  (vllm-project/vllm `main` at `256d8b4`: `entrypoints/pooling/scoring`,
  `entrypoints/pooling/embed/protocol.py`, `entrypoints/pooling/base/protocol.py`,
  `entrypoints/anthropic/protocol.py`).
- `TestAuthzMatrix`: every admin route of the router (a route without a row
  fails the test) called by 12 principals on one organisation's objects:
  master key, `proxy_admin`, `proxy_viewer`, `org_admin` of the organisation
  and of another one (the cross-organisation attacker), `team_admin`,
  `member`, team and organisation viewers, a user without roles, an
  inference key, a JWT-shaped bearer. Listings are checked for leaks to
  outsiders. The matrix was checked against planted mistakes.
- Chaos: PostgreSQL behind a fault-injecting proxy (down, restarted,
  partitioned), two replicas on one database, an upstream killed and
  restarted mid-stream, a key rotated with requests in flight, the clock
  stepped back, the export target down, clients that stop reading.
- Fuzzing: the 29 existing targets for 10 minutes each, and a new target,
  `FuzzScopeEveryRoute`. `govulncheck` on the module and on the four
  modules above.

**Findings.**

| ID | Severity | Finding | Status |
|---|---|---|---|
| A1 | medium | Multimodal `uuid`s were scoped only on chat and Responses. On rerank, tokenize and embeddings (batched messages, chat `input`) the client's uuid reached vLLM unchanged: one tenant could probe or poison another tenant's cached media. | fixed, #48, #55 |
| A2 | medium | A client that never read its answer blocked its handler forever, holding up to 64 MiB of buffered answer, its key's in-flight slot and its fair-share grant. A few connections exhausted the gateway's memory. | fixed, #49 |
| A3 | high | Any organisation admin could re-enable a user that a proxy admin had disabled, with every role the user kept, `proxy_admin` included (`POST /admin/users` on an existing name). | fixed, #50 |
| A4 | low | A key created while its team was being disabled stayed valid (READ COMMITTED race, and the team's flag was not read on the request path). Disabling a team did not clear the cache of verified JWTs. | fixed, #51 |
| A5 | medium | Export queues were bounded in objects only: with S3 down and content logging on, up to 8 GiB of request logs could pile up in memory. | fixed, #52 |
| A6 | medium | Store calls were unbounded: during a network partition with PostgreSQL, requests with an uncached key hung until their client gave up (no answer after 20 s in the test), and a stuck budget refresh stopped every later one. | fixed, #54 |
| A7 | low | Two replicas started together on an empty database raced on the schema, and one failed to start. | fixed, #54 |
| A8 | low | The admin API, SSO and SCIM answers had no `nosniff`, CSP or frame denial (the UI had them). | fixed, #56 |
| A9 | high | go-llmflow/pii v0.1.0, used by the `pii` guardrail, is quadratic on long runs of digits (1 MiB took 5 to 20 minutes of one core): any key holder of a guarded team could pin a core per request. Found by the performance review. | fixed in pii v0.2.0 (go-llmflow/pii#2), bumped here in #57 |

Verified without finding: no cross-organisation IDOR on the 39 admin routes;
CSRF (synchronizer token, `Origin` and `Sec-Fetch-Site`) on every state change
of the UI and of cookie-authenticated API calls; a fresh session at login,
the login cookie bound to the state, `__Host-` cookies (Secure, HttpOnly,
SameSite=Lax), no open redirect; every outbound URL comes from the
configuration and no client redirects are followed (upstreams, guardrails,
MCP servers, alert webhooks; go-authn refuses redirects to cleartext);
virtual keys, admin tokens, the master key and the SCIM token are compared in
constant time over hashes and are never logged; a fallback keeps the original
organisation's cache salt, uuids and priority (`TestFallbackKeepsTenantIsolation`);
scheduled-rotation targets stay under `vault_prefix/<team>/` with validated
path segments; export keys are validated against traversal; request bodies are
read only after authentication and are bounded. Against a real vLLM, the
end-to-end run of go-llmflow/bench v0.2.0 (same day) confirmed the following:

- a client's `priority`, `vllm_xargs`, `extra_body` and `cache_salt` change
  neither the scheduling order nor the cache isolation;
- prefix-cache isolation per organisation holds. vLLM's own counters showed
  768 of 814 prefix tokens hit within one organisation, and 0 of 814 from
  another.

**Chaos results.**

| Fault | Behaviour |
|---|---|
| PostgreSQL down | An unknown key gets 503, not 401, and is not cached as invalid; a cached key is served until the budgets are 3 refresh periods old, then everything fails closed; everything recovers. |
| PostgreSQL partitioned | 503 within 5 s (A6); fail-closed on time; recovery when the partition heals. |
| PostgreSQL restarted | A call that draws a dead pooled connection fails safe with 503 (one call in the test); the pool recovers by itself. |
| Accounting during an outage | Every record of the requests served during the outage is lost and counted as failed (open defect 6). |
| Two replicas, one database | A concurrent first start works (A7); keys are shared; a revocation reaches the other replica after `key_cache_ttl` (open defect 1); the audit chain stays one verifiable chain under two concurrent writers. |
| Upstream killed mid-stream, restarted | The client gets the partial stream; counters return to zero; the next requests are served. |
| Key rotated with 16 requests in flight | All 16 finish and are charged to the key; the old secret is refused at once and the new one is served. |
| Clock stepped back 2 h | The audit chain still verifies. |
| Export target down | The queue drops beyond 256 MiB and alerts (A5). |
| Clients that stop reading | Released after `max_request_duration` + 5 s (A2). |

**Fuzzing.** The 29 existing targets ran for 10 minutes each on v0.8.0, three
at a time with two workers each. None failed, and no new crasher was found. The
new `FuzzScopeEveryRoute` ran for 60 s with no failure. It caught a planted
regression on its first seed, and it is part of the CI fuzz smoke.

**Dependencies.** `govulncheck` (database of 2026-10-21) finds nothing in the
gateway built with Go 1.27.3. go-authn/oidc v0.4.2, go-authn/rp v0.1.2 and
go-volumes/s3 v0.2.0 declare `go 1.27.1`: built with that toolchain, nine
standard-library vulnerabilities are reachable from them (net/http, crypto/tls,
crypto/x509; fixed in 1.27.2 and 1.27.3). The gateway's binary is not affected
because it is built with 1.27.3. Measured with go-fleettools/modfloor, every
fleet module in the gateway's build graph now declares `go 1.27.3`, and the
gateway requires those releases:

- go-authn/oidc v0.4.3 (#18), go-authn/rp v0.1.3 (#7; v0.2.0 since, for `min_refresh`), go-authn/scim v0.1.1 (#4);
- go-volumes/interface v0.1.0 (#8) and go-volumes/pool v0.1.0 (#14, #15), their first tags, and go-volumes/s3 v0.2.2 (#17, #18). Since gateway v0.10.1 no fleet module in the graph is pinned at a pseudo-version;
- go-llmflow/pii v0.2.1 (#3);
- go-gitsafe/gitsafe v0.17.1, which already declared it.

A consumer building with `GOTOOLCHAIN=local` on an older toolchain is now
refused instead of building unpatched code.

**Residual risks.**

1. Usage served during a store outage was not recorded. **Fixed in v0.10.0**
   by the durable accounting spool: the chaos runs recovered 21 of 21 records
   after an outage, and 21 of 21 after a SIGKILL of the gateway followed by a
   restart, none of them duplicated.
2. SCIM adopts an existing gateway user with the same `userName`, and
   re-enables it if a proxy admin had disabled it by hand. The identity
   provider is already trusted for usernames through SSO.
3. Roles that SSO maps from ID-token claims are fixed at login, for the
   session's lifetime. Stored roles and the disabled flag are read on every
   request.
4. A member may point their own key's scheduled rotation at any field under
   the team's vault prefix, overwriting a secret delivered there for another
   key of the team. That is a denial of service inside the team; it gives no
   read access.
5. Secret files are read whatever their permissions (Kubernetes mounts
   secrets 0644 by default). Keep them in a directory that only the
   gateway's user can read.
6. A tool server's error message (at most 4 KiB) is returned to the MCP
   client: tool servers are configured, so they are trusted.

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
