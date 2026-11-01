package gateway

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-llmflow/aws/eventstream"
	"github.com/go-llmflow/aws/sigv4"
	"github.com/go-llmflow/gateway/config"
	"github.com/go-llmflow/gateway/openai"
)

// Amazon Bedrock as an external provider (#89, part 2): Claude models through
// InvokeModel and InvokeModelWithResponseStream with the Anthropic Messages
// body. Sources opened 2026-10-31 are listed in config/bedrock.go; the facts
// this file relies on:
//
//   - POST /model/{modelId}/invoke and /invoke-with-response-stream, Content-Type
//     application/json, Accept application/json (non-stream) or
//     application/vnd.amazon.eventstream (stream: "the content type in the
//     response is always set to application/vnd.amazon.eventstream");
//   - the body is the Anthropic Messages body without "model" and "stream"
//     (the model is in the path, streaming is the operation), with
//     "anthropic_version": "bedrock-2023-05-31" (required) and the betas as
//     "anthropic_beta": [...] in the body;
//   - the stream is AWS event-stream framing (go-llmflow/aws/eventstream); an
//     event message carries ":message-type" = "event", ":event-type" =
//     "chunk" and a JSON payload {"bytes": <base64>} (PayloadPart.bytes:
//     "Base64-encoded bytes of payload data", at most 1000000 characters)
//     whose decoded content is ONE Anthropic stream event (message_start,
//     content_block_delta, message_delta with the output usage, message_stop);
//     errors in the stream are messages with ":message-type" = "exception"
//     (internalServerException, modelStreamErrorException,
//     modelTimeoutException, serviceUnavailableException, throttlingException,
//     validationException) and end it;
//   - requests are signed with SigV4, signing name "bedrock", or carry
//     "Authorization: Bearer <API key>" (API keys page);
//   - X-Amzn-Bedrock-GuardrailIdentifier/-GuardrailVersion/-Trace/
//     -PerformanceConfig-Latency/-Service-Tier/-Request-Metadata headers and
//     the body's "amazon-bedrock-guardrailConfig" change what a request costs
//     or reads: the gateway builds its headers from scratch and refuses a
//     client that asks for them.
//
// A budget is held at the worst case W before the request leaves, exactly as
// for the Anthropic provider (bound.go); W prices the whole context window at
// the highest input price, cache writes included, because Bedrock's implicit
// prompt caching "automatically attempts to reuse eligible prompt prefixes"
// and, with cross-region inference, "may lead to increased cache writes"
// (prompt-caching.html). A request whose final usage the gateway did not read
// (a stream cut, an exception event, a CRC failure) is charged W, the safe
// side.

var errBedrockEndpoint = errors.New("this model is served by Amazon Bedrock and is reachable at /v1/messages only (InvokeModel with the Anthropic Messages body; Converse, for other vendors' models, is a later change)")

const (
	// bedrockEventStream is the media type of a streamed answer.
	bedrockEventStream = "application/vnd.amazon.eventstream"
	// bedrockMaxMessage bounds one event-stream message: a chunk's payload is
	// at most 1 000 000 base64 characters (PayloadPart), plus the framing.
	bedrockMaxMessage = 1<<20 + 16<<10
	// bedrockMaxChunk bounds the base64 text of one chunk.
	bedrockMaxChunk = 1_000_000
)

// bedrockRefusedFields are body fields that change what a request costs or
// whose cost the gateway cannot read (#89 items 1, 2, 5, 7): guardrails
// (billed separately), the service tier and performance config (the price
// depends on the tier), Converse's additionalModelRequestFields (betas the
// allow-list must see), and Prompt management variables (the prompt is
// server-side). Refused always: the gateway sends none of them.
var bedrockRefusedFields = []string{
	"guardrailConfig", "amazon-bedrock-guardrailConfig", "amazon-bedrock-trace",
	"serviceTier", "service_tier", "performanceConfig", "performance_config", "amazon-bedrock-performanceConfig",
	"promptVariables", "additionalModelRequestFields", "requestMetadata",
}

// awsCred is the credentials of one organisation.
type awsCred struct {
	sigv4.Credentials
	bearer string
}

func (c awsCred) usable() bool {
	return c.bearer != "" || c.AccessKeyID != "" && c.SecretAccessKey != ""
}

// secrets lists the values that must never reach an output.
func (c awsCred) secrets() []string {
	var out []string
	for _, s := range []string{c.AccessKeyID, c.SecretAccessKey, c.SessionToken, c.bearer} {
		if len(s) >= 8 {
			out = append(out, s)
		}
	}
	return out
}

// bedrockRuntime is what a Bedrock group keeps besides its configuration.
type bedrockRuntime struct {
	cfg  *config.Bedrock
	def  awsCred
	orgs map[string]awsCred
}

func loadCred(refs config.AWSCredRefs, keys map[string]string) awsCred {
	if refs.BearerTokenFile != "" {
		return awsCred{bearer: keys[refs.BearerTokenFile]}
	}
	return awsCred{Credentials: sigv4.Credentials{
		AccessKeyID: keys[refs.AccessKeyIDFile], SecretAccessKey: keys[refs.SecretAccessKeyFile], SessionToken: keys[refs.SessionTokenFile]}}
}

// newBedrockRuntime loads the credentials. An error names the model and the
// organisation, never a value.
func newBedrockRuntime(model string, cfg *config.Bedrock, keys map[string]string, reach []string, isolated bool) (*bedrockRuntime, error) {
	b := &bedrockRuntime{cfg: cfg, orgs: map[string]awsCred{}}
	if !cfg.Default.Empty() {
		b.def = loadCred(cfg.Default, keys)
		if !b.def.usable() {
			return nil, fmt.Errorf("gateway: model %q: the default AWS credentials could not be loaded", model)
		}
	}
	owner := map[string]string{} // access key id (or key) → organisation
	for _, o := range reach {
		c := b.def
		if refs, ok := cfg.Orgs[o]; ok {
			c = loadCred(refs, keys)
		}
		if !c.usable() {
			return nil, fmt.Errorf("gateway: model %q: no usable AWS credentials for organisation %q", model, o)
		}
		b.orgs[o] = c
		id := "akid:" + c.AccessKeyID
		if c.bearer != "" {
			id = "bearer:" + c.bearer
		}
		if prev, dup := owner[id]; dup && isolated {
			return nil, fmt.Errorf("gateway: model %q: organisations %q and %q resolve to the same AWS credentials, so their prompt caches may be one: give each its own AWS account, or set cache_isolation = \"none\"", model, prev, o)
		}
		owner[id] = o
	}
	return b, nil
}

// CheckBedrockSecrets is check-config's look at the loaded credentials of
// every Bedrock model: each organisation's set is usable, a bearer key is a
// single printable token, and a trial signature succeeds. Errors name the
// model and the organisation, never a value.
func CheckBedrockSecrets(cfg *config.Config, keys map[string]string) error {
	for _, g := range cfg.ModelGroups {
		if g.External == nil || g.External.Bedrock == nil {
			continue
		}
		bc := g.External.Bedrock
		sets := map[string]config.AWSCredRefs{"(default)": bc.Default}
		for o, r := range bc.Orgs {
			sets[o] = r
		}
		for who, refs := range sets {
			if refs.Empty() {
				continue
			}
			c := loadCred(refs, keys)
			if !c.usable() {
				return fmt.Errorf("model %q, %s: the AWS credentials are empty", g.Name, who)
			}
			if c.bearer != "" {
				if len(c.bearer) > 4096 || strings.ContainsFunc(c.bearer, func(r rune) bool { return r <= ' ' || r >= 0x7f }) {
					return fmt.Errorf("model %q, %s: the Bedrock API key must be one printable token", g.Name, who)
				}
				continue
			}
			req, _ := http.NewRequest("POST", bc.Endpoint+"/model/x/invoke", nil)
			s := sigv4.Signer{Credentials: c.Credentials, Region: bc.Region, Service: "bedrock"}
			if err := s.Sign(req, nil, time.Unix(0, 0)); err != nil {
				return fmt.Errorf("model %q, %s: the AWS access key id or secret is malformed (%v)", g.Name, who, err)
			}
		}
	}
	return nil
}

func (b *bedrockRuntime) cred(org string) awsCred {
	if c, ok := b.orgs[org]; ok {
		return c
	}
	return b.def
}

// awsEscape percent-encodes everything but the unreserved characters, as the
// AWS SDKs do for a model ID in the path (":" becomes %3A, "/" %2F).
func awsEscape(s string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '.' || c == '_' || c == '~' {
			b.WriteByte(c)
		} else {
			b.WriteByte('%')
			b.WriteByte(hex[c>>4])
			b.WriteByte(hex[c&15])
		}
	}
	return b.String()
}

// target is the URL of the operation for base (the derived endpoint).
func (b *bedrockRuntime) target(base string, stream bool) string {
	op := "/invoke"
	if stream {
		op = "/invoke-with-response-stream"
	}
	return base + "/model/" + awsEscape(b.cfg.ModelID) + op
}

// bedrockObject decodes a request body that must be a JSON object. The JSON
// literal null decodes into a nil map without error, and writing to that map
// panicked in wireBody (found by FuzzBedrockIngress on main, 2026-11-01), so
// it is refused like any non-object.
func bedrockObject(body []byte) (map[string]json.RawMessage, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil || m == nil {
		return nil, errors.New("the request body is not a JSON object")
	}
	return m, nil
}

// checkBedrockBody refuses what the gateway cannot bound.
func checkBedrockBody(m map[string]json.RawMessage) error {
	for _, f := range bedrockRefusedFields {
		if raw, ok := m[f]; ok && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return fmt.Errorf("%q is a Bedrock option the gateway does not forward: it changes what a request costs (guardrails are billed separately, the service tier changes the price, prompt variables hide the prompt's size)", f)
		}
	}
	return nil
}

// bedrockIngress runs on the client's request before admission. It refuses the
// Bedrock options above (fields and X-Amzn-Bedrock-* headers) and moves a
// body-level "anthropic_beta" into betas, where the same allow-list and
// betaUnbounded rules as the Anthropic provider see it: a beta in the body
// must not slip past them.
func bedrockIngress(h http.Header, body []byte, betas []string) ([]byte, []string, error) {
	for k := range h {
		if strings.HasPrefix(strings.ToLower(k), "x-amzn-bedrock-") {
			return nil, nil, errors.New("X-Amzn-Bedrock-* headers (guardrails, service tier, performance config, trace, request metadata) are not forwarded: they change what a request costs")
		}
	}
	m, err := bedrockObject(body)
	if err != nil {
		return nil, nil, err
	}
	if err := checkBedrockBody(m); err != nil {
		return nil, nil, err
	}
	out := slices.Clone(betas)
	if raw, ok := m["anthropic_beta"]; ok {
		var bs []string
		if err := json.Unmarshal(raw, &bs); err != nil || len(bs) > 32 {
			return nil, nil, errors.New(`"anthropic_beta" must be an array of at most 32 strings`)
		}
		for _, b := range bs {
			if b = strings.TrimSpace(b); b != "" && len(out) < 64 && !slices.Contains(out, b) {
				out = append(out, b)
			}
		}
		delete(m, "anthropic_beta")
	}
	delete(m, "anthropic_version")
	enc, err := json.Marshal(m)
	if err != nil {
		return nil, nil, err
	}
	return enc, out, nil
}

// wireBody turns the gateway's Messages body into Bedrock's: no "model" and no
// "stream" (the path says both), the fixed anthropic_version, and the betas
// the allow-list lets through, in the body.
func (b *bedrockRuntime) wireBody(body []byte, ext *config.External, betas []string) ([]byte, error) {
	m, err := bedrockObject(body)
	if err != nil {
		return nil, err
	}
	if err := checkBedrockBody(m); err != nil {
		return nil, err
	}
	delete(m, "model")
	delete(m, "stream")
	delete(m, "anthropic_beta")
	m["anthropic_version"], _ = json.Marshal(config.BedrockVersion)
	var allowed []string
	for _, bt := range betas {
		if ext.AllowsBeta(bt) && !slices.Contains(allowed, bt) {
			allowed = append(allowed, bt)
		}
	}
	if len(allowed) > 0 {
		m["anthropic_beta"], _ = json.Marshal(allowed)
	}
	return json.Marshal(m)
}

// prepare builds the request's URL, headers and body (signed over exactly the
// bytes sent, at the server's clock).
func (b *bedrockRuntime) prepare(req *http.Request, body []byte, org string, now time.Time) error {
	c := b.cred(org)
	req.Header.Set("Content-Type", "application/json")
	if c.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+c.bearer)
		return nil
	}
	s := sigv4.Signer{Credentials: c.Credentials, Region: b.cfg.Region, Service: "bedrock"}
	return s.Sign(req, body, now)
}

// redact removes the organisation's secrets from a provider message.
func (b *bedrockRuntime) redact(_, s string) string {
	// Every organisation's secrets, not only the caller's: a provider message
	// must not carry one institution's credentials to another.
	all := []awsCred{b.def}
	for _, c := range b.orgs {
		all = append(all, c)
	}
	for _, c := range all {
		for _, sec := range c.secrets() {
			s = strings.ReplaceAll(s, sec, "[redacted]")
		}
	}
	return s
}

// bedrockErrorBody translates a Bedrock error answer ({"message": "..."}) to
// the Anthropic error shape /v1/messages clients read. 401 and 403 say only
// that the provider refused the credentials: its message can describe the
// signature it computed.
func (b *bedrockRuntime) errorBody(org string, status int, data []byte) []byte {
	typ := "api_error"
	switch {
	case status == 400:
		typ = "invalid_request_error"
	case status == 401:
		typ = "authentication_error"
	case status == 403:
		typ = "permission_error"
	case status == 404:
		typ = "not_found_error"
	case status == 429:
		typ = "rate_limit_error"
	}
	msg := "the provider answered " + strconv.Itoa(status)
	if status != 401 && status != 403 {
		var e struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(data, &e) == nil && e.Message != "" {
			msg = clipText(b.redact(org, printable(e.Message)), 300)
		}
	} else {
		msg = "the provider refused the gateway's credentials for this organisation"
	}
	out, _ := json.Marshal(map[string]any{"type": "error", "error": map[string]string{"type": typ, "message": msg}})
	return out
}

// clipText cuts s to at most n bytes on a rune boundary.
func clipText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "..."
}

// printable drops control characters.
func printable(s string) string {
	return strings.Map(func(r rune) rune {
		if r < ' ' || r == 0x7f || r == utf8.RuneError {
			return -1
		}
		return r
	}, s)
}

// ---- the stream ----

// bedrockSSE reads an InvokeModelWithResponseStream answer (AWS event-stream
// framing, untrusted input: eventstream.Limits bound lengths and header
// counts, both CRCs are checked) and presents it as the Anthropic SSE stream
// /v1/messages clients and openai.CopySSEFor read. It records whether the
// stream is COMPLETE: a message_start, a message_delta with the output usage
// and a message_stop, then a clean end at a message boundary, and no
// exception. Anything else is not, and the request is then charged W.
type bedrockSSE struct {
	dec                *eventstream.Decoder
	out                bytes.Buffer
	done, failed       bool
	start, usage, stop bool
}

func newBedrockSSE(r io.Reader) *bedrockSSE {
	return &bedrockSSE{dec: eventstream.NewDecoder(r, eventstream.Limits{
		MaxMessage: bedrockMaxMessage, MaxHeadersLen: 4 << 10, MaxHeaders: 16, MaxValueLen: 256})}
}

// Complete reports whether the final usage was delivered.
func (b *bedrockSSE) Complete() bool {
	return b.done && !b.failed && b.start && b.usage && b.stop
}

func (b *bedrockSSE) Read(p []byte) (int, error) {
	for b.out.Len() == 0 {
		if b.done {
			return 0, io.EOF
		}
		b.step()
	}
	return b.out.Read(p)
}

// fail ends the stream with an Anthropic error event. why is a fixed text.
func (b *bedrockSSE) fail(why string) {
	b.failed, b.done = true, true
	msg, _ := json.Marshal(map[string]any{"type": "error", "error": map[string]string{"type": "api_error", "message": why}})
	b.out.WriteString("event: error\ndata: ")
	b.out.Write(msg)
	b.out.WriteString("\n\n")
}

func (b *bedrockSSE) step() {
	msg, err := b.dec.Next()
	if err != nil {
		b.done = true
		if !errors.Is(err, io.EOF) {
			b.fail("the stream from the provider was interrupted or malformed")
		} else if !(b.start && b.usage && b.stop) {
			b.fail("the stream from the provider ended before message_stop")
		}
		return
	}
	mt, _ := msg.Header(":message-type")
	switch mt.Str() {
	case "event":
		if et, _ := msg.Header(":event-type"); et.Str() == "chunk" {
			b.chunk(msg.Payload)
		}
	case "exception", "error":
		b.fail("the provider ended the stream with an exception")
	default:
		b.fail("the stream from the provider carried an unknown message type")
	}
}

func validEventType(s string) bool {
	if s == "" || len(s) > 40 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; c < 'a' || c > 'z' {
			if c != '_' {
				return false
			}
		}
	}
	return true
}

func (b *bedrockSSE) chunk(payload []byte) {
	var pl struct {
		Bytes string `json:"bytes"`
	}
	if err := json.Unmarshal(payload, &pl); err != nil || len(pl.Bytes) > bedrockMaxChunk {
		b.fail("a chunk of the provider's stream could not be read")
		return
	}
	raw, err := base64.StdEncoding.DecodeString(pl.Bytes)
	if err != nil {
		b.fail("a chunk of the provider's stream could not be read")
		return
	}
	var ev struct {
		Type  string `json:"type"`
		Usage *struct {
			OutputTokens *int64 `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &ev); err != nil || !validEventType(ev.Type) {
		b.fail("a chunk of the provider's stream could not be read")
		return
	}
	var data bytes.Buffer
	if err := json.Compact(&data, raw); err != nil {
		b.fail("a chunk of the provider's stream could not be read")
		return
	}
	b.out.WriteString("event: " + ev.Type + "\ndata: ")
	b.out.Write(data.Bytes())
	b.out.WriteString("\n\n")
	switch ev.Type {
	case "message_start":
		b.start = true
	case "message_delta":
		if b.start && ev.Usage != nil && ev.Usage.OutputTokens != nil {
			b.usage = true
		}
	case "message_stop":
		if b.start && b.usage {
			b.stop = true
		}
	case "error":
		b.failed = true
	}
}

// settleBedrockStream is what a Bedrock stream is charged under a hold of w:
// the cost of the usage it delivered when it is complete, else w itself (the
// worst case, the safe side), whatever partial usage it carried.
func settleBedrockStream(complete bool, st openai.StreamResult, p groupPrices, w int64) (costNano int64, atW bool) {
	if !complete || !st.HaveUsage {
		return w, true
	}
	return p.costNano(st.Usage), false
}
