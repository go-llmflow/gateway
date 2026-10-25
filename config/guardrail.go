package config

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-llmflow/pii"
	"github.com/hashicorp/hcl/v2"
)

// Guardrail types.
const (
	GuardSecrets     = "hide_secrets"
	GuardPII         = "pii"
	GuardLlamaGuard  = "llama_guard"
	GuardPromptGuard = "prompt_guard"
	GuardHTTP        = "http"
)

// Guardrail actions.
const (
	ActionBlock = "block"
	ActionMask  = "mask"
	ActionLog   = "log"
)

// Guardrail is one guard on prompts (stage "pre") and/or completions
// (stage "post", streams included).
type Guardrail struct {
	Name string
	Type string
	// Pre and Post: the stages it runs at.
	Pre, Post bool
	// Orgs and Teams it applies to; keys add guardrails by name.
	Orgs, Teams []string
	// FailOpen lets a request through when the guard errs or times out;
	// false (default): the request is refused.
	FailOpen bool
	Timeout  time.Duration
	// Action is the default action; Actions overrides it per category
	// (a PII kind, a Llama Guard category S1…).
	Action  string
	Actions map[string]string
	// Model is the model group (class "guard") a llama_guard or
	// prompt_guard calls.
	Model string
	// Threshold is the probability above which prompt_guard flags a text.
	Threshold float64
	// StreamCheckBytes: a classifier on a stream judges the completion
	// every that many new bytes, before releasing them.
	StreamCheckBytes int
	// URL, TokenFile and AllowExternal configure an http guardrail.
	URL           string
	TokenFile     string
	AllowExternal bool
}

type guardrailHCL struct {
	Name             string            `hcl:"name,label"`
	NameRange        hcl.Range         `hcl:"name,label_range"`
	Type             string            `hcl:"type"`
	TypeRange        hcl.Range         `hcl:"type,attr_value_range"`
	Stages           []string          `hcl:"stages,optional"`
	StagesRange      hcl.Range         `hcl:"stages,attr_value_range"`
	Orgs             []string          `hcl:"orgs,optional"`
	OrgsRange        hcl.Range         `hcl:"orgs,attr_value_range"`
	Teams            []string          `hcl:"teams,optional"`
	TeamsRange       hcl.Range         `hcl:"teams,attr_value_range"`
	OnError          *string           `hcl:"on_error,optional"`
	OnErrorRange     hcl.Range         `hcl:"on_error,attr_value_range"`
	Timeout          *string           `hcl:"timeout,optional"`
	TimeoutRange     hcl.Range         `hcl:"timeout,attr_value_range"`
	Action           *string           `hcl:"action,optional"`
	ActionRange      hcl.Range         `hcl:"action,attr_value_range"`
	Actions          map[string]string `hcl:"actions,optional"`
	ActionsRange     hcl.Range         `hcl:"actions,attr_value_range"`
	Model            *string           `hcl:"model,optional"`
	ModelRange       hcl.Range         `hcl:"model,attr_value_range"`
	Threshold        *float64          `hcl:"threshold,optional"`
	ThresholdRange   hcl.Range         `hcl:"threshold,attr_value_range"`
	StreamCheckBytes *int              `hcl:"stream_check_bytes,optional"`
	StreamRange      hcl.Range         `hcl:"stream_check_bytes,attr_value_range"`
	URL              *string           `hcl:"url,optional"`
	URLRange         hcl.Range         `hcl:"url,attr_value_range"`
	TokenFile        *string           `hcl:"token_file,optional"`
	AllowExternal    *bool             `hcl:"allow_external,optional"`
	AllowRange       hcl.Range         `hcl:"allow_external,attr_value_range"`
	DefRange         hcl.Range         `hcl:",def_range"`
}

var llamaCategory = regexp.MustCompile(`^S[1-9][0-9]?$`)

// guardActions are the actions each type may take.
var guardActions = map[string][]string{
	GuardSecrets:     {ActionMask, ActionBlock, ActionLog},
	GuardPII:         {ActionMask, ActionBlock, ActionLog},
	GuardLlamaGuard:  {ActionBlock, ActionLog},
	GuardPromptGuard: {ActionBlock, ActionLog},
	GuardHTTP:        {ActionBlock, ActionLog},
}

var guardDefaultAction = map[string]string{
	GuardSecrets: ActionMask, GuardPII: ActionMask, GuardLlamaGuard: ActionBlock, GuardPromptGuard: ActionBlock, GuardHTTP: ActionBlock,
}

func (v *validator) guardrails(raw []guardrailHCL, orgs, teams map[string]bool, modelClass map[string]Class) []Guardrail {
	var out []Guardrail
	names := map[string]bool{}
	for _, g := range raw {
		cg := Guardrail{Name: g.Name, Type: g.Type, Orgs: g.Orgs, Teams: g.Teams, Actions: g.Actions,
			Timeout: v.dur(g.Timeout, g.TimeoutRange, "timeout", 2*time.Second), StreamCheckBytes: 1024, Threshold: 0.9}
		where := "guardrail " + strconv.Quote(g.Name)
		if !ValidName(g.Name) || names[g.Name] {
			v.errf(g.NameRange, "Invalid guardrail name", "%s: want [A-Za-z0-9_.-]{1,64}, declared once", where)
		}
		names[g.Name] = true
		allowed, ok := guardActions[g.Type]
		if !ok {
			v.errf(g.TypeRange, "Invalid guardrail type", "%s: type %q: want hide_secrets, pii, llama_guard, prompt_guard or http", where, g.Type)
			continue
		}
		stages := g.Stages
		if len(stages) == 0 {
			stages = []string{"pre", "post"}
			if g.Type == GuardPromptGuard {
				stages = []string{"pre"}
			}
		}
		for _, s := range stages {
			switch s {
			case "pre":
				cg.Pre = true
			case "post":
				cg.Post = true
			default:
				v.errf(g.StagesRange, "Invalid stage", "%s: stage %q: want pre or post", where, s)
			}
		}
		if g.Type == GuardPromptGuard && cg.Post {
			v.errf(g.StagesRange, "Invalid stage", "%s: prompt_guard judges prompts (stage pre) only", where)
		}
		for _, o := range g.Orgs {
			v.known(orgs, o, g.OrgsRange, "organisation", where)
		}
		for _, t := range g.Teams {
			v.known(teams, t, g.TeamsRange, "team", where)
		}
		switch strOr(g.OnError, "block") {
		case "block":
		case "allow":
			cg.FailOpen = true
		default:
			v.errf(g.OnErrorRange, "Invalid on_error", "%s: on_error %q: want block (fail-closed) or allow (fail-open)", where, *g.OnError)
		}
		if cg.Timeout < 10*time.Millisecond || cg.Timeout > time.Minute {
			v.errf(g.TimeoutRange, "Invalid timeout", "%s: timeout between 10ms and 1m", where)
		}
		cg.Action = strOr(g.Action, guardDefaultAction[g.Type])
		if !slices.Contains(allowed, cg.Action) {
			v.errf(g.ActionRange, "Invalid action", "%s: action %q: want %s", where, cg.Action, strings.Join(allowed, ", "))
		}
		for cat, act := range g.Actions {
			if !slices.Contains(allowed, act) {
				v.errf(g.ActionsRange, "Invalid action", "%s: %s = %q: want %s", where, cat, act, strings.Join(allowed, ", "))
			}
			switch g.Type {
			case GuardPII:
				if _, ok := pii.ParseKind(cat); !ok {
					v.errf(g.ActionsRange, "Unknown PII kind", "%s: %q: the kinds are email, phone, iban, nir, card, ipv4, ipv6", where, cat)
				}
			case GuardLlamaGuard:
				if !llamaCategory.MatchString(cat) {
					v.errf(g.ActionsRange, "Unknown category", "%s: %q: want a Llama Guard category S1…S99", where, cat)
				}
			case GuardSecrets:
				if cat != "token" && cat != "url-credential" {
					v.errf(g.ActionsRange, "Unknown category", "%s: %q: want token or url-credential", where, cat)
				}
			default:
				v.errf(g.ActionsRange, "No categories", "%s: a %s guardrail has one action", where, g.Type)
			}
		}
		cg.Model = strOr(g.Model, "")
		switch g.Type {
		case GuardLlamaGuard, GuardPromptGuard:
			if cl, ok := modelClass[cg.Model]; !ok {
				v.errf(g.ModelRange, "Unknown model", "%s: model %q names no model block", where, cg.Model)
			} else if cl != Guard {
				v.errf(g.ModelRange, "Not a guard model", "%s: model %q must have class = \"guard\" (clients cannot call it)", where, cg.Model)
			}
		default:
			if cg.Model != "" {
				v.errf(g.ModelRange, "Unexpected model", "%s: only llama_guard and prompt_guard call a model", where)
			}
		}
		if g.Threshold != nil {
			cg.Threshold = *g.Threshold
			if g.Type != GuardPromptGuard || !(cg.Threshold > 0 && cg.Threshold <= 1) {
				v.errf(g.ThresholdRange, "Invalid threshold", "%s: threshold applies to prompt_guard, in (0, 1]", where)
			}
		}
		if g.StreamCheckBytes != nil {
			cg.StreamCheckBytes = *g.StreamCheckBytes
			if cg.StreamCheckBytes < 64 || cg.StreamCheckBytes > 1<<20 {
				v.errf(g.StreamRange, "Invalid stream_check_bytes", "%s: between 64 and 1 MiB", where)
			}
		}
		cg.URL, cg.TokenFile = strOr(g.URL, ""), strOr(g.TokenFile, "")
		if g.AllowExternal != nil {
			cg.AllowExternal = *g.AllowExternal
		}
		if g.Type == GuardHTTP {
			if !validIssuer(cg.URL) {
				v.errf(g.URLRange, "Invalid url", "%s: url %q: want https (http only to loopback)", where, cg.URL)
			}
			// Off by default: a third-party guardrail sends prompts and
			// completions off the site, which an on-premise cluster exists
			// to avoid. It takes an explicit opt-in.
			if !cg.AllowExternal {
				v.errf(g.DefRange, "External guardrail not allowed", "%s sends prompts and completions to %s: set allow_external = true to accept that they leave the site", where, cg.URL)
			}
		} else if cg.URL != "" || cg.TokenFile != "" || g.AllowExternal != nil {
			v.errf(g.DefRange, "Unexpected http settings", "%s: url, token_file and allow_external belong to an http guardrail", where)
		}
		out = append(out, cg)
	}
	return out
}
