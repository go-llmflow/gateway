# A multi-institution B300 pool that lends its spare capacity to a third party
# (ILaaS: infrastructure/inference-as-a-service), and bills the third party's
# electricity and cooling.
#
#   - three owners (etabA, etabB, etabC) bought the B300 servers in the
#     proportions 50 / 30 / 20 %. Each is guaranteed its slots and may borrow
#     the slots the others leave free (as with borrow = "all");
#   - "tiers" holds NO share (share = 0). With borrow = "spare" it is admitted
#     only while fewer than C requests are in flight and vLLM's queues are not
#     saturated; otherwise it gets 429 rate_limit_exceeded with Retry-After: 5.
#     Admitted, it runs in the LAST priority band: behind every owner, and the
#     first to be preempted by vLLM when an owner needs the GPUs. The owners'
#     use is therefore never delayed by the third party, except by the
#     in-flight requests that were already running when an owner arrived (vLLM
#     never preempts a running request for a waiting one);
#   - the fluids block meters the nodes (DCGM) and prices electricity x PUE.
#     Active energy is attributed by weighted tokens to whoever worked, so
#     tiers pays for its own work: GET /admin/fluids?group_by=org gives its row.
#     Fixed costs (idle electricity; cooling through the PUE) are spread over
#     ALL clients, the third party included: idle_energy = "by_usage" (pro rata
#     to each organisation's active energy in the month and pool). With the
#     default "by_share" the third party, share 0, would pay none of it.
#
# Parsed by the test suite (config/ilaas_example_test.go) and by check-config.
master_key_file     = "/etc/llmflow/master.key"
cache_salt_key_file = "/etc/llmflow/cache-salt.key"

fluids {
  pue           = 1.3
  price_per_kwh = 0.18
  currency      = "EUR"
  idle_energy   = "by_usage" # fixed costs shared by all clients, the third party included
  idle_watts    = 150        # per GPU: the baseline is booked as idle and shared by_usage

  # Fixed costs: hosting of the racks (space, power distribution, network), an
  # amount of money per month, not energy. Spread over ALL clients like the idle
  # energy (by_usage; no usage in the month: the shares), a part of a month
  # prorated by time. Reported in the fixed_cost and total_cost columns.
  fixed_cost "racks-b300" {
    monthly = 1800
    pool    = "b300" # omit for the whole platform
    period "2026-12" {
      monthly = 1950 # per-month override
    }
  }
}

org "etabA" {
  share = 0.5
  team "lab-nlp" {}
}
org "etabB" {
  share = 0.3
  team "lab-chem" {}
}
org "etabC" {
  share = 0.2
  team "lab-vision" {}
}
org "tiers" {
  share = 0 # the third party
  team "tiers-prod" {}
}

pool "b300" {
  capacity = 256
  # no shares: the organisations' shares above apply, tiers holding 0.
  # "spare": an organisation holding no share is served only while the pool
  # has room (429 + Retry-After otherwise). Owners are unaffected.
  borrow = "spare"
}

model "llama-3.3-70b" {
  upstream_model = "meta-llama/Llama-3.3-70B-Instruct"
  pool           = "b300"
  key_file       = "/etc/llmflow/vllm.key"
  upstream "b300-01" {
    base_url = "http://10.0.0.11:8000"
    dcgm_url = "http://10.0.0.11:9400/metrics"
  }
}
