# Two institutions sharing the Mistral and Nova models of Amazon Bedrock in an
# EU region through Converse (OpenAI format at /v1/chat/completions), each with
# the credentials of an AWS account of its own, under hard budgets. Claude
# models use InvokeModel instead: see examples/bedrock.hcl. Parsed by the test
# suite (config/bedrock_test.go) and by check-config.
master_key_file     = "/etc/llmflow/master.key"
cache_salt_key_file = "/etc/llmflow/cache-salt.key"

org "etabA" {
  share = 0.8125
  team "lab-nlp" {}
}
org "etabB" {
  share = 0.1875
  team "lab-chem" {}
}

pool "bedrock" { capacity = 32 } # concurrent requests the institutions share at Bedrock

model "mistral-large" {
  provider = "bedrock"
  region   = "eu-west-3"
  # api = "converse" is inferred: this is not an Anthropic ID. Set it for an
  # application inference profile, which does not name its vendor.
  model_id       = "eu.mistral.mistral-large-2407-v1:0"
  pool           = "bedrock"
  allow_external = true
  orgs           = ["etabA", "etabB"]
  # Hard budgets: input bounded by the context window, output by maxTokens.
  context_window    = 128000
  max_output_tokens = 8192
  cost {
    # USD per million tokens, from aws.amazon.com/bedrock/pricing
    unit           = "USD"
    price_date     = "2026-11-01"
    input_per_mtok  = 2
    output_per_mtok = 6
  }
  org_credentials "etabA" {
    access_key_id_file     = "vault:secret/llmflow/bedrock-etabA#access_key_id"
    secret_access_key_file = "vault:secret/llmflow/bedrock-etabA#secret_access_key"
    session_token_file     = "vault:secret/llmflow/bedrock-etabA#session_token"
  }
  org_credentials "etabB" {
    bearer_token_file = "/etc/llmflow/bedrock-etabB.apikey" # a short-term Bedrock API key
  }
}

model "nova-pro" {
  provider       = "bedrock"
  region         = "eu-west-3"
  model_id       = "eu.amazon.nova-pro-v1:0"
  pool           = "bedrock"
  allow_external = true
  orgs           = ["etabA", "etabB"]
  context_window = 300000
  cost {
    unit                    = "USD"
    price_date              = "2026-11-01"
    input_per_mtok          = 0.8
    output_per_mtok         = 3.2
    cached_per_mtok         = 0.2
    cache_write_5m_per_mtok = 0.8
    cache_write_1h_per_mtok = 0.8
  }
  org_credentials "etabA" {
    access_key_id_file     = "vault:secret/llmflow/bedrock-etabA#access_key_id"
    secret_access_key_file = "vault:secret/llmflow/bedrock-etabA#secret_access_key"
    session_token_file     = "vault:secret/llmflow/bedrock-etabA#session_token"
  }
  org_credentials "etabB" {
    bearer_token_file = "/etc/llmflow/bedrock-etabB.apikey"
  }
}

# Where the vault: references above are read (OpenBao / Vault KV).
vault {
  address    = "https://bao.example.org:8200"
  token_file = "/etc/llmflow/vault.token"
}

# Required before prompts leave the site (external_requires_guardrails).
guardrail "pii" {
  type = "pii"
  orgs = ["etabA", "etabB"]
}
guardrail "secrets" {
  type = "hide_secrets"
  orgs = ["etabA", "etabB"]
}
