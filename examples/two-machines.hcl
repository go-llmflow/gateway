# Two machines, two apportionments, one gateway.
#
#   - the B300 servers (the Hi! PARIS partners): 256 slots, apportioned by the
#     organisations' own shares (no `shares` on the pool);
#   - a machine with 8 RTX 6000 GPUs bought by three institutions in the
#     proportions of what each paid: 50 / 30 / 20 %. Each is guaranteed its
#     slots of THIS machine (largest remainder over C = 64: 32 / 19 / 13) and
#     may borrow the GPUs that are free. The fourth organisation did not
#     contribute and, with borrow = "shareholders", may not use the machine.
#
# Parsed by the test suite (config/poolshares_test.go) and by check-config.
master_key_file     = "/etc/llmflow/master.key"
cache_salt_key_file = "/etc/llmflow/cache-salt.key"

org "etabA" {
  share = 0.5
  team "lab-nlp" {}
}
org "etabB" {
  share = 0.25
  team "lab-chem" {}
}
org "etabC" {
  share = 0.25
  team "lab-vision" {}
}
org "etabD" {
  share = 0 # holds no share of the B300: it only borrows there
  team "lab-bio" {}
}

pool "b300" {
  capacity = 256
  # no shares: the organisations' shares above apply to this pool
}

pool "rtx6000" {
  capacity = 64 # 8 GPUs, 8 concurrent requests each
  # What each institution paid for the machine; an organisation not listed
  # (etabB) has a reservation of 0 here.
  shares = { etabA = 0.5, etabC = 0.3, etabD = 0.2 }
  # "all" (the default): idle slots are lent to anyone, at borrowing priority.
  # "shareholders": only the contributors may use the machine.
  borrow = "shareholders"
}

model "llama-3.3-70b" {
  upstream_model = "meta-llama/Llama-3.3-70B-Instruct"
  pool           = "b300"
  key_file       = "/etc/llmflow/vllm.key"
  upstream "gpu01" { base_url = "http://10.0.0.11:8000" }
}

model "qwen-32b" {
  upstream_model = "Qwen/Qwen3-32B"
  pool           = "rtx6000"
  key_file       = "/etc/llmflow/vllm.key"
  upstream "rtx01" { base_url = "http://10.0.0.21:8000" }
}
