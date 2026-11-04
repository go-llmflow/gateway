# Several identity providers (v0.27.0): two institutions that run their own
# OpenID provider, and a federation proxy (eduGAIN through MyAccessID,
# eduTEAMS, Renater or a Keycloak broker) for the partners, filtered to a list
# of accepted institutions. Parsed by the test suite (config/idp_test.go).
master_key_file     = "/etc/llmflow/master.key"
cache_salt_key_file = "/etc/llmflow/cache-salt.key"

org "univ-a" {
  share = 0.4
  team "lab-a" {}
}
org "univ-b" {
  share = 0.4
  team "lab-b" {}
}
org "partners" {
  share = 0.2
  team "lab-partners" {}
}

pool "b300" { capacity = 256 }

vault {
  address    = "https://bao.example.org:8200"
  token_file = "/etc/llmflow/vault.token"
}

model "llama-3.3-70b" {
  upstream_model = "meta-llama/Llama-3.3-70B-Instruct"
  pool           = "b300"
  key_file       = "/etc/llmflow/vllm.key"
  upstream "gpu01" { base_url = "http://10.0.0.11:8000" }
}

# Admin login. redirect_url is this gateway's callback base: each provider's
# own callback is /admin/callback/<name>, the redirect URI to register at that
# provider (RFC 9700 §4.4.2.2: a distinct redirect URI per issuer).
sso {
  redirect_url = "https://llm.example.org/admin/callback"

  # An institution's own provider. It can vouch for its own organisation
  # only: a role it would grant on another organisation is refused.
  provider "univ-a" {
    issuer             = "https://sso.univ-a.example"
    client_id          = "llmflow-admin"
    client_secret_file = "vault:secret/llmflow#sso-univ-a"
    orgs               = ["univ-a"]
    role {
      claim = "groups"
      value = "llm-admins"
      role  = "org_admin"
      org   = "univ-a"
    }
    role {
      claim = "groups"
      value = "physics"
      role  = "member"
      team  = "lab-a"
    }
  }

  provider "univ-b" {
    issuer             = "https://login.univ-b.example/realms/main"
    client_id          = "llmflow-admin"
    client_secret_file = "vault:secret/llmflow#sso-univ-b"
    orgs               = ["univ-b"]
    role {
      claim = "groups"
      value = "llm-admins"
      role  = "org_admin"
      org   = "univ-b"
    }
  }

  # A federation proxy. Whoever it authenticates may log in only when the
  # institution it states is on the list. schac_home_organization is a domain
  # name (SCHAC 1.6.0), compared without regard to case; the claim must be in
  # the ID token (a Keycloak mapper, or the proxy's scope), the gateway does
  # not call the userinfo endpoint.
  provider "partners" {
    issuer             = "https://proxy.federation.example"
    client_id          = "llmflow-admin"
    client_secret_file = "vault:secret/llmflow#sso-partners"
    orgs               = ["partners"]
    accept {
      claim  = "schac_home_organization"
      values = ["univ-c.example", "univ-d.example"]
    }
    role {
      claim = "schac_home_organization"
      value = "univ-c.example"
      role  = "member"
      team  = "lab-partners"
    }
  }
}

# Inference access tokens, one block per issuer; a token is routed to its
# verifier by its iss claim, an unknown iss is a 401.
jwt "univ-a" {
  issuer   = "https://sso.univ-a.example"
  audience = "llmflow"
  orgs     = ["univ-a"]
  map {
    claim = "groups"
    value = "physics"
    team  = "lab-a"
  }
}

jwt "partners" {
  issuer   = "https://proxy.federation.example"
  audience = "llmflow-api"
  orgs     = ["partners"]
  accept {
    claim  = "schac_home_organization"
    values = ["univ-c.example", "univ-d.example"]
  }
  map {
    claim = "groups"
    value = "llm-users"
    team  = "lab-partners"
  }
}
