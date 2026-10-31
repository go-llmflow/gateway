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

`export` has the API's bounds (since v0.13.1): at most 366 days, 100 000
rows and 60 s in the store. An operator raises them explicitly
(`--max-days`, `--max-rows`, `--timeout`). Every run is written to the
audit log (kind `export`, actor `cli:<login>`) before the store is read. A
run with a raised bound does not start when that entry cannot be written;
a run within the defaults prints a warning instead.

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

## High availability: several replicas (since v0.14.0)

Without an `ha` block nothing changes: one replica, the fair-share state in
its memory. With one, several replicas share the pools, and PostgreSQL stays
the only shared component (no etcd, no Redis). First released in v0.14.0;
v0.13.1, tagged afterwards on a later commit, contains it too. Design and discussion:
[#74](https://github.com/go-llmflow/gateway/issues/74).

```hcl
store {
  kind      = "postgres"          # required by ha
  dsn_file  = "/run/secrets/dsn"
  spool_dir = "/var/spool/llmflow"
}
ha {
  replica_id           = "gw-1"             # optional, default the host name
  listen               = "10.0.0.5:9444"    # internal port, replicas only
  advertise            = "10.0.0.5:9444"    # default: listen (an IP literal)
  peer_networks        = ["10.0.0.0/24"]    # owners are dialled only there
  secret_file          = "/run/secrets/ha"  # shared, at least 32 bytes
  lease_ttl            = "6s"               # renewed every third
  heartbeat            = "1s"               # grant reports to owners
  forward_timeout      = "250ms"
  usage_persist        = "10s"
  on_owner_unreachable = "lowest_priority"  # or "reject" (503)
  # optional (since v0.16.0): mutual TLS 1.3 on the internal port
  tls_cert_file        = "vault:secret/llmflow/ha#cert"  # this replica's
  tls_key_file         = "vault:secret/llmflow/ha#key"
  tls_ca_file          = "/etc/llmflow/ha-ca.pem"        # signs every replica
  tls_server_name      = "llmflow-ha"                    # the default
}
```

- **One owner per pool.** Each pool's fair share (in-flight counts,
  reservations, decayed usage) is decided by one replica at a time, the
  holder of the pool's lease row in PostgreSQL. The lease's `epoch` is a
  fencing token (Kleppmann, *How to do distributed locking*): it grows at
  every change of owner. Expiry is computed with PostgreSQL's clock
  (`now()`), never a replica's, and a takeover is one conditional statement,
  so of several replicas racing on an expired lease exactly one wins.
- **No split brain.** The owner decides only while the monotonic time at
  which its last successful renewal *started*, plus 0.9 × `lease_ttl`, is in
  the future; PostgreSQL cannot hand the lease to anyone before that renewal
  expires. An owner cut from PostgreSQL therefore stops deciding before
  another replica can start. Its usage writes are fenced at the storage (a
  transaction that holds the lease row `FOR SHARE` and checks the epoch), and
  a replica that has seen a newer epoch refuses an older owner's answer and
  tells it to step down.
- **Forwarding the decision, not the request.** The other replicas send
  acquire and release to the owner over the internal port and still send the
  request to vLLM themselves: one small round trip per admission (the release
  is asynchronous), the streamed answer does not cross a second gateway, and
  the owner's death does not cut streams it did not serve. Every request and
  answer is HMAC-SHA256-signed (key derived with HKDF from `secret_file`),
  bound to the sender's instance, a sequence number and a timestamp
  (5 minutes of clock difference are accepted); replays are refused with a
  sliding window per instance (RFC 4303, 3.4.3). The owner's address is read
  from the database and dialled only inside `peer_networks`, without
  redirects, with `forward_timeout`; bodies are bounded. With the three
  `tls_*` files (secret references, all three or none) the channel runs
  over mutual TLS 1.3: each replica presents its certificate, accepts only
  peers the CA signed, and checks that the server's certificate carries
  `tls_server_name` (replicas are dialled by IP). The three are PEM, read
  over several lines (files, or OpenBao/Vault fields): the certificate file
  may carry intermediates after the leaf, the CA file may bundle several
  CAs, the key is one PEM block (PKCS #8, EC or RSA; ECDSA and Ed25519
  tested) and its file must not be readable by group or others (`chmod
  600`). `check-config` also refuses a certificate outside its validity.
  v0.16.0 read them with the one-line secret reader and refused every real
  PEM file; fixed in v0.16.1. The HMAC signatures stay
  (they bind each answer to its request and refuse replays). The cost is a
  handshake per kept-alive connection, then AES-GCM on bodies of a few
  hundred bytes; `BenchmarkAdmission/forwarded-mtls` measures it (not
  measured yet: machine load above 4). Tested: a peer without a
  certificate, with one from another CA, speaking plain HTTP, or expecting
  another server name gets nothing, although it holds the HMAC secret.
- **Failover.** Every replica reports its live grants to the owner each
  `heartbeat`. A new owner (epoch + 1) restores the decayed usage the
  previous one persisted every `usage_persist` (decayed by PostgreSQL's
  `now() - updated_at`, so the ranking survives), adopts the grants the
  surviving replicas report, and counts the previous owner's last persisted
  in-flight requests as **phantoms**: a safety margin that expires after
  `limits.max_request_duration` and shrinks as reports account for them.
  The grants of a replica that stops reporting for 3 heartbeats become
  phantoms too. A graceful stop persists the usage and ends the lease at once.
- **Without a reachable owner** (the seconds of a failover), a request is
  admitted at the lowest borrowing priority (vLLM never lets a waiting
  request preempt a running one and preempts the largest priority first, so
  it cannot take anyone's reservation), or refused with 503 under
  `on_owner_unreachable = "reject"`. Such a request cannot move to a fallback
  on saturation (the pool's room is unknown).
- Metrics: `llmflow_ha_owner`, `llmflow_ha_epoch`, `llmflow_ha_local_grants`
  (per pool), `llmflow_ha_unowned_total`, and `llmflow_inflight_phantoms`
  (per pool and organisation). The fair-share gauges are published by the
  owner only.
- **SSO logins and sessions** live in PostgreSQL: a login started on one
  replica finishes on another, and a session outlives the replica that
  created it. The row key is the SHA-256 of the cookie (or of the login
  state), the content is sealed with AES-256-GCM under a key derived from
  `secret_file` (a fresh nonce per seal, the kind and hashed id as
  associated data, so a sealed value moved to another row does not open),
  and expiry is PostgreSQL's clock. Logout or a disabled user ends the
  session on every replica.
- **Soft-budget alerts** go through an outbox table, one row per alert and
  channel (unique on the `Idempotency-Key` and the channel, so two replicas
  queue an alert once). Every replica delivers due rows, claimed with
  `FOR UPDATE SKIP LOCKED` and hidden for 2 minutes while one sends them; a
  failure is retried with the same backoff as before (6 attempts), by
  whichever replica claims it next, across restarts. Delivery is at least
  once: a replica that dies after sending and before recording it leaves the
  alert to be sent again after the 2 minutes, with the same
  `Idempotency-Key`.
- **Key revocations** (a key revoked or rotated, a team, project or
  organisation disabled) are broadcast with `NOTIFY llmflow_keys`; every
  replica `LISTEN`s and drops the cached identities at once (measured: 3 ms).
  PostgreSQL does not keep notifications for a listener that is not
  connected, so a replica whose listener reconnects drops its whole key
  cache; `key_cache_ttl` stays the bound for anything else. A silent
  connection is found by a ping every 5 s.
- `check-config` validates the block: the postgres store, an advertised IP
  literal inside `peer_networks`, `heartbeat` at most a third of `lease_ttl`,
  `forward_timeout` at most a heartbeat, and a secret of at least 32 bytes.

Chaos tests (real PostgreSQL 18, 2026-10-26):

| Fault | Result |
|---|---|
| Owner killed with SIGKILL (by PID) mid-traffic, 3 requests of one organisation in flight on two replicas (`TestHAOwnerKilled`) | survivor owner after 1.3 s (`lease_ttl` 1.5 s), epoch 1 → 2; its 2 requests adopted, the dead owner's one counted as a phantom, so the organisation stays at its reservation until `max_request_duration`; 207 requests served meanwhile, 0 failed (203 at the lowest priority before the takeover) |
| Owner partitioned from PostgreSQL (blackhole, `TestChaosHAPartitionedOwner`) | stops deciding 804 ms after the cut (`lease_ttl` 900 ms), the other replica owns from 904 ms, epoch + 1; 0 of 805 samples with two owners; after the heal it forwards to the new owner and does not take the lease back |
| Two replicas racing for a lease (8 claimants × 20 rounds on PostgreSQL; two replicas started together) | exactly one owner each time, epoch + 1 |
| Clock skew ± 2 min on one replica | ownership does not move, forwarding works (0 admissions without owner), the skewed replica takes over normally |
| A key revoked, then a team disabled, on one replica while cached on the other (`key_cache_ttl` 1 h, `TestChaosHARevocationNotify`) | refused on the other replica after 3 ms; a revocation made while that replica was cut from PostgreSQL is applied when its listener reconnects (the test fails when the reconnect flush is removed) |
| SSO login started on replica A, callback on B, B killed, session used on a new replica C (`TestChaosHASSOSessionSurvives`) | the session is served by C; the state cannot be replayed on A; the row holds neither the cookie nor the user name, and does not open under another id |
| The same soft-budget alert queued by two replicas, the webhook failing once (`TestChaosHAAlertOutbox`) | one delivery, after one failed attempt |
| Property: random partitions, kills and restarts of 4 replicas under traffic, 8 s (`TestPGPropertyOneOwnerPerEpoch`) | 14 000 to 27 000 decisions over 5 to 7 epochs per run: never two owners at one epoch, and the decisions of an epoch always end before the next epoch's first one; the checker fails on planted violations |

Added latency of a forwarded admission: not measured yet (the machine's load
was above 4); `BenchmarkAdmission` in `ha/` compares a single replica, the
owner and a forwarded admission.

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
| `alerts { webhook_url, webhook_token_file, smtp { server, from, to, username, password_file, tls, timeout, allow_cleartext } }` | where soft-budget alerts go; the mail session requires TLS by default (see "Mail transport") |

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

## Project attribution, labs and users (v0.12.1)

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
calendar; weeks start on Monday, as ISO 8601 weeks do) or per rolling window
(`rolling:5h`, since v0.18.0, see [Workload profiles](#workload-profiles-since-v0180)),
optionally for one model group only:

```json
{"kind": "team", "name": "lab-nlp", "model": "", "period": "month", "max_cost": 500, "soft_limit": 400}
```

| `kind` | `name` | set by |
|---|---|---|
| `org` | organisation | proxy admin |
| `team` | team | the organisation's admins |
| `project` | `team/project` | the team's admins |
| `key` | key ID (with `model`: a per-model budget of that key) | the team's admins |
| `user` | the person, as the reports name them: a key's owner, or a JWT's user (since v0.17.0) | the admins of an organisation or team the user belongs to; the user reads their own |
| `tag` | tag | proxy admin |

A request is refused (429, the message names the scope) when one budget it
falls under cannot hold its worst-case cost (see [Hard budgets](#hard-budgets-since-v0170)):
its organisation, team, project, key, user and every tag it carries, each
for all models and for its own model. A user budget follows the person
across all their keys and tokens. Tags come
from the key (`"tags"` at key creation) and from one allow-listed request
header (`tags { header, allowed }` in the configuration; at most 8, others
refused with 400).

`soft_limit` triggers one alert per budget and period, to a webhook (JSON
POST, optional bearer token from a file) and/or by e-mail through an SMTP
relay (`alerts { … }`); the "sent" mark is stored with the budget, so several
gateways and restarts send it once. A temporary increase raises the limit
until its expiry, whatever the period; setting the budget again keeps it.

The `spent` of `GET /admin/budgets` is the stored records of the period
plus what this gateway charged since (soft alerts use it). `committed` and
`held` are the period's ledger: the cost of the requests settled under the
budget, and the grants the replicas hold for the requests in flight.

### Hard budgets (since v0.17.0)

**A budget is never exceeded.** A request starts only when every budget it
falls under can pay its worst case, as Slurm's
`AccountingStorageEnforce=safe` starts a job only "if the job will be able
to run to completion" ([resource limits](https://slurm.schedmd.com/resource_limits.html)).

**The worst case W.** vLLM refuses an input longer than `max_model_len` and
generates at most `min(max_tokens, max_model_len − input)` per choice
(`get_max_tokens`). With `L = max_model_len`, `M` the `max_tokens` sent and
`n` choices, per prompt:

    W = max((L − M)·p_in + n·M·p_out, L·p_in)

where `p_in` is the highest input price that can apply (input, cached,
cache writes, either tier) and `p_out` the highest output price. Completion
prompt lists, embedding inputs and rerank documents count one prompt each.
For an Anthropic model, `W = context_window·p_in + M·p_out`. Costs are
integers of nano-units: no float drift in `spent + held ≤ limit`.

**max_tokens.** Under a budget the gateway always sends one:
- the client's, never lowered: when its W does not fit, the request is refused (429 `insufficient_quota`, naming the scope, the room left and the request's maximum cost);
- when the client sent none, the largest that fits, from the model's `max_output_tokens` (or the context length) down to `budgets.min_max_tokens`; the response says it in `X-Llmflow-Max-Tokens`.

Every answer under a budget carries `X-Llmflow-Budget-Reset` (RFC 3339): when the
budget that refused resets, or, on success, when the tightest one does.

**Refused under a budget** (400, with the reason), since nothing bounds their cost:
- Responses built-in tools (the server generates again after each call);
- Anthropic server tools, `mcp_servers` and `container`;
- the `compact-*` betas (their compaction step is missing from the top-level usage), and a `context-1m-*` beta wider than `context_window`;
- transcriptions of a priced model (priced per audio duration).

A model whose context length is unknown is refused under a budget (503), rather than guessed.

**The escrow.** PostgreSQL holds, per budget and period, `spent` and, per
replica process, a grant; every transaction keeps `spent + Σ grants ≤
limit`. A replica admits locally while its grant covers what it has in
flight: no database round trip per request. A grant that runs short grows in
one transaction, all of the request's budgets or none, rows locked in one
order. It grows by the larger of what is missing and `grant_share` of the
room left, so near the limit grants shrink to one request. An unused grant
goes back after `grant_idle`. Storing a request's record settles its hold in
the same transaction (`spent += actual`, `grant −= min(actual, W)`); a record
replayed from the spool settles nothing twice. A request that costs more
than its W is charged anyway and counted in
`llmflow_budget_hold_exceeded_total`; alert on any. The estimate of an
aborted stream is capped at W.

**Crashes.** A dead process's grants are never released on a timer, since its
spool may still hold records. The process that replays the spool releases
them once every record is stored. A replica that never comes back is
released by `POST /admin/budgets/release-replica {"replica": "<name>"}`
(audited). A proxy admin releases all of it: its grants and its
registration, which an arming budget of any organisation waits for. An
organisation admin releases only its grants on that organisation's budgets
(`"unregistered": false` in the answer).

**Limits that move.**
- Lowering a budget below what the period committed is refused (409) unless `"force": true` (proxy and organisation admins only, audited). A forced lower stops new admissions; what was admitted finishes. Other replicas see it at their next budget refresh.
- A temporary increase is lent only to requests that end before it does (`limits.max_request_duration`).
- A request is charged to the period that admitted it.

**A budget created mid-period (since v0.17.1).** The requests in its scope that were admitted before it existed hold nothing on it, yet their cost lands in it. So a new budget is created **arming**:
- Every gateway process keeps, for every request it admitted (budget or not), its worst case W until its record is stored.
- On loading the new budget (at once on the replica that created it, by NOTIFY on the others under `ha`, at the latest at `budget_refresh`), each process reports the W it still has unsettled in the budget's scope, and lowers the report as those requests settle. Their actual cost goes to the budget's `spent` when their records are stored.
- No grant is made until **every registered process** has reported. Until then, requests in the scope get 429 with "was just created and is arming … retry in a moment". After that, the reports count against the limit (`spent + Σ grants + Σ reports ≤ limit`) until they are all back to 0, and the budget is then active.
- A process that died without a clean shutdown stays registered, so the budget stays arming for its scope until the process that replays its spool releases it, or a proxy admin releases its replica (`POST /admin/budgets/release-replica`, audited). Never on a timer.
- An `org` or `team` budget created over a scope with heavy traffic may therefore refuse for a moment after its creation.
- The starting spend is read with the usage inserts blocked for the moment (`LOCK TABLE … IN SHARE MODE`), so no record falls between it and the later settlements.

What no admission control can refuse is the work admitted before the budget existed, or by a replica in the instant before it loaded the budget. The guarantee is that what the budget admits never takes the period past its limit, or past the cost of that earlier work when it alone is larger.

```hcl
budgets {
  grant_share    = 0.1   # share of the room left a refill takes (0..1]
  grant_idle     = "5s"  # an unused grant goes back after this (100ms..10m)
  min_max_tokens = 16    # the smallest max_tokens the gateway picks
}
model "llama" {
  max_model_len     = 32768  # else read from the upstreams' /v1/models
  max_output_tokens = 4096   # caps max_tokens (a larger one: 400)
}
model "claude" {
  provider       = "anthropic"
  context_window = 1000000   # required for a budget to admit it
}
```

When `max_model_len` is not configured, the gateway reads it from each
upstream's `GET /v1/models` at health check, and sends a held request only to
an upstream whose length it has read since that upstream last became healthy.
**Set `max_model_len` on every model a budget can apply to.** It is then never
learned late, for example from a Slurm instance that just joined.

`check-config` validates these fields, bounds every price to 1e6 per million
tokens (so the integers cannot overflow), and refuses a context beta that is
wider than `context_window`.

**Edges.**
- A forced lower (`"force": true`) is an explicit admin act. Like Slurm's "safe" limits, what was already admitted finishes.
- A record dropped by a full accounting queue is not billed, as before. Its hold stays reserved until its replica restarts or is released, which errs on the safe side.
- With `store.fail_closed = false`, a replica that cannot load the budgets keeps admitting without them: the guarantee needs the default, fail-closed.

**Migration (v0.16 → v0.17).**
- Start-up adds `llmflow_budget_ledger` and `llmflow_budget_grants`, `limit_epoch` on `llmflow_budgets_v2`, and `cost_nano` and `admitted_at` on `llmflow_usage`. Each period's ledger starts from its stored usage.
- Budgets that admitted while under their limit now admit only what fits at the worst case: set `max_tokens` (or `max_output_tokens`) to keep small budgets usable.
- Spool files written by v0.17 carry the holds and are not readable by v0.16: drain the spool before a downgrade.

**Migration (v0.17.0 → v0.17.1).**
- Start-up adds `llmflow_budget_holders`, `llmflow_budget_arming` and `arming` on `llmflow_budgets_v2`. Existing budgets stay active.
- Upgrade every replica before creating a budget. A v0.17.0 process does not register, so its unheld requests are not reported.
- A stored record now counts in every budget of its scope, held or not (it is how the cost of the work admitted before a budget lands in it).

### Workload profiles (since v0.18.0)

An administrator, often in finance or HR rather than IT, gives each person a
**workload profile in working days per week** instead of a money budget:
¼ day, ½ day, 1 day … 7 days a week. The gateway turns the profile into hard
budgets, so **a profile is never exceeded**.

**The unit: a workday.** A workday is what one person working with the
assistant for a full day consumes, in each cost unit: GPU-hours for the local
pools, and a currency for each external provider. It is **measured, not
guessed**. `/ui/profiles/calibrate` (or `GET /admin/profiles/calibrate?weeks=4`)
shows the distribution of the cost of every *active person-day* (a person
with any cost that day) over the last weeks, and proposes its 75th
percentile. A proxy admin accepts a value (`POST /admin/profiles/calibrate
{"unit": "USD", "amount": 13}`, audited), and every profile follows at once.
For reference, Anthropic reports about $13 per developer per active day for
Claude Code across enterprise deployments
([costs](https://code.claude.com/docs/en/costs)), but your own measurement
is what counts. An accepted calibration takes precedence over a `workday`
block, which only gives a starting value. **A priced request in a unit
without a workday is refused (503) for a person under a profile**: nothing
could hold its cost against the profile.

**What a profile becomes.** For a person with a profile of *d* days, in every
unit that has a workday:

| budget | period | limit |
|---|---|---|
| `profile_week` | the ISO 8601 week, reset Monday 00:00 UTC (as every calendar budget) | *d* × workday |
| `profile_session` | a rolling window, `session_window` (5 h) | `session_share` × workday |
| `profile_overflow` | the calendar month, only when overflow is allowed for the person | `overflow_cap` × workday |

The session window stops one person from burning the week in one sitting, as
the five-hour session limit of Anthropic's plans does
([Max plan](https://support.claude.com/en/articles/11049741-what-is-the-max-plan),
[Team plan](https://support.claude.com/en/articles/9266767-what-is-the-team-plan)).
The profiles do not borrow those plans' names: their ratios (Team Premium is
6.25× Pro per session against 1.25× for Standard) do not match a day scale.
The labels default to the day count (`"2 days / week"`) and can be changed.

**Overflow.** It is the analogue of usage credits
([manage usage credits](https://support.claude.com/en/articles/12429409-manage-usage-credits-for-paid-claude-plans)),
which Anthropic caps per organisation, per seat type and per person. When
the week or the session refuses a request and an administrator allowed
overflow for that person, the request is admitted on the profile's monthly
`overflow_cap` instead. Such a request is marked `overflow` in its record,
counts for the overflow budget only, and still for every other budget of its
scope (team, organisation, project…). An organisation-wide cap is an
ordinary `org` budget. Without overflow, the request is refused.

**Refusals** are 429 `insufficient_quota` with `X-Llmflow-Budget-Reset`. The
message is in workdays and names only the caller's own allowance:

    weekly allowance of 2 days used; resets Monday 2 Nov 00:00 UTC
    session allowance of 0.5 day per 5h: 0.1 day left, and this request may cost up to 0.2 day (send a smaller max_tokens, or wait: room again from Wednesday 28 Oct 15:35 UTC)

For the session window the header gives when enough has left the window for
the request, not the end of a period.

**The rolling window under the escrow.** A rolling budget keeps its spend in
5-minute buckets. Its spent is the sum of the buckets overlapping the window,
so it counts at most 5 minutes more than the window, never less. Spent in a
rolling window only falls as time passes, so `spent + Σ grants ≤ limit`,
true when a grant is made, stays true. A request counts in the bucket of its
**end**, not its admission: the time of admission is read before the hold,
which may draw on a grant grown later, whose room had already dropped buckets
that a window ending in between still counts. The property test found that
overshoot (13.4 for a limit of 12) before the rule was in place. Rolling
windows are a general budget period, `"period": "rolling:5h"` (5m..168h, in
whole 5 minutes; no `soft_limit`). Replica clocks must be in sync, as they
must be for calendar periods.

**Who assigns.**
Who a person belongs to is decided by their **team bindings** (which SSO and SCIM also create), as in the
[v0.18.1](#security-fixes-in-v0181) and [v0.18.2](#security-fix-in-v0182-who-may-place-a-person-in-an-organisation-104) security sections.
- A proxy admin assigns anyone.
- An organisation admin assigns a person all of whose team bindings are in their organisation; a team admin, a person all of whose team bindings are in teams they administer.
- An organisation-level binding (`viewer`, `org_admin`) is authority, not membership: it gives no access to the person, whoever granted it. A team binding that an organisation admin granted does not make the person theirs either (it is ignored by the access rules; SCIM, an SSO claim or a proxy admin make one that counts).
- A person with a binding in another organisation, a proxy role, or no team binding at all (a JWT's user, or one only an org admin placed) is the proxy admins' only: **the first grant to a binding-less user, or to another organisation's user, needs a proxy admin**. A profile limits the person everywhere, so one organisation does not decide it for another.
- People read their own profile, and administrators and viewers read the people of their scope.

Every assignment is stored with its history (who, when, which profile) and
audited.

```
PUT    /admin/users/{user}/profile   {"profile": "2d", "overflow": false}
DELETE /admin/users/{user}/profile   # back to default_profile, or none
GET    /admin/users/{user}/profile   # the person's week and session, in days, and the history
GET    /admin/profiles               # the profiles, the default, the workday in force per unit and its source
GET    /admin/profiles/people        # ?org= or ?team=: everyone the principal may read
GET    /admin/profiles/capacity      # see below
GET    /admin/profiles/calibrate     # ?weeks=1..26 (proxy roles)
POST   /admin/profiles/calibrate     # {"unit", "amount"} (proxy admins)
```

The profile budgets are the gateway's: the budgets API neither lists nor
changes them (`PUT /admin/budgets` with a `profile_*` kind: 404). A person with
`default_profile` gets their budgets at their first request. That request
waits up to 3 s for them to arm, and only once.

**The admin UI.** `/ui/profiles` shows each person's week as a bar in days
("1.3 / 2 days") with when it resets, their session, and their overflow, and
offers the assignment form to those who may assign. `/ui/profiles/capacity`
is the check a manager needs. It compares the days assigned each week to an
organisation's people with what its share of the pools serves: share × C ×
the measured cost per request-slot-hour (the last 7 days of records, cost ÷
busy time) × 168 h, in workdays. It shows the oversubscription ratio as a
seat count would ("assigned 140 days a week for a capacity of about 90 →
×1.6"). Oversubscription is normal, since people do not all work at the
same time and fair share arbitrates when they do. A pool without measured
load says "not measured" rather than inventing a figure. An external
provider has no capacity: it is billed per request.

**Reports** gain a `profile` dimension (`group_by=profile`, the reports page,
and the CSV, XLSX and PDF exports): the profile in force when each request was
admitted, so a change of profile never rewrites history.

**Configuration** (`check-config` validates it):

```hcl
workday {               # optional starting values; a calibration accepted in the UI wins
  gpu_hours = 0.8
  USD       = 13        # one attribute per unit, as the models spell it
}
profile "q" {           # id: [a-z0-9_-]{1,32}; at most 64 profiles
  days          = 0.25  # 0..7 days a week
  session_share = 0.25  # (0, 1] workday per session window
}
profile "2d" {
  label         = "2 days / week"  # default: the day count; at most 64 printable bytes
  days          = 2
  session_share = 0.5
  overflow_cap  = 2     # optional, 0..217 workdays a month beyond the profile
}
default_profile = "q"   # optional: everyone without an assignment
session_window  = "5h"  # 5m..24h in whole 5 minutes
```

HCL allows one attribute only in a block written on one line
(`profile "q" { days = 0.25 }`): write one attribute per line.

**Migration (v0.17 → v0.18).**
- Start-up adds `llmflow_budget_buckets`, `llmflow_profile_assignments`, `llmflow_workdays`, and the `profile` and `overflow` columns of `llmflow_usage`. Existing records have no profile and are not overflow.
- Nothing changes without `profile` blocks.
- With profiles, calibrate a workday for every unit a person under a profile uses before setting `default_profile`. Until then, their priced requests in that unit are refused.
- Upgrade every replica before assigning profiles. A v0.17 replica does not know the profile kinds: it admits without holding on them, and the arming of each new profile budget covers its requests only until their records are stored.
- Fixed on the way: a budget deleted and created again now re-reads its period's spend from the records. Records stored while it did not exist were missing from it.

### Fluids: measured energy, attributed to consumers (#97)

Electricity and cooling are re-billed on **measured** consumption, not on a
guess. Name the NVIDIA DCGM exporter of each node on its upstreams and price
the energy:

```hcl
fluids {
  pue           = 1.4        # facility energy / IT energy, 1..5 (default 1)
  price_per_kwh = 0.18
  currency      = "EUR"      # required with a price; never summed with GPU-hours
  idle_energy   = "by_share" # or "by_usage" (default by_share)
  w_prefill     = 1          # relative energy of a prefill and a decode token
  w_decode      = 1          #   (a modelling choice: calibrate on your models)
  idle_watts    = 0          # per GPU; energy up to it is idle (0: none)
  interval      = "15s"      # scrape period
  settle        = "1m"       # wait before attributing, so accounting has landed
  period "2026-07" { price_per_kwh = 0.21 }   # per-month override (pue too)
}
model "llama" {
  upstream "gpu01" {
    base_url = "http://10.0.0.1:8000"
    dcgm_url = "http://10.0.0.1:9400/metrics"
  }
}
```

- **Source.** `DCGM_FI_DEV_TOTAL_ENERGY_CONSUMPTION` is a counter "since the
  driver was last reloaded (in mJ)" (dcgm-exporter `etc/default-counters.csv`).
  The gateway turns it into per-interval deltas per GPU. A counter that goes
  down (driver reload) counts its new value as the energy since the reload and
  is counted in `resets`; a failed scrape loses nothing, since the next delta
  spans the gap; a GPU seen for the first time only sets a baseline. Energy used
  while the gateway is down is not counted. The URL follows the rules of
  `base_url` (http or https, no credentials, no query), is read with a timeout
  and a bounded body (8 MiB), and **follows no redirect**. The Prometheus text
  parser is fuzzed. Several upstreams on one node share its `dcgm_url`; Slurm
  discovered members are not metered yet.
- **Attribution.** All arithmetic is in integer millijoules, so what is
  attributed adds up exactly to what was measured (property-tested). In an
  interval, *active* energy is split between the requests that finished in it by
  `prefill tokens × w_prefill + decode tokens × w_decode` (prefill tokens are the
  prompt minus cached tokens). *Idle* energy (an interval with no work, or the
  `idle_watts` floor) is allocated over a month by the agreement: `by_share`
  pro rata to the organisations' shares, `by_usage` to their energy over the **whole calendar month** (a report over part of
  a month splits that part's idle energy as the month's report would, so the parts
  add up to the month); energy no
  one can be charged for is `(unallocated)`, visible to the proxy roles only.
  An organisation's idle energy is spread over its rows pro rata to their active
  energy, so every grouping adds up to the measured total.
- **Cost** is `kWh_IT × PUE × price_per_kWh`, priced per month (a `period` block
  overrides PUE and price for one month).
- **Report.** `GET /admin/fluids?from=&to=&group_by=&format=json|csv` with
  `group_by` in org, lab, team, project, user, model, pool, day. Columns:
  `gpu_hours`, `kwh_active`, `kwh_idle`, `kwh`, `fluids_cost`, `currency`: three
  units in three columns, never added. Proxy roles read every organisation;
  `org_admin` and org-level `viewer` read their own; team-level roles get 403.
  Periods are capped at 366 days. Stored in PostgreSQL as one row per interval
  (`llmflow_energy_intervals`) and per consumer (`llmflow_energy_active`).
- **Batch.** `energy.ParseSacct` reads `sacct -P -o Account,ConsumedEnergyRaw`
  (joules; sacct.html). It refuses a unit-prefixed `ConsumedEnergy` (the page
  does not say the base) and skips job steps. Slurm notes the value is a job's
  real energy only for an exclusive allocation. It is not yet wired into the
  reconciliation sheet.

### Guaranteed share against use, and the lending balance (#97)

`GET /admin/lending?from=&to=&format=json|csv` settles the first two rules of a
shared-GPU agreement from the period's finished requests (package `lending`):

- **Guaranteed capacity**: reserved slots × hours of the period. The
  reservation is `fairshare.Apportion(C, shares)`, the same slots the admission
  test uses, with **C as it was at each instant**: the gateway records every
  change of a pool's C (its configured capacity plus its Slurm-discovered
  instances) in `llmflow_pool_capacity`, and the report integrates that
  history. A pool with `capacity = 0` fed by Slurm jobs is guaranteed what its
  instances made it. A period before the history started falls back to the
  configured capacity.
- **Used in share** and **borrowed** GPU-hours: the request's `Borrowing` flag,
  decided at admission.
- **Lent**: borrowing is served by capacity others reserved and did not use at
  that moment. At every instant a borrower's GPU-hour rate is split between the
  lenders in proportion to their idle reserved slots (reservation minus in-share
  requests in flight). `borrowed_from_others` is what came from other
  organisations; `borrowed_self_served` ran while the borrower's own reservation
  was idle again; borrowing that found no idle reservation anywhere is
  `unattributed_gpu_hours` (proxy roles), never dropped.
- **Net** = lent − borrowed from others; the nets of all organisations sum to 0.
  The **lending matrix** (lender × borrower, GPU-hours) is returned to the
  proxy roles only.
- **Wait**: each record now carries its **admission wait** (`wait_ms`): the
  time between the request's arrival and the fair-share grant, in the gateway's
  queue. A request holds its slot from the grant to its end, so the lending
  interval is `[end − latency + wait, end]`: waiting in the queue is not holding
  a reservation. The report gives p50/p95/p99 of the admission wait of in-share
  against borrowing requests (`wait_in_share_ms`, `wait_borrowing_ms`): the
  evidence that the guarantee held, next to the end-to-end latency
  (`latency_*`). The time a granted request spends in vLLM's own queue is not
  recorded. Records from before this release have `wait_ms = 0`.

A request belongs to the period it finished in; a period is capped at 366 days
and 500 000 requests (HTTP 413 beyond). The heavy reports (lending, chargeback,
decision) read the store under `ReportTimeout` and at most 2 run at once: the
next caller gets 429 with `Retry-After`. `org_admin` and org-level `viewer` read
their own row, not the matrix; team roles get 403. The identities above are
property-tested against the real `fairshare.Pool` driven on random timelines.

### Soft quotas: warnings before a window runs out (since v0.19.0)

The hard budgets never let a window be exceeded; soft quotas tell people
**before** it runs out, the way Anthropic does for its five-hour session and
weekly limits. By default a person is warned once past **75 %** and again past
**95 %** of a window (Anthropic's values:
[usage warnings in Claude Code](https://code.claude.com/docs/en/claude-apps-gateway-spend-limits.md);
[usage limits](https://code.claude.com/docs/en/errors.md#usage-limits)). The
windows are the profile's *session* (rolling, 5 h) and *week* (ISO week), and a
money budget with a user scope.

**What is not done.** Claude Code's *own* warnings and `/usage` ring read the
`anthropic-ratelimit-unified-*` response headers. Their exact format is
documented only in a Claude apps gateway's own `<public_url>/protocol`
reference, which is not public, so **this gateway does not emit them** and
guesses nothing. A follow-up adopts them if a primary source documents the
format. Until then the warnings reach people through the channels below, which
the gateway controls.

**Channels**

| | |
|---|---|
| response headers | on every successful response, for the caller's own windows only: `X-Llmflow-Usage-Session: 85`, `X-Llmflow-Usage-Session-Reset: <RFC 3339>`, `X-Llmflow-Usage-Week`, `X-Llmflow-Usage-Week-Reset`, and, once a threshold is passed, `X-Llmflow-Usage-Warning: You've used 85% of your session allowance · resets 15:45` (French with `Accept-Language: fr`) |
| `GET /v1/me/usage` | authenticated with the caller's own key or token, rate limited (60 a minute per key), `Cache-Control: no-store`, served from memory: windows, percentages, reset times, the profile and the thresholds in force |
| `llmflow-status` | a small cobra program (`cmd/llmflow-status`) for Claude Code's `statusLine`, below |
| notifications | at each threshold, once per window: a webhook (with its `Idempotency-Key`) and/or mail from the `alerts` block. The mail goes to the person when their user name is an address (an SSO or SCIM user's is) and, with `notify = ["user", "team_admin"]`, to the administrators of their teams. They go through the soft-budget alert outbox: at least once, shared by the replicas |
| admin UI | `/ui/softquotas`: who is above a threshold in the current windows, and the thresholds of each level |

**The hard limit** now speaks as Anthropic's does: `429` with
`You've hit your session allowance · resets 15:45` (or `weekly`; French with
`Accept-Language: fr`), `retry-after` in integer seconds, the existing
`X-Llmflow-Budget-Reset`, and `x-should-retry: false`. Claude Code stops
retrying above 60 seconds and shows the message at once
([gateway protocol, response headers](https://code.claude.com/docs/en/llm-gateway-protocol.md)).

**Thresholds.** Configured from the most general level to the most specific,
which wins:

```hcl
soft_quotas {
  warn_at  = [75, 95]          # the gateway; Anthropic's values when omitted
  language = "fr"              # of warnings when a request names none (en by default)
  timezone = "Europe/Paris"    # the zone reset times are spelled in (UTC by default)
  notify   = ["user", "team_admin"]   # who is mailed (user by default)
}
org "etabA" {
  share = 0.8125
  soft_quotas {
    warn_at = [80, 95]         # one organisation
  }
}
profile "2d" {
  days = 2
  session_share = 0.5
  soft_quotas {
    warn_at = [50, 75, 95]     # one profile
  }
}
```

A user budget takes `warn_at` from the budgets API (`PUT /admin/budgets`,
`"warn_at": [60]`, user scope only). Resolution: **gateway → organisation →
profile → budget** (the budget's wins). **1 to 5 thresholds, integers from 1 to
99, strictly increasing; `[]` turns warnings off for that level.** `check-config`
refuses the rest. An organisation administrator sets their organisation's
thresholds in the UI (`PUT /admin/soft-quotas/orgs/{name}`); the gateway
default and the profiles' are the proxy administrators'
(`PUT /admin/soft-quotas/gateway`, `/profiles/{name}`); `DELETE` goes back to
the configured value. A runtime override replaces the configured value of its
level, every change is audited (`soft_quota.levels`), and the other replicas
apply it at their next evaluation (15 s). A person in several organisations:
the key's organisation on the response path, the first of their organisations
by name in notifications.

**Once per window instance, across replicas.** A threshold fires once per
window *instance*, decided by one row per (person, window, threshold) in
PostgreSQL (`llmflow_warn_marks`), written with one conditional
`INSERT … ON CONFLICT DO UPDATE` whose row lock serialises the replicas, in the
same transaction as the notification's outbox rows (a replica killed in between
leaves both or neither). The week (and a calendar budget) has an instance: its
start. The **session has none: it is a rolling window of 5-minute buckets.** The
rule is therefore: the instance is the bucket in which the first replica fired
the threshold, and a fired threshold is **armed again only when usage falls
below the threshold minus a hysteresis** (5 points, or half the threshold when
smaller: 75 re-arms below 70). When several thresholds are crossed at once only
the highest is notified; the lower ones are marked, so they do not fire later.
`TestSoftQuotaOnceAcrossReplicasPG` runs three replicas on PostgreSQL with
random kills and checks exactly one firing per week and per episode; its planted
control (the store's decision skipped) fires more than once.

**Claude Code's status line.** Install `llmflow-status`
(`go install github.com/go-llmflow/gateway/cmd/llmflow-status@latest`, or
build it from this repository) and add to `~/.claude/settings.json`
([status line](https://code.claude.com/docs/en/statusline.md)):

```json
{
  "statusLine": {
    "type": "command",
    "command": "llmflow-status",
    "refreshInterval": 30
  }
}
```

`refreshInterval` re-runs the command every N seconds (minimum 1) in addition
to the event-driven updates, so the window's usage and reset time stay current
while the session is idle. The program reads the status line's JSON on stdin,
calls `/v1/me/usage` at `ANTHROPIC_BASE_URL` with `ANTHROPIC_AUTH_TOKEN` (else
`ANTHROPIC_API_KEY`: the same credential Claude Code sends) and prints, for
example,

```
session 85% · 15:45 │ week 40% · Mon 00:00        (semaine, lun. with --lang fr or LANG=fr_FR)
```

coloured green below the first threshold, yellow past some, red past the last
(`NO_COLOR` or `--no-color` for none). It caches the answer for `--ttl` (30 s)
in the user cache directory (the directory mode 0700, the file 0600, **no
secret in it**: the answer, and a file name derived from a hash of the gateway
and the credential). **On any error or timeout (`--timeout`, 2 s) it prints
nothing and exits 0**, and it never prints the credential (a planted test
proves the check can see a leak). It refuses to send the credential over plain
http except to this machine (`--allow-http` to override) and never follows a
redirect. If your `apiKeyHelper` supplies the credential and neither variable
is set in the status line's environment, pass `--token-file`. (The status line
documentation does not say which environment variables the command inherits;
Claude Code's own are assumed, and `--url` / `--token-file` are the fallback.)

**Continue** (and any client without a status line) shows no native warning:
read the `X-Llmflow-Usage-*` response headers from your own tooling, or
`GET /v1/me/usage`; the notifications reach people regardless of the client.

**Migration (v0.18 → v0.19).** Start-up adds `llmflow_warn_marks`,
`llmflow_warn_levels` and the `warn_at` column of `llmflow_budgets_v2`.
Nothing changes for a person without a profile or a user budget. The refusal
text of a profile changed (Anthropic's wording; the numbers in days now follow
it in parentheses). Upgrade every replica: a v0.18 replica does not evaluate
thresholds, so it only misses the firings it would have shared.

**Not measured.** `BenchmarkAdmission` was not run for this change: the machine
was loaded (load average above 30; the rule is 4). The request path adds one
profile lookup and a handful of in-memory reads per request (`usageHeaders`).

### Chargeback, cost centres, finance reader and adoption (#97)

- **`finance_viewer`** is a role without scope: it reads the reports of every
  organisation (`/admin/spend`, `/admin/export`, `/admin/fluids`,
  `/admin/lending` with its matrix, `/admin/chargeback`, `/admin/adoption`) and
  nothing else: no listing of teams, keys or users, no audit log, no write.
  Only a **proxy admin** grants it (`PUT /admin/users/{name}/roles`); an
  `org_admin` never can, so an organisation's admin cannot open another
  organisation's reports. It is also a valid `sso` role mapping. Every report
  route has a row in the authorization-matrix test with a `finance_viewer`
  column.
- **Cost centres.** General-ledger codes on the organisation, a lab or a
  project; the most specific wins (project, lab, organisation):

  ```hcl
  org "etabA" {
    share       = 0.5
    cost_center = "GL-7001"
    team "t1" { lab = "genomics" }
    lab "genomics" { cost_center = "GL-7002" }
    project "t1" "p9" { cost_center = "GL-7003" }
  }
  ```

  A code is 1 to 32 characters of letters, digits, `_ . / -` (no separator,
  quote, space or leading sign: it is read by a finance system).
- **`GET /admin/chargeback?from=&to=&format=csv|json`** (csv by default): one
  row per cost centre and calendar month of the period, with `gpu_hours`, `kwh`,
  `fluids_cost` and `fluids_currency` from the fluids block, and an external
  provider's spend in its own currency on a row of its own (`provider_cost`,
  `provider_currency`). **Money is exact**: each row's cost is rounded once, half
  to even, to the currency's minor unit (`fluids.currency_decimals`, default 2;
  provider spend: 2) and written as a decimal string without floating point; the
  JSON also carries the integer `*_cost_minor`. A file's total is the sum of its
  rows, in integers. An interval belongs to the month its start falls in, the
  same as in `/admin/fluids`, whatever time of day the period ends. What has no code is the cost centre `UNMAPPED`, and
  energy no organisation is charged for is `UNALLOCATED`: the file always adds
  up to the reports. This is a chargeback in FinOps' sense, the file a finance
  system books (finops.org, Invoicing and Chargeback). Budget-against-actual
  variance is in the decision report (below). Organisation admins and viewers
  get their own codes only.
- **`GET /admin/adoption?from=&to=&format=json|csv`**: active people per day and
  per week (Monday), requests, and per model the active people and requests.
  Counts only: no name, key or content leaves it. A service key's request counts
  as a request and as no one. **Small cells are suppressed**: a day, a week or a
  model used by 1 to k−1 distinct people is returned without counts
  (`suppressed`), models suppressed together are merged into `(other models)`
  with their requests, and the overall figure is hidden below k. k is
  `adoption_min_users` (top level, default 5, at least 2).

### The monthly decision report (#97)

`GET /admin/decision?from=&to=&format=pdf|xlsx|json` gathers the figures above
for a period (at most 366 days) into the document a partnership's steering
committee reads:

- **PDF**, typeset by `go-pdfkit/html2pdf` v0.3.0: A4, page numbers in the
  margin (`counter(page)`), a table of contents whose page numbers come from CSS
  `target-counter`, then **one page per partner** (guaranteed capacity, use in
  share, borrowed and lent GPU-hours, net balance, inference GPU-hours against the
  previous period, fluids on measured energy, latency in share against borrowing,
  active people, cost centre), then the lending matrix, fluids, adoption,
  chargeback rows and go-plot charts. The HTML is a golden file in the tests.
- **XLSX**: sheets Partners, Lending matrix, Chargeback and Adoption; typed
  cells, one column per unit.
- **json**: the same data. Proxy roles and `finance_viewer` read every partner and
  the matrix; `org_admin` and org-level viewers their own partner only. Every
  download is audited.

Scheduled delivery:

```hcl
decision_report {
  destination = "reports"          # an export block (S3-compatible or a directory)
  formats     = ["pdf", "xlsx"]    # default both
  day         = 2                  # of the month, UTC, 1..28 (default 2)
  mail_to     = ["finance@partner-a.example", "finance@partner-b.example"]  # optional, max 50
}
```

From that day on, the previous calendar month is delivered twice over:

- to the export, as `decision/YYYY-MM/decision-YYYY-MM.pdf` and `.xlsx`
  (write-once under Object Lock where the export has it);
- by mail to `mail_to`, through the `alerts { smtp { … } }` relay (which
  `mail_to` requires): a MIME message with the PDF attached, the XLSX too when
  both fit in 8 MiB, else a line saying where the export holds them. The relay
  session follows `smtp.tls` (below).

The sink is the state: an object that exists is delivered, and the marker
`decision/YYYY-MM/mailed.json` says the mail went out, so a restart or a second
replica does nothing twice, and an upload still retrying is not typeset again.
Every delivery is audited (actor `decision-report`) **when the object is seen in
the export**, not when it is queued, and so is every mail (period, format,
size, never the content).

**Bounds.** The report is refused (413) beyond 100 partners, 2 000 matrix cells,
3 000 chargeback rows, 400 adoption rows or 16 MiB, typesetting is limited to
2 minutes, the build runs under a timeout, and at most 2 heavy reports run at
once (429 with `Retry-After` otherwise). The quota tables are refused beyond
3 000 rows (soft, refusal and profile rows together) or 1 500 oversubscription
days, the variance table beyond 3 000 rows; their store queries are bounded
(100 000 events and assignments, 20 000 refusal cells, ErrTooMany past them).

#### Quotas and profiles in the report

Sections "Quotas and profiles" (PDF), sheets `Soft quotas`, `Refusals`,
`Profiles`, `Oversubscription` (XLSX) and the `quotas` key (json), per partner,
lab (team group) and profile, for the period:

- **Soft-quota notifications** by threshold and window (5 h session, weekly):
  events and distinct people. Every notification is now logged durably in
  `llmflow_warn_events` (`WarnMark.At`; a backfill copies the latest mark of each
  user, window and threshold that existed before the upgrade, so the log starts
  from there, and says so in the report notes). The outbox is not the source: it
  only holds mail and webhooks when they are configured, and is pruned.
- **Hard refusals** (429): budget, profile session, profile week, profile
  overflow, counted per day, organisation, team and profile in
  `llmflow_refusals`. The counts are kept in memory and written every 30 s and
  when the gateway stops (a crash loses at most 30 s of counts; at most 4 096
  distinct cells are held, the rest are counted under their kind). Never a
  person. `llmflow_denied_total` stays the live Prometheus view.
- **Workload days assigned and used** per profile: assigned days are the
  assignment history overlapping the period (days of the period times days per
  week over 7), used days are the cost divided by the profile's workday.
- **Oversubscription over time**: for up to 70 sampled days, the sum of the
  assigned days per week against the capacity of that day (trailing 7 days of
  pool load, the same `capacityOf` as `/admin/profiles`), not the point-in-time
  ratio. The PDF shows peak, average and days above 1.0 per partner; the XLSX
  and json carry every sampled day.
- **Small cells**, as for adoption: a figure over fewer than
  `adoption_min_users` **people** is `suppressed`. For refusals too: the store
  keeps, per day and cell, the set of people refused as truncated SHA-256
  hashes (`llmflow_refusal_users`, at most 1 000 per cell and day; a service
  key is a refusal and no one), so one person refused five times is a cell of
  one. No row carries a person. `org_admin` sees their own partner only.
- **Secondary suppression.** The partner row and its lab rows are published
  together, so a partner figure minus the shown labs could give a suppressed
  lab back. Per partner and (window, threshold) / (profile, kind) / profile:
  when exactly one lab is suppressed, the smallest shown lab is suppressed
  too; when the partner's people outside the shown labs are 1 to k-1, the
  partner row is suppressed. Fuzzed (`FuzzQuotaSubtraction`): no published
  parent minus the published children is a value from fewer than k people.
  Source opened: the *ESSnet Handbook on Statistical Disclosure Control*
  (Hundepool et al., [PDF](https://research.cbs.nl/casc/SDC_Handbook.pdf)), §5.3
  "Methods", p. 175: protecting the unsafe cells "is called primary
  suppression, and to ensure these cannot be derived by subtractions from
  published marginal totals, additional cells are selected for secondary
  suppression"; the protection holds only if "the same cells in any linked
  tables are also suppressed" (the adoption limit below is that case). The
  handbook's optimal and network algorithms (§4.4) minimise the information
  lost; the rule above is a simpler conservative one that suppresses at least
  as much in this partner/lab hierarchy.
- **Adoption's "(other models)" row** carries its people and requests only if
  the DISTINCT people over the merged models (a union measured by the store,
  `UsersOfModels`) are k or more; otherwise no count. Two single-person
  models of the same person no longer show that person's requests. Open:
  daily and weekly totals minus the shown models still bound the merged
  requests.
- Refusal counts are written outside the request path's lock (the pending
  map is swapped out, written, and merged back on failure) and are keyed by
  their day, so a failed write never moves yesterday's refusals to today.

#### Budget against actual (FinOps variance)

A table per month, cost centre and unit (`Variance` sheet, `variance` key,
"Budget against actual" section). Sources opened: FinOps Foundation,
Budgeting (budget to actual, thresholds 20 % Crawl / 15 % Walk / 12 % Run) and
Forecasting (percentage variance is the primary metric, the amount is also
informative); FOCUS defines no budget or variance column, so it is not used.

- **Budgeted**: the declared org and team budgets (no per-model budget), prorated
  to the month by days (a team budget wins over the organisation's).
- **Estimated**: the weekly profile allowances (`profile_week` budgets) of the
  partner's people, prorated the same way.
- **Actual**: the chargeback figure of the cost centre and month, in GPU-hours
  (milli-GPU-hour minor unit) and in each provider currency (cents); units are
  never added.
- **Variance** is actual minus budgeted (positive: over budget), as a minor-unit
  integer and as a percentage of the budgeted amount (one decimal, half to
  even). Each amount is rounded once, half to even. A row beyond
  `variance_threshold_pct` (top level, default 15, above 0 and at most 1000) is
  flagged.

#### Energy reconciliation: Slurm against DCGM

With `slurm { sacct_file = "/abs/path" }` the report compares, per partner, the
energy Slurm measured for the jobs of the partner's accounts with the IT energy
the gateway attributed from the DCGM counters. The file is
`TZ=UTC sacct -a -X -P -o JobID,Account,Start,End,ConsumedEnergyRaw -S <from> -E <to>`
(`sacct.html`: `-S`/`-E` select jobs *active* in the window, not jobs that ended
in it; `End` is `Unknown` for a running job; timestamps are
`YYYY-MM-DDThh:mm:ss`). The header **must** name `JobID`, `Account`, `Start`,
`End` and `ConsumedEnergyRaw`: without the instants a job cannot be placed in a
period, and without `JobID` steps cannot be told from allocations. The scheduled
monthly report reads the same file every month, so **each job's energy is
attributed to the report period in proportion to the share of its run inside
it** (constant power assumed; a job entirely outside counts 0; a running job has
accrued its energy up to the file's latest instant). Job steps (a `JobID` with a
`.`) are skipped even without `-X`, a row without energy counts as nothing, a
job that never started is left out, and a value with a unit prefix is refused
(the base is not documented). The report states its coverage (jobs used,
prorated, outside, running; the file's earliest and latest instant) and **when
the file does not cover the period** (its span reaches less than half of it, or
no job falls in it) it says so and prints no difference. Accounts map to
partners by `slurm.accounts` (else by name). The difference is **DCGM minus
Slurm**, in kWh and as a percentage of Slurm's figure (`n/a` when Slurm reports
nothing). It is shown as
a table in the PDF, a sheet "Energy reconciliation" in the XLSX, and
`energy_reconciliation` in the JSON; an unreadable input is said in the report,
not hidden. A difference is a question, not an error: Slurm counts a job's
allocation ("only in the case of an exclusive job allocation does this value
reflect the job's real energy consumption", sacct.html), DCGM counts the GPUs.
Organisation admins see their own partner only.

#### Mail transport: TLS is required (changed in this release)

Alert and report mail went through `net/smtp.SendMail`, which upgrades to TLS
only when the server offers it: a relay that did not advertise STARTTLS got the
message, and an unauthenticated session got it in clear text. Now:

```hcl
alerts {
  smtp {
    server  = "mail.example.org:587"
    from    = "llmflow@example.org"
    to      = ["ops@example.org"]
    tls     = "starttls"   # default; "implicit" (port 465); "none"
    timeout = "30s"        # dial and whole session
  }
}
```

- `starttls` (the default): the session is refused, and nothing is sent, unless
  the server offers STARTTLS and the handshake succeeds (certificate verified
  against the host name, TLS 1.2 or later).
- `implicit`: TLS from the first byte.
- `none`: clear text, the old behaviour. Accepted without more for a relay on
  this host (`127.0.0.1`, `localhost`); for any other host it needs
  `allow_cleartext = true`. A password is never sent over a clear connection
  to another host.

**Migration:** an existing `smtp` block that points at a relay without TLS now
fails every send ("does not offer STARTTLS"). Enable STARTTLS on the relay, or
set `tls = "none"` (plus `allow_cleartext = true` off the local host) to keep
the old behaviour knowingly.

#### Binary size

`go-pdfkit/html2pdf` brings go-webengine (layout, a JavaScript engine, esbuild)
into the module graph: the stripped gateway binary grows from 28.8 MB to 43.8 MB
(+52 %), 60.2 MB unstripped. See issue #97 for the options (a build tag, or a
separate `llmflow-report` command that renders the PDF from `format=json`).

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
- Without an `ha` block, sessions live in the memory of the replica that
  created them (behind several replicas, use sticky sessions); with one,
  they are sealed in PostgreSQL and shared (see
  [High availability](#high-availability-several-replicas-since-v0140)).

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
| Reports (since v0.12.1) | see below |
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
unchanged; every label is escaped. Every chart is drawn by
[go-plot/plot](https://github.com/go-plot/plot) v0.3.0 (since v0.15.1; v0.13.0 to v0.15.0 used go-plot v0.2.0) with
`ParseMath(false)` on the figure and `PlainText()` on every call that
draws a name (tick labels, axis labels, titles). An org called
`$\input{/etc/passwd}$`, `$5 and $6` or `<script>` therefore stays a
string and never reaches go-plot's TeX typesetter. A test spies on
go-plot's text renderer to prove it, with a control that does reach TeX.
The pie receives no name at all: its legend is the gateway's own escaped
SVG text. Pies, bars and the series together on one page were checked in
Chrome under the policy above: no violation, while a control page with a
planted inline style is refused. The page's queries are fuzzed
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
(go-pdfkit v0.20.0, one embedded font, Atkinson Hyperlegible) draws every
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
`/v1/audio/transcriptions`, `/tokenize`, `/detokenize`, `GET /v1/models`, `GET /v1/me/usage` (the caller's own soft-quota windows), and `POST /mcp` with `mcp_server` blocks. `GET /healthz` returns 503 when the budget
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

### vLLM instances as Slurm jobs: elastic pools (#75)

A model can take its upstreams from **Slurm**, instead of (or on top of) static `upstream` blocks. This is the `slurm` block *inside a `model` block*. The top-level `slurm` block is a different thing: the accounting input of the reconciliation reports. The vLLM instances run as long jobs of a service account, for example account `svc_llm` with QOS `service`. They can be stopped at night so that the GPUs can be lent to a borrowing QOS. The gateway **follows** the running jobs; it never starts or stops one. Slurm (scrontab) or the operator does that.

```hcl
pool "gpu" { capacity = 0 }   # 0: C is the sum of the instances' capacities

model "llama" {
  upstream_model = "meta/llama-3.3-70b"
  pool           = "gpu"
  key_file       = "/etc/llmflow/vllm.key"     # the instances' --api-key
  slurm {
    url         = "https://slurmrestd.cluster:6820"   # from here only
    api_version = "v0.0.44"                      # slurmrestd -d list
    token_file  = "/etc/llmflow/slurm.jwt"       # any secret reference
    user_name   = "svc_llm"                      # X-SLURM-USER-NAME (optional)
    accounts    = ["svc_llm"]                    # required
    partitions  = ["gpu"]                        # required
    users       = ["svc_llm"]                    # optional allow-list
    interval    = "15s"
    stale_after = "5m"
    instance_capacity     = 8                    # when the job does not say
    max_instance_capacity = 64
  }
}
```

**Convention.** A job serves model `M` on port `P` when its name (or else its comment) is `llmflow:M:P` or `llmflow:M:P:C`. `C` is the instance's contribution to the pool's capacity; when it is absent, `instance_capacity` applies.

**Discovery.**
- Every `interval`, the gateway reads `GET <url>/slurm/<api_version>/jobs/` with `X-SLURM-USER-TOKEN`.
- It keeps the jobs that are `RUNNING`, with none of the `COMPLETING`, `CONFIGURING`, `STOPPED`, `SIGNALING`, `RESIZING` or `REQUEUED` flags.
- The job must be in an allowed account and partition, and, when `users` is set, belong to an allowed user.
- The instance is `scheme://<batch_host>:P`. It is added only after `GET /health` answers 200 and `GET /v1/models` (with the model's key) lists `upstream_model`.

**Removal.**
- When the job leaves the list, or fails health 3 polls in a row, the instance is **drained**: it takes no new request, not even as a last resort.
- It is removed once its requests in flight have finished, or after `limits.max_request_duration`.
- It is admitted again, after the same checks, if its job comes back.

**Capacity.**
- The pool's C is its configured `capacity` plus the capacities of its non-draining instances.
- Every membership change re-apportions the organisations' integer reservations with the same Hamilton method (`fairshare.Pool.SetCapacity`).
- Every change is audited: `slurm.member_added`, `slurm.member_drained` (with the reason), `slurm.member_removed`, and `pool.capacity` (before, after, reservations).
- Requests in flight keep their within-share or borrowing status.

**With an `ha` block (since v0.15.2).**
- Every replica runs its own discovery and its own health checks, for its own routing. Publishing the healthy set through PostgreSQL was the alternative; it was not chosen because it ties every replica's routing to the owner's liveness and to PostgreSQL, and because what is healthy depends on the replica's network path to the instance.
- Only the pool's owner's view sets C. The owner decides with it, and publishes C and the integer reservations with the lease's epoch (`llmflow_pool_capacity` shows the decided C on every replica). Forwarded decisions therefore use the owner's membership, and a replica that sees fewer instances still routes only to those it sees.
- A replica takes a pool only after its discovery has read the members once. Until then, as after a takeover before that, requests are admitted at the lowest priority (or refused with `on_owner_unreachable = "reject"`), as when no owner answers.
- An owner whose slurmrestd stays unreadable past `stale_after` yields: a replica that still reads the jobs takes the lease (epoch + 1) and stays quiet for one `lease_ttl`, until the previous owner's validity has surely ended. If no replica reads slurmrestd, the owner keeps the pool with its last known members.
- Chaos (real PostgreSQL): the owner killed while a job joins (the survivor owns with the new C, epoch + 1); slurmrestd lost by the owner only (handover after 1.2 s, never two owners in ~950 samples, the test fails without the quiet period); two replicas that disagree (each routes to what it sees, the fair share is the owner's). The sum of the reservations never exceeded C in any sample.

**Safety.**
- Only the configured accounts and partitions (and users) are trusted. Without this, anyone could submit a job named `llmflow:llama:8000` and capture prompts.
- An untrusted job claiming a model is logged once and counted in `llmflow_slurm_rejected_jobs_total{reason}`.
- `batch_host` must be a plain host name, and the URL built from it is validated. A fuzz test checks that nothing in a job's name or host can steer the URL elsewhere.
- slurmrestd's URL comes from the configuration only.
- The JWT is a secret reference, never an environment variable or an argument. It is never sent across a redirect: redirects are refused.
- The answer is bounded to 32 MiB and 100 000 jobs.
- **Fail-closed**: while slurmrestd cannot be read, the last known members are kept. Each member is still health-checked and drained on failure. Past `stale_after`, the gateway logs an error, and no instance is admitted until a poll succeeds. Healthy members are never dropped because of an outage.
- Metrics: `llmflow_slurm_polls_total{outcome}`, `llmflow_slurm_last_success_timestamp_seconds`, `llmflow_pool_capacity`.

**slurmrestd.** Use `AuthAltTypes=auth/jwt` with `AuthAltParameters=jwt_key=…` ([JWT authentication](https://slurm.schedmd.com/jwt.html)). Issue the gateway's token with `scontrol token username=svc_llm lifespan=…` and store it in the secret source. The token is read when the gateway starts, so pick a lifespan longer than your restart cycle: the default is only 1800 s. Once it has expired, polls fail, and the fail-closed rule above applies.

**sbatch template** (one instance per job):

```bash
#!/bin/bash
#SBATCH --job-name=llmflow:llama:8000:8
#SBATCH --account=svc_llm --qos=service --partition=gpu
#SBATCH --nodes=1 --gres=gpu:4 --exclusive --time=7-00:00:00
#SBATCH --signal=B:TERM@120
# The key goes in VLLM_API_KEY, not --api-key: the node's process list
# shows every command line to every user.
VLLM_API_KEY=$(cat /etc/llmflow/vllm.key); export VLLM_API_KEY
exec vllm serve meta/llama-3.3-70b --served-model-name meta/llama-3.3-70b \
  --host 0.0.0.0 --port 8000 --tensor-parallel-size 4
```

The node's vLLM port must be reachable by the gateway only (firewall), as for static upstreams: `/metrics` is unauthenticated, and the key protects `/v1` only.

**Day/night with scrontab** (needs `ScronParameters=enable`; [scrontab](https://slurm.schedmd.com/scrontab.html)):

```
#SCRON --account=svc_llm --qos=service --partition=service -t 0:05:00
0 7 * * 1-5 sbatch /opt/llmflow/vllm-llama.sbatch
#SCRON --account=svc_llm --qos=service --partition=service -t 0:05:00
0 20 * * 1-5 scancel --account=svc_llm --name=llmflow:llama:8000:8
```

At 20:00 the jobs end. The gateway drains them, its pool shrinks, and the reservations follow. At 07:00 the new jobs are admitted as soon as vLLM answers.

**Tested against a real Slurm** (go-llmflow/bench `TestSlurmDiscoveryE2E`, 2026-10-26). The setup was Slurm 24.11.5 with slurmrestd data_parser v0.0.42, auth/jwt, arm64. Results:
- A vLLM CPU job was admitted 18 s after `sbatch`, served a chat request, and was removed 2 s after `scancel`.
- Another account's job claiming the model was rejected and never contacted.
- A vLLM job whose engine failed to start stayed `RUNNING`, and it was never admitted.

References opened on 2026-10-26:
- Slurm `master` source: `src/slurmrestd/plugins/openapi/slurmctld/api.c` (`GET /slurm/{data_parser}/jobs/`); `src/plugins/data_parser/v0.0.43`–`v0.0.46` `parsers.c` (`PARSER_ARRAY(JOB_INFO)`: `job_id`, `name`, `comment`, `admin_comment`, `account`, `partition`, `user_name`, `batch_host`, and `job_state` as a flag array); `src/slurmrestd/plugins/auth/jwt/jwt.c` (`X-SLURM-USER-TOKEN`, `X-SLURM-USER-NAME`).
- slurm.schedmd.com: `rest_api.html`, `rest_quickstart.html`, `jwt.html`, `scrontab.html`.
- vLLM v0.32.0: `entrypoints/serve/instrumentator/health.py` (`/health`: 200, or 503 when the engine is dead) and `entrypoints/openai/models/api_router.py` (`/v1/models`).

The job environment is not readable through `job_info`: it exists only in the submit description. So the convention uses the name or the comment. `squeue` and `scontrol` are not used, because slurmrestd is the only interface that needs no Slurm client on the gateway host.

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
`llmflow_fairshare_level`, `llmflow_inflight_phantoms` (per pool and organisation, label `org`);
hard budgets: `llmflow_budget_hold_exceeded_total` (alert on any),
`llmflow_budget_grant_refills_total`, `llmflow_budget_hold_refused_total`,
`llmflow_budget_grant_return_failures_total`;
with `ha`, `llmflow_ha_owner`, `llmflow_ha_epoch`, `llmflow_ha_local_grants`,
`llmflow_ha_unowned_total`; `llmflow_upstream_healthy`,
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

1. **Rate limits and the key cache are per process.** Since v0.14.0 the
   fair-share state is shared between replicas with an `ha` block (see
   [High availability](#high-availability-several-replicas-since-v0140));
   without one, run one replica per pool, since several would each grant the
   full reservation. Per-key `rpm`/`tpm`/`max_inflight` limits are still
   counted per replica (N replicas allow up to N times the limit). Without
   an `ha` block, a revocation reaches another replica's key cache after
   `key_cache_ttl` (with one, at once by NOTIFY).
   During a failover (up to `lease_ttl`) requests are admitted at the lowest
   priority without an owner's decision; a new owner's safety margin is the
   in-flight count persisted up to `usage_persist` earlier; the fencing token
   is checked by PostgreSQL and by the replicas, not by vLLM, so a decision
   an owner made just before a long pause can still reach vLLM. The internal
   channel is authenticated but, without the `tls_*` files, not encrypted
   (keep it on a private network, or set them: mutual TLS since v0.16.0).
2. *(Removed in v0.17.0: budgets no longer lag. See
   [Hard budgets](#hard-budgets-since-v0170) for what remains at their edges.)*
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
   and reasoning tokens. Under a budget it is capped at the request's worst
   case W (with `echo`, the forwarded deltas hold the prompt).
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
    `llmflow_alert_retries_total`). Without an `ha` block a gateway stopped
    mid-retry does not resume (the period is already marked as alerted);
    with one, retries live in a PostgreSQL outbox and resume on any
    replica, delivered at least once.

11. **SSO sessions live in one replica's memory without an `ha` block**:
    behind several replicas, use sticky sessions or the `ha` block (sessions
    and logins in progress sealed in PostgreSQL).
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
18. ~~**The PDF export links more than it uses.**~~ **Fixed upstream in go-pdfkit/pdfkit v0.20.0.** Its widget bridge moved to the subpackage `pdfkit/widget`, so importing pdfkit no longer links go-widgets/toolkit, go-crdt/collab, richdoc, or the JPEG 2000 and JBIG2 decoders.
    - The gateway binary went from 42,110,114 to 39,790,834 bytes, 2,319,280 bytes (5.5%) smaller (darwin/arm64, `CGO_ENABLED=0 -trimpath`, same tree but for the bump).
    - `go list -deps ./cmd/llmflow-gateway` names none of those modules.
    - A test in pdfkit fails if its root package links the toolkit again.

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
| Two replicas, one database | A concurrent first start works (A7); keys are shared; a revocation reaches the other replica after `key_cache_ttl` without an `ha` block (open defect 1), in 3 ms with one (v0.14.0); the audit chain stays one verifiable chain under two concurrent writers. |
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

### Security fixes in v0.18.1

Two defects of v0.18.0, found by a post-merge review. Upgrade if you use
workload profiles.

- **#98, a hard-budget hole.** A rolling window kept only twice its current
  length of spend. Widening `session_window` (1h to 5h) therefore forgot
  spend that was inside the new window, and a session budget could be
  exceeded. A rolling budget now keeps its buckets for the longest window that
  can apply (`MaxRollingWindow`, 7 days: at most 2016 buckets of 5 minutes per
  budget), on both the memory and the PostgreSQL store. Spend lost by a
  v0.18.0 prune before the upgrade is not recoverable from the buckets; it is
  still in the usage records.
- **#99, an authorization bypass.** An organisation admin could grant their
  organisation's `viewer` role to any existing user, which made that person
  theirs to assign a profile, read and delete. Who a person belongs to is now
  decided by their **team** bindings (which SSO and SCIM also create). An
  organisation-level binding (`viewer`, `org_admin`) is authority, not
  membership: it gives no access to the person, whoever granted it. A person
  with only such bindings, or none, or a proxy role, is the proxy admins'. The
  same rule applies to the user budgets API. Consequence: an organisation
  admin no longer assigns the profile of another organisation admin who is
  in no team; ask a proxy admin.

### Security fix in v0.18.2: who may place a person in an organisation (#104)

v0.18.1 still let an organisation admin pull a user who had no binding (or
belonged to another organisation) into their organisation by granting them a
team role, and then act on them. Every binding now records its **provenance**:
`scim`, `sso_claim`, `proxy_admin` or `org_admin` (shown as `provenance` in
`GET /admin/users/{name}/roles`).

- An organisation or team admin may grant a role only to a user already
  provisioned into that organisation: one holding a team binding of that
  organisation created by SCIM, an SSO claim or a proxy admin. A user with no
  binding, or in another organisation, is a proxy admin's to place (a user
  created by an org admin needs a proxy admin's first grant).
- A team binding an org admin granted does not make the person theirs: it is
  ignored by the profile and user-budget access rules. SCIM re-creates a
  binding it also wants under its own provenance.
- **Migration.** The column is added automatically
  (`provenance text NOT NULL DEFAULT ''`). Bindings stored before v0.18.2 have
  an **unknown** provenance, which is treated as legitimate: nothing says they
  were not, so no existing access changes at the upgrade. Only grants made
  after it are classified.

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
