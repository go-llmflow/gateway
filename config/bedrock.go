package config

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/hashicorp/hcl/v2"
)

// ProviderBedrock is Amazon Bedrock, reached with InvokeModel and
// InvokeModelWithResponseStream and the Anthropic Messages body, for Claude
// models (#89). Converse, for the other vendors' models, is a later change.
//
// References opened 2026-10-31:
//
//   - InvokeModel / InvokeModelWithResponseStream API references
//     (docs.aws.amazon.com/bedrock/latest/APIReference/API_runtime_InvokeModel.html and
//     ..._InvokeModelWithResponseStream.html): POST /model/{modelId}/invoke and
//     /invoke-with-response-stream; modelId may be a base model ID or ARN, an
//     inference profile ID or ARN, a prompt ARN (Prompt management), a
//     Provisioned Throughput ARN, a custom or imported model ARN, a SageMaker
//     endpoint, or a prompt router ARN; the headers X-Amzn-Bedrock-Trace,
//     -GuardrailIdentifier, -GuardrailVersion, -PerformanceConfig-Latency
//     (standard | optimized), -Service-Tier (priority | default | flex |
//     reserved) and -Request-Metadata; guardrails also take
//     "amazon-bedrock-guardrailConfig" in the body;
//   - Anthropic Claude Messages on Bedrock
//     (.../userguide/model-parameters-anthropic-claude-messages-request-response.html):
//     "anthropic_version – (Required) … The value must be bedrock-2023-05-31",
//     "anthropic_beta" a list of beta strings IN THE BODY, "max_tokens
//     (Required)", the response's usage input_tokens and output_tokens;
//   - API keys (.../userguide/api-keys.html): a short-term key "lasts up to 12
//     hours … Recommended for production use", a long-term key "Recommended
//     only for exploration", sent as "Authorization: Bearer <key>".
const ProviderBedrock = "bedrock"

// BedrockVersion is the anthropic_version Bedrock requires.
const BedrockVersion = "bedrock-2023-05-31"

// AWSCredRefs names the secret references (a file path or a Vault reference,
// like every key_file) of one set of AWS credentials. Never the secrets.
type AWSCredRefs struct {
	AccessKeyIDFile     string
	SecretAccessKeyFile string
	SessionTokenFile    string
	// BearerTokenFile is a Bedrock API key (a short-term one in production).
	BearerTokenFile string
}

// Empty reports whether nothing is set.
func (c AWSCredRefs) Empty() bool { return c == AWSCredRefs{} }

// Refs lists the references that are set.
func (c AWSCredRefs) Refs() []string {
	var out []string
	for _, r := range []string{c.AccessKeyIDFile, c.SecretAccessKeyFile, c.SessionTokenFile, c.BearerTokenFile} {
		if r != "" {
			out = append(out, r)
		}
	}
	return out
}

// Bedrock configures a model served by Amazon Bedrock.
type Bedrock struct {
	Region  string
	ModelID string
	// API is APIInvoke (Anthropic Messages body, /v1/messages) or APIConverse
	// (/v1/chat/completions).
	API string
	// Endpoint is the base URL requests go to: derived from the region, or the
	// endpoint_override of a test.
	Endpoint string
	// Default is used by organisations without credentials of their own.
	Default AWSCredRefs
	// Orgs maps an organisation to ITS credentials (one AWS account per
	// organisation keeps the institutions' prompt caches apart: the caching
	// page does not say whether caches are isolated per account).
	Orgs     map[string]AWSCredRefs
	OrgOrder []string
}

// CredsFor returns the credentials of org.
func (b *Bedrock) CredsFor(org string) AWSCredRefs {
	if c, ok := b.Orgs[org]; ok {
		return c
	}
	return b.Default
}

type awsCredsHCL struct {
	AccessKeyIDFile     *string   `hcl:"access_key_id_file,optional"`
	SecretAccessKeyFile *string   `hcl:"secret_access_key_file,optional"`
	SessionTokenFile    *string   `hcl:"session_token_file,optional"`
	BearerTokenFile     *string   `hcl:"bearer_token_file,optional"`
	DefRange            hcl.Range `hcl:",def_range"`
}

type orgCredsHCL struct {
	Org                 string    `hcl:"org,label"`
	OrgRange            hcl.Range `hcl:"org,label_range"`
	AccessKeyIDFile     *string   `hcl:"access_key_id_file,optional"`
	SecretAccessKeyFile *string   `hcl:"secret_access_key_file,optional"`
	SessionTokenFile    *string   `hcl:"session_token_file,optional"`
	BearerTokenFile     *string   `hcl:"bearer_token_file,optional"`
	DefRange            hcl.Range `hcl:",def_range"`
}

var regionRe = regexp.MustCompile(`^[a-z]{2}(-[a-z]+)+-[0-9]{1,2}$`)

// BedrockEndpoint derives the endpoint from the region:
// https://bedrock-runtime.<region>.amazonaws.com (".amazonaws.com.cn" for the
// China regions). The host is never taken from a client or from the
// environment.
func BedrockEndpoint(region string) string {
	suffix := "amazonaws.com"
	if strings.HasPrefix(region, "cn-") {
		suffix = "amazonaws.com.cn"
	}
	return "https://bedrock-runtime." + region + "." + suffix
}

// ValidRegion reports whether s looks like an AWS region name.
func ValidRegion(s string) bool { return len(s) <= 32 && regionRe.MatchString(s) }

var (
	bedrockPlainID = regexp.MustCompile(`^(?:(?:eu|us|apac|au|jp|us-gov|global)\.)?anthropic\.[A-Za-z0-9.-]+(?::[0-9]{1,3})?$`)
	bedrockCharset = regexp.MustCompile(`^[A-Za-z0-9._:/-]+$`)
)

// CheckBedrockModelID accepts what the gateway can bound and price: an
// Anthropic base model ID or inference profile ID (geographic or global), or
// the ARN of a foundation model, an inference profile or an application
// inference profile in the configured region. It refuses, each with its
// reason (#89): a prompt router (Bedrock picks the model, so the price),
// Prompt management ARNs (the prompt is server-side, its size unknown),
// Provisioned Throughput (billed per hour, not per token), and custom,
// imported, Marketplace and SageMaker resources (price and vendor unknown).
func CheckBedrockModelID(id, region string) error {
	return CheckBedrockModelIDFor(id, region, APIInvoke)
}

// The two operations a Bedrock model is reached through (#89): InvokeModel
// with the Anthropic Messages body for Claude models, at /v1/messages; and
// Converse / ConverseStream for the models of the other vendors (Mistral,
// Llama, Nova …), at /v1/chat/completions.
const (
	APIInvoke   = "invoke"
	APIConverse = "converse"
)

// bedrockVendorID is any vendor's base model or inference profile ID.
var bedrockVendorID = regexp.MustCompile(`^(?:(?:eu|us|apac|au|jp|us-gov|global)\.)?[a-z0-9-]+\.[A-Za-z0-9.-]+(?::[0-9]{1,3})?$`)

// CheckBedrockModelIDFor is CheckBedrockModelID for an operation: InvokeModel
// takes Anthropic models only; Converse takes any vendor's.
func CheckBedrockModelIDFor(id, region, api string) error {
	if id == "" || len(id) > 2048 || !bedrockCharset.MatchString(id) {
		return errors.New("want a model ID such as \"anthropic.claude-sonnet-4-5-20250929-v1:0\", \"eu.mistral.mistral-large-2407-v1:0\" or an inference profile ID")
	}
	if !strings.HasPrefix(id, "arn:") {
		if api == APIConverse && bedrockVendorID.MatchString(id) || bedrockPlainID.MatchString(id) {
			return nil
		}
		if api == APIConverse {
			return errors.New("want [geo.]<vendor>.<model>")
		}
		return errors.New("only Anthropic Claude models are served through InvokeModel (set api = \"converse\" for the other vendors' models): want [geo.]anthropic.<model>")
	}
	parts := strings.SplitN(id, ":", 6)
	if len(parts) != 6 || parts[2] != "bedrock" {
		return errors.New("an ARN must be a Bedrock resource: arn:aws:bedrock:<region>:<account>:<resource>")
	}
	if region != "" && parts[3] != region {
		return fmt.Errorf("the ARN is in region %q but the model's region is %q", parts[3], region)
	}
	kind, _, _ := strings.Cut(parts[5], "/")
	switch kind {
	case "foundation-model", "inference-profile", "application-inference-profile":
		return nil
	case "prompt-router", "default-prompt-router":
		return errors.New("a prompt router chooses the model, and so the price, itself: a request's cost could not be bounded")
	case "prompt":
		return errors.New("a Prompt management prompt lives on the server: its size is unknown to the gateway, so a request's cost could not be bounded")
	case "provisioned-model":
		return errors.New("Provisioned Throughput is billed per hour, not per token: it has no per-request cost to bound (out of scope)")
	case "custom-model", "custom-model-deployment", "imported-model":
		return errors.New("custom and imported models have a price and a vendor the gateway does not know: use a foundation model or an inference profile")
	}
	return fmt.Errorf("resource %q is not a foundation model or an inference profile", kind)
}

// checkOverride accepts the endpoint of a test: https, or http to a loopback
// address.
func checkOverride(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return errors.New("want a bare scheme://host[:port]")
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && loopbackHost(u.Hostname())) {
		return errors.New("want https (http only to a loopback address, for tests)")
	}
	return nil
}

func refsOf(ak, sk, st, bt *string) AWSCredRefs {
	return AWSCredRefs{AccessKeyIDFile: strOr(ak, ""), SecretAccessKeyFile: strOr(sk, ""), SessionTokenFile: strOr(st, ""), BearerTokenFile: strOr(bt, "")}
}

// credsOK says why a set of references is not usable, or "".
func credsOK(c AWSCredRefs) string {
	switch {
	case c.Empty():
		return "no credentials: set access_key_id_file and secret_access_key_file, or bearer_token_file"
	case c.BearerTokenFile != "" && (c.AccessKeyIDFile != "" || c.SecretAccessKeyFile != "" || c.SessionTokenFile != ""):
		return "bearer_token_file and the SigV4 keys are two ways to authenticate: set one"
	case c.BearerTokenFile != "":
		return ""
	case c.AccessKeyIDFile == "" || c.SecretAccessKeyFile == "":
		return "access_key_id_file and secret_access_key_file go together"
	}
	return ""
}

// bedrockAPI is the operation a model is reached through: api when set, else
// InvokeModel for an Anthropic model ID (or ARN), Converse for the rest.
func bedrockAPI(m modelHCL, id string) string {
	if m.API != nil && (*m.API == APIInvoke || *m.API == APIConverse) {
		return *m.API
	}
	if bedrockPlainID.MatchString(id) || strings.Contains(id, "/anthropic.") || strings.Contains(id, "/eu.anthropic.") ||
		strings.Contains(id, "/us.anthropic.") || strings.Contains(id, "/global.anthropic.") || strings.Contains(id, "/apac.anthropic.") {
		return APIInvoke
	}
	return APIConverse
}

// bedrockSetup validates the Bedrock settings of model m and fills e and g:
// the model ID becomes the upstream model, and the one upstream is derived
// from the region. Called with g.External = e.
func (v *validator) bedrockSetup(m modelHCL, g *ModelGroup, e *External, where string) {
	b := &Bedrock{Orgs: map[string]AWSCredRefs{}}
	e.Bedrock = b
	for _, set := range []struct {
		on   bool
		name string
	}{{len(m.Upstreams) > 0, "upstream blocks (the endpoint is derived from region)"}, {len(m.OrgKeys) > 0, "org_key (use org_credentials)"},
		{m.KeyFile != nil, "key_file (use credentials)"}, {len(m.AnthropicVersions) > 0, "anthropic_versions (Bedrock requires " + BedrockVersion + ")"}} {
		if set.on {
			v.errf(m.DefRange, "Unexpected setting", "%s: a Bedrock model does not take %s", where, set.name)
		}
	}
	b.Region = strOr(m.Region, "")
	if m.Region == nil || !ValidRegion(b.Region) {
		r := m.RegionRange
		if m.Region == nil {
			r = m.DefRange
		}
		v.errf(r, "Invalid region", "%s: region = %q: want an AWS region such as \"eu-west-3\"", where, b.Region)
	}
	b.ModelID = strOr(m.ModelID, "")
	b.API = bedrockAPI(m, b.ModelID)
	if m.API != nil && *m.API != APIInvoke && *m.API != APIConverse {
		v.errf(m.APIRange, "Invalid api", "%s: api = %q: want \"invoke\" (Claude, at /v1/messages) or \"converse\" (any vendor, at /v1/chat/completions)", where, *m.API)
	}
	if m.API == nil && strings.Contains(b.ModelID, ":application-inference-profile/") {
		v.errf(m.DefRange, "Missing api", "%s: an application inference profile does not name its vendor: set api = \"invoke\" (a Claude model) or \"converse\"", where)
	}
	if b.API == APIConverse && len(m.AnthropicBetas) > 0 {
		v.errf(m.BetasRange, "Unexpected setting", "%s: anthropic_betas belong to InvokeModel; Converse takes no betas (additionalModelRequestFields is refused: it can carry anything that widens a request's cost)", where)
	}
	if m.ModelID == nil {
		v.errf(m.DefRange, "Missing model_id", "%s: set model_id to the exact model ID or inference profile ID (the price is set per configured model ID)", where)
	} else if err := CheckBedrockModelIDFor(b.ModelID, b.Region, b.API); err != nil {
		v.errf(m.ModelIDRange, "Invalid model_id", "%s: model_id %q: %v", where, b.ModelID, err)
	}
	g.UpstreamModel = b.ModelID
	b.Endpoint = BedrockEndpoint(b.Region)
	if m.EndpointOverride != nil {
		if err := checkOverride(*m.EndpointOverride); err != nil {
			v.errf(m.EndpointOverrideRange, "Invalid endpoint_override", "%s: %v", where, err)
		} else {
			b.Endpoint = strings.TrimSuffix(*m.EndpointOverride, "/")
		}
	}
	if len(m.Credentials) > 1 {
		v.errf(m.Credentials[1].DefRange, "Duplicate credentials", "%s: one credentials block", where)
	}
	if len(m.Credentials) > 0 {
		c := m.Credentials[0]
		b.Default = refsOf(c.AccessKeyIDFile, c.SecretAccessKeyFile, c.SessionTokenFile, c.BearerTokenFile)
		if why := credsOK(b.Default); why != "" {
			v.errf(c.DefRange, "Invalid credentials", "%s: %s", where, why)
		}
	}
	for _, oc := range m.OrgCredentials {
		refs := refsOf(oc.AccessKeyIDFile, oc.SecretAccessKeyFile, oc.SessionTokenFile, oc.BearerTokenFile)
		if _, dup := b.Orgs[oc.Org]; dup {
			v.errf(oc.DefRange, "Duplicate org_credentials", "%s: organisation %q has two credential sets", where, oc.Org)
			continue
		}
		if why := credsOK(refs); why != "" {
			v.errf(oc.DefRange, "Invalid org_credentials", "%s: organisation %q: %s", where, oc.Org, why)
		}
		b.Orgs[oc.Org] = refs
		b.OrgOrder = append(b.OrgOrder, oc.Org)
	}
	g.Upstreams = []Upstream{{Name: "bedrock", BaseURL: b.Endpoint}}
}

// bedrockKeys checks who may use the model against the credentials: every
// organisation that may must have some, and, when the institutions' caches
// must stay apart (cache_isolation is not "none"), organisations that may
// share the model must not share an AWS credential set.
func (v *validator) bedrockKeys(m modelHCL, e *External, reach map[string]bool, isolated bool, where string) {
	b := e.Bedrock
	for _, oc := range m.OrgCredentials {
		if !reach[oc.Org] {
			v.errf(oc.OrgRange, "Useless org_credentials", "%s: organisation %q is not allowed to use the model (orgs/teams)", where, oc.Org)
		}
	}
	seen := map[string]string{}
	for _, o := range b.OrgOrder {
		for _, ref := range []string{b.Orgs[o].AccessKeyIDFile, b.Orgs[o].BearerTokenFile} {
			if ref == "" {
				continue
			}
			if prev, dup := seen[ref]; dup && prev != o && isolated {
				v.errf(m.DefRange, "Shared AWS credentials",
					"%s: organisations %q and %q use the same credential reference %q; each needs an AWS account of its own, or set cache_isolation = \"none\" to accept that they share a prompt cache", where, prev, o, ref)
			}
			seen[ref] = o
		}
	}
	for o := range reach {
		if _, own := b.Orgs[o]; own {
			continue
		}
		switch {
		case b.Default.Empty():
			v.errf(m.DefRange, "Missing credentials", "%s: organisation %q has no org_credentials and the model has no credentials block", where, o)
		case isolated && len(reach) > 1:
			v.errf(m.DefRange, "Shared AWS account",
				"%s: organisation %q has no org_credentials of its own. The prompt caching page (docs.aws.amazon.com/bedrock/latest/userguide/prompt-caching.html) does not say that caches are isolated per AWS account, so institutions sharing one credential set may share one cache: "+
					"give each organisation an org_credentials of its own AWS account, or set cache_isolation = \"none\" to accept a shared cache", where, o)
		}
	}
}
