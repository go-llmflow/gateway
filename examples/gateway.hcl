# llmflow-gateway configuration for a 4-node HGX B300 cluster (32 GPUs)
# shared by two institutions (HCL2 native syntax).
#
# Secrets are never in this file: every *_file attribute names a file holding
# one secret on one line (mode 0600). The file is evaluated with no variables
# and no functions. Quoted strings are HCL templates: write a literal "${" as
# "$${" and a literal "%{" as "%%{".

listen             = "0.0.0.0:8080"
master_key_file    = "/etc/llmflow/master.key"   # admin API bearer token
metrics_token_file = "/etc/llmflow/metrics.token" # optional bearer token on /metrics

retries                = 2
budget_refresh         = "10s"
key_cache_ttl          = "30s"
shutdown_grace         = "30s"
expose_overhead_header = false

# Failed authentications allowed per client network (IPv4 address, IPv6 /64)
# per minute before 429; 0 disables.
auth_failures_per_minute = 60

# Prefix-cache isolation (vLLM security guide: prefix-cache reuse is a timing
# side channel between tenants). The gateway sets cache_salt on every request,
# derived from this key and the tenant: "org" (default, one cache per
# institution), "team", "key", or "none" (single tenant). Generate the key
# once, e.g. 32 random bytes in base64, and keep it secret.
cache_isolation     = "org"
cache_salt_key_file = "/etc/llmflow/cache-salt.key"

# Reverse proxies whose X-Forwarded-For is believed (read from the right,
# skipping these). Without it the connection's address is the client.
trusted_proxies = ["10.0.0.0/24"]

# Networks allowed per route class. /metrics and /healthz default to
# loopback; /metrics also opens with the metrics token from anywhere.
routes {
  metrics { allow = ["127.0.0.0/8", "10.0.1.0/24"] } # Prometheus
  healthz { allow = ["127.0.0.0/8", "10.0.0.0/24"] } # load balancer
  admin { allow = ["10.0.2.0/24", "2001:db8:2::/48"] } # operators
}

store {
  kind        = "postgres"            # or "memory" (state lost on restart)
  dsn_file    = "/etc/llmflow/pg.dsn" # e.g. postgres://llmflow@db:5432/llmflow?sslmode=verify-full
  fail_closed = true                  # 503 when the budget state cannot be refreshed
  queue_size  = 10000                 # bounded accounting queue
  batch_size  = 256
  # Durable accounting spool (required with postgres): every batch of usage
  # records is fsync'd here before it is written, replayed after an outage
  # or a crash; files are 0600, the directory 0700 and owned by the gateway.
  spool_dir       = "/var/lib/llmflow/spool"
  spool_max_bytes = 268435456 # 256 MiB; beyond it records are dropped, counted and alerted
}

limits {
  max_body_bytes          = 8388608
  max_header_bytes        = 65536
  read_header_timeout     = "10s"
  read_timeout            = "60s"
  idle_timeout            = "120s"
  upstream_header_timeout = "10m"
  max_request_duration    = "30m"
  max_response_bytes      = 67108864
}

fairshare {
  half_life = "168h" # usage decay, Slurm's default PriorityDecayHalfLife (7 days)
}

health {
  interval = "5s"
  timeout  = "2s"
  cooldown = "30s"
  path     = "/health"
}

# Organisations (institutions) hold the shares of every pool, like the top
# accounts of a Slurm account tree. Teams (labs) sit under them; more teams
# and their projects can be created through the admin API.
org "etabA" {
  share = 0.8125 # 26 of 32 GPUs
  team "lab-nlp" {}
  team "lab-vision" {}
}

org "etabB" {
  share = 0.1875 #  6 of 32 GPUs
  team "lab-chem" {}
}

# Request tags (in addition to the tags set on keys), for tag budgets and
# reports. Only the listed tags are accepted.
tags {
  header  = "X-Llmflow-Tags"
  allowed = ["eval", "prod", "teaching"]
}

# Soft-budget alerts: once per budget and period.
alerts {
  webhook_url        = "https://alerts.example.org/llmflow"
  webhook_token_file = "/etc/llmflow/webhook.token"
  smtp {
    server        = "smtp.example.org:587"
    from          = "llmflow@example.org"
    to            = ["hpc-ops@example.org"]
    username      = "llmflow"
    password_file = "/etc/llmflow/smtp.password"
  }
}

# C: sustainable concurrency of the pool, measured externally (the number of
# concurrent requests vLLM keeps running without preempting).
pool "b300" { capacity = 256 }

model "llama-3.3-70b" {
  upstream_model = "meta-llama/Llama-3.3-70B-Instruct"
  pool           = "b300"
  class          = "interactive"
  key_file       = "/etc/llmflow/vllm.key" # default for the upstreams below

  cost { # GPU-hours per million tokens (internal price)
    input_per_mtok  = 0.02
    output_per_mtok = 0.20
    cached_per_mtok = 0.002
  }

  upstream "gpu01" { base_url = "http://10.0.0.11:8000" }
  upstream "gpu02" { base_url = "http://10.0.0.12:8000" }
  upstream "gpu03" { base_url = "http://10.0.0.13:8000" }
  upstream "gpu04" {
    base_url = "http://10.0.0.14:8000"
    key_file = "/etc/llmflow/gpu04.key" # per-upstream override
  }
}

model "llama-3.3-70b-batch" {
  upstream_model = "meta-llama/Llama-3.3-70B-Instruct"
  pool           = "b300"
  class          = "batch"
  key_file       = "/etc/llmflow/vllm.key"

  cost {
    input_per_mtok  = 0.02
    output_per_mtok = 0.20
    cached_per_mtok = 0.002
  }

  upstream "gpu01" { base_url = "http://10.0.0.11:8000" }
  upstream "gpu02" { base_url = "http://10.0.0.12:8000" }
  upstream "gpu03" { base_url = "http://10.0.0.13:8000" }
  upstream "gpu04" {
    base_url = "http://10.0.0.14:8000"
    key_file = "/etc/llmflow/gpu04.key"
  }
}

# Workload profiles (README, "Workload profiles"): a person's allowance in
# working days per week. The workday (what one person-day consumes, per
# unit) is calibrated from measured usage in /ui/profiles/calibrate; until
# then, requests under a profile are refused in that unit. One attribute per
# line: HCL refuses several in a one-line block.
profile "q" {
  days          = 0.25
  session_share = 0.25
}
profile "1d" {
  days          = 1
  session_share = 0.5
  overflow_cap  = 2 # workdays a month beyond the profile, when allowed for the person
}
profile "5d" {
  days          = 5
  session_share = 0.5
}
session_window = "5h"
# default_profile = "q"  # everyone without a profile of their own
