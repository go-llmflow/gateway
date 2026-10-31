package gateway

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/go-llmflow/gateway/config"
)

// FuzzBedrockIngress: whatever the client sends, a body that passes the
// ingress carries none of the refused options, no body-level beta or version
// (the betas moved to the allow-list), and no X-Amzn-Bedrock header; and what
// goes on the wire is the Anthropic body without model and stream.
func FuzzBedrockIngress(f *testing.F) {
	for _, s := range []string{
		`{"model":"claude","max_tokens":1,"messages":[]}`,
		`{"guardrailConfig":{}}`, `{"serviceTier":"priority"}`, `{"anthropic_beta":["context-1m-2025-08-07"]}`,
		`{"anthropic_beta":"x"}`, `{"promptVariables":null}`, `[]`, `{`, `{"additionalModelRequestFields":{}}`,
		`{"anthropic_version":"2023-06-01","stream":true,"model":"m","max_tokens":9}`,
	} {
		f.Add([]byte(s), "Content-Type", "x")
	}
	f.Add([]byte(`{"max_tokens":1}`), "X-Amzn-Bedrock-GuardrailIdentifier", "g")
	ext := &config.External{Betas: []string{"*"}}
	b := &bedrockRuntime{cfg: &config.Bedrock{Region: "eu-west-3", ModelID: "anthropic.claude-3-haiku-20240307-v1:0"}}
	f.Fuzz(func(t *testing.T, body []byte, hk, hv string) {
		h := http.Header{}
		if hk != "" && !strings.ContainsAny(hk, " \t\r\n:") {
			h[hk] = []string{hv}
		}
		out, betas, err := bedrockIngress(h, body, nil)
		if err != nil {
			return
		}
		for k := range h {
			if strings.HasPrefix(strings.ToLower(k), "x-amzn-bedrock-") {
				t.Fatalf("header %q accepted", k)
			}
		}
		var m map[string]json.RawMessage
		if json.Unmarshal(out, &m) != nil {
			t.Fatalf("the ingress returned a non-object %q", out)
		}
		for _, k := range append([]string{"anthropic_beta", "anthropic_version"}, bedrockRefusedFields...) {
			if raw, ok := m[k]; ok && string(raw) != "null" {
				t.Fatalf("field %s survived the ingress: %s", k, out)
			}
		}
		wire, err := b.wireBody(out, ext, betas)
		if err != nil {
			t.Fatalf("wireBody after a good ingress: %v", err)
		}
		var w map[string]json.RawMessage
		if json.Unmarshal(wire, &w) != nil {
			t.Fatalf("wire body %q", wire)
		}
		if _, ok := w["model"]; ok {
			t.Fatal("model on the wire")
		}
		if _, ok := w["stream"]; ok {
			t.Fatal("stream on the wire")
		}
		if string(w["anthropic_version"]) != `"bedrock-2023-05-31"` {
			t.Fatalf("version %s", w["anthropic_version"])
		}
	})
}

// A stored or masked body that still carries a refused option is caught again
// when the request is built (defence in depth).
func TestBedrockWireBodyRefusesAgain(t *testing.T) {
	b := &bedrockRuntime{cfg: &config.Bedrock{}}
	if _, err := b.wireBody([]byte(`{"max_tokens":1,"guardrailConfig":{"a":1}}`), &config.External{}, nil); err == nil {
		t.Fatal("guardrailConfig reached the wire builder")
	}
}

func TestCheckBedrockSecrets(t *testing.T) {
	cfg, err := config.Parse([]byte(bedrockHCL("http://127.0.0.1:1", "http://127.0.0.1:2", "")), "x.hcl")
	if err != nil {
		t.Fatal(err)
	}
	keys := map[string]string{"/aws/alpha-id": akidAlpha, "/aws/alpha-secret": skAlpha, "/aws/alpha-token": tokAlpha, "/aws/beta-bearer": bearerBeta}
	if err := CheckBedrockSecrets(cfg, keys); err != nil {
		t.Fatalf("good secrets: %v", err)
	}
	keys["/aws/alpha-id"] = "AKID BAD"
	err = CheckBedrockSecrets(cfg, keys)
	if err == nil || strings.Contains(err.Error(), "AKID BAD") || !strings.Contains(err.Error(), "etabA") {
		t.Fatalf("a malformed key id: %v", err)
	}
	keys["/aws/alpha-id"] = akidAlpha
	keys["/aws/beta-bearer"] = "two words"
	if err := CheckBedrockSecrets(cfg, keys); err == nil || strings.Contains(err.Error(), "two words") {
		t.Fatalf("a bearer with a space: %v", err)
	}
}
