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
- virtual keys (hashed), budgets in GPU-hours, per-key RPM / TPM / in-flight limits;
- per-request accounting in PostgreSQL and a CSV export to reconcile with Slurm `sreport`;
- Prometheus metrics; streaming pass-through without buffering.

```sh
go install github.com/go-llmflow/gateway/cmd/llmflow-gateway@latest
llmflow-gateway check-config -c examples/gateway.yaml   # validates the file and every secret file it names
llmflow-gateway serve -c examples/gateway.yaml
llmflow-gateway export -c examples/gateway.yaml --from 2026-10-01 --to 2026-11-01 > october.csv
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
measured externally. Each team owns an integer reservation, obtained by
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

See [`examples/gateway.yaml`](examples/gateway.yaml) (parsed by the test suite,
so it stays valid). Secrets are only read from files named in the
configuration: `master_key_file`, `metrics_token_file`, each group's
`api_key_file` (the vLLM `--api-key`) and `store.dsn_file`. Nothing is read
from the environment or the command line. Unknown YAML keys are rejected.

| key | meaning |
|---|---|
| `teams[].share` | fraction of every pool reserved for the team (sum ≤ 1) |
| `pools[].capacity` | C, sustainable concurrency of the pool |
| `model_groups[]` | client model name → `upstream_model`, `pool`, `class` (`interactive` / `batch`), `upstreams`, `api_key_file`, `cost` in GPU-hours per million input / output / cached tokens |
| `store` | `memory` or `postgres`; `fail_closed` (default true); accounting queue and batch size |
| `fairshare.half_life` | usage decay half-life (default 168h) |
| `limits` | body size, header size, timeouts, max request duration, max upstream response size |

Cost of a request: `((prompt − cached) × input + cached × cached_price +
completion × output) / 10⁶` GPU-hours, from the `usage` vLLM reports. For
streams the gateway sets `stream_options.include_usage` and removes the usage
chunk again if the client did not ask for it.

## Admin API

All admin routes require `Authorization: Bearer <master key>`.

| route | |
|---|---|
| `GET` / `POST /admin/teams`, `DELETE /admin/teams/{name}` | list, create `{"name"}`, disable (revokes all its keys) |
| `GET /admin/keys?team=`, `POST /admin/keys`, `DELETE /admin/keys/{id}` | list (no secrets), create `{"team","name","rpm","tpm","max_inflight"}` (the `llmf-…` secret is returned once), revoke |
| `GET /admin/budgets`, `PUT` / `DELETE /admin/budgets/{team}` | `{"period": "day" \| "week" \| "month", "max_cost": <GPU-hours>}`, periods aligned on UTC |
| `GET /admin/usage.csv?from=&to=` | per team and pool: requests, tokens, GPU-hours (same as `llmflow-gateway export`) |
| `GET /admin/fairshare` | reservations, in-flight, borrowing, usage, LF and rank per pool and team |

Client routes: `POST /v1/chat/completions`, `/v1/completions`,
`/v1/embeddings`, `GET /v1/models`. `GET /healthz` returns 503 when the budget
state is stale and fail-closed is on.

## Metrics

`GET /metrics` (Prometheus text format 0.0.4, optional bearer token):
`llmflow_inflight`, `llmflow_inflight_borrowing`, `llmflow_borrowed_fraction`,
`llmflow_reserved`, `llmflow_inflight_tokens`, `llmflow_fairshare_usage`,
`llmflow_fairshare_level` (per pool and team); `llmflow_upstream_healthy`,
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

BENCHMARK_RESULTS

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
  with unknown fields rejected; bounded key cache and accounting queue.
- Budgets fail closed: when PostgreSQL cannot be read for 3 refresh periods,
  every request gets 503 (`store.fail_closed: false` to fail open).
- CI: race tests on amd64 and arm64, PostgreSQL integration tests against a
  service container, fuzz smoke, `govulncheck`, `CGO_ENABLED=0` cross-builds
  for linux/amd64 and linux/arm64.

Open defects (v0.1.0):

1. **State is per process.** In-flight counts, rate limits and the key cache
   live in memory: run one gateway replica per pool. Several replicas would each
   grant the full reservation.
2. **Budgets lag.** Cost is known when a request ends, so a burst of long
   requests can overspend by what is in flight; records still queued when the
   budget state is refreshed are uncounted until the next refresh.
3. **Priorities are fixed at admission.** A borrowing request already queued in
   vLLM keeps its priority when its team drops back under its reservation (vLLM
   has no API to re-prioritise a queued request).
4. **Interrupted streams are not charged.** The usage chunk comes last; a stream
   cut before it records zero tokens.
5. **Shared prefix cache.** Institutions share vLLM's prefix cache, a timing
   side channel between them; the gateway does not set vLLM's `cache_salt` yet.
6. **Accounting can drop records** when the queue is full or a batch write fails
   twice; both are counted in metrics, and budgets still see the cost in memory.
7. No TLS termination in the gateway (put it behind a TLS proxy); the admin API
   shares the client listener; `/v1/responses` is not proxied.
8. `C` is static configuration; the gateway does not measure or adapt it.

## Development

```sh
go test -race ./...
LLMFLOW_TEST_PG_DSN=postgres://… go test ./store   # PostgreSQL contract test
go test -run '^$' -fuzz FuzzRewrite -fuzztime 1m ./openai
```

License: BSD-3-Clause.
