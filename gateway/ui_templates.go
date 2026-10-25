package gateway

// The admin UI's templates (html/template: every value is escaped for its
// context) and stylesheet. No script anywhere: the CSP forbids it.

const uiCSS = `body{font:15px/1.45 system-ui,sans-serif;margin:0;color:#1d2330;background:#fafbfc}
header{background:#1d2330;color:#fff;padding:.6rem 1rem;display:flex;gap:1rem;align-items:center;flex-wrap:wrap}
header a{color:#cfe0ff;text-decoration:none}header .who{margin-left:auto}
main{padding:1rem;max-width:1200px}
table{border-collapse:collapse;width:100%;margin:.6rem 0;font-size:13px}
th,td{border-bottom:1px solid #dde2ea;padding:.3rem .5rem;text-align:left;vertical-align:top}
th{background:#eef1f6}
form.inline{display:inline}
fieldset{border:1px solid #dde2ea;margin:1rem 0;padding:.6rem}
input,select{font:inherit;padding:.15rem .3rem}
button{font:inherit;padding:.2rem .6rem;cursor:pointer}
.msg{background:#e7f6ea;padding:.4rem .6rem}.err{background:#fde8e8;padding:.4rem .6rem}
.secret{font:16px monospace;background:#fff6d6;padding:.6rem;word-break:break-all}
.bad{color:#a00;font-weight:600}.good{color:#060;font-weight:600}
`

const uiHTML = `
{{define "head"}}<!doctype html><html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>{{.Title}} · llmflow</title><link rel="stylesheet" href="/ui/style.css"></head><body>
<header><strong>llmflow</strong>
<a href="/ui/">Home</a><a href="/ui/keys">Keys</a><a href="/ui/budgets">Budgets</a><a href="/ui/spend">Spend</a>
<a href="/ui/fairshare">Fair share</a><a href="/ui/audit">Audit</a>
{{if .User}}<span class="who">{{.User}}
<form class="inline" method="post" action="/ui/logout"><input type="hidden" name="csrf" value="{{.CSRF}}"><button>Sign out</button></form></span>{{end}}
</header><main><h1>{{.Title}}</h1>
{{if .Message}}<p class="msg">{{.Message}}</p>{{end}}{{if .Error}}<p class="err">{{.Error}}</p>{{end}}{{end}}

{{define "foot"}}</main></body></html>{{end}}

{{define "table"}}<table><tr>{{range .Cols}}<th>{{.}}</th>{{end}}</tr>
{{range .Rows}}<tr>{{range .}}<td>{{.}}</td>{{end}}</tr>{{end}}</table>{{end}}

{{define "home"}}{{template "head" .}}
<p>Signed in as <strong>{{.User}}</strong>. Roles: <code>{{.Data}}</code>.</p>
<p>What you see and may change is what the admin API allows your roles: an
organisation administrator sees their organisation only.</p>
{{template "foot" .}}{{end}}

{{define "keys"}}{{template "head" .}}
<form method="get" action="/ui/keys"><label>Team <select name="team"><option value="">all visible</option>
{{range .Data.Teams}}<option>{{.}}</option>{{end}}</select></label> <button>Show</button></form>
<table><tr>{{range .Data.Keys.Cols}}<th>{{.}}</th>{{end}}<th>actions</th></tr>
{{$csrf := .CSRF}}{{$ids := .Data.Keys.IDs}}
{{range $i, $row := .Data.Keys.Rows}}<tr>{{range $row}}<td>{{.}}</td>{{end}}<td>
{{with index $ids $i}}
<form class="inline" method="post" action="/ui/keys/{{.}}/rotate"><input type="hidden" name="csrf" value="{{$csrf}}">
<input name="grace_period" placeholder="grace 24h" size="8"><button>Rotate</button></form>
<form class="inline" method="post" action="/ui/keys/{{.}}/revoke"><input type="hidden" name="csrf" value="{{$csrf}}"><button>Revoke</button></form>
{{end}}</td></tr>{{end}}</table>
<fieldset><legend>New key</legend>
<form method="post" action="/ui/keys"><input type="hidden" name="csrf" value="{{.CSRF}}">
<label>Team <select name="team" required>{{range .Data.Teams}}<option>{{.}}</option>{{end}}</select></label>
<label>Project <input name="project" size="10"></label> <label>Name <input name="name" size="14"></label>
<label>RPM <input name="rpm" type="number" min="0" size="5"></label> <label>TPM <input name="tpm" type="number" min="0" size="7"></label>
<label>Max in flight <input name="max_inflight" type="number" min="0" size="3"></label>
<button>Create</button></form><p>The secret is shown once, on the next page.</p></fieldset>
{{template "foot" .}}{{end}}

{{define "secret"}}{{template "head" .}}
<p>Key <code>{{.Data.ID}}</code>. Copy the secret now: it is not stored in clear and will not be shown again.</p>
<p class="secret">{{.Data.Key}}</p><p><a href="/ui/keys">Back to keys</a></p>
{{template "foot" .}}{{end}}

{{define "budgets"}}{{template "head" .}}
<table><tr>{{range .Data.Cols}}<th>{{.}}</th>{{end}}<th></th></tr>{{$csrf := .CSRF}}
{{range .Data.Rows}}<tr>{{range .}}<td>{{.}}</td>{{end}}<td>
<form class="inline" method="post" action="/ui/budgets/delete"><input type="hidden" name="csrf" value="{{$csrf}}">
<input type="hidden" name="kind" value="{{index . 0}}"><input type="hidden" name="name" value="{{index . 1}}">
<input type="hidden" name="model" value="{{index . 2}}"><button>Delete</button></form></td></tr>{{end}}</table>
<fieldset><legend>Set a budget (GPU-hours)</legend>
<form method="post" action="/ui/budgets"><input type="hidden" name="csrf" value="{{.CSRF}}">
<label>Scope <select name="kind"><option>org</option><option>team</option><option>project</option><option>key</option><option>tag</option></select></label>
<label>Name <input name="name" required size="16"></label> <label>Model <input name="model" size="10" placeholder="all"></label>
<label>Period <select name="period"><option>day</option><option>week</option><option selected>month</option></select></label>
<label>Max <input name="max_cost" required size="6"></label> <label>Soft <input name="soft_limit" size="6"></label>
<button>Save</button></form></fieldset>
{{template "foot" .}}{{end}}

{{define "spend"}}{{template "head" .}}
<form method="get" action="/ui/spend"><label>From <input name="from" value="{{.Data.From}}" size="10"></label>
<label>To <input name="to" value="{{.Data.To}}" size="10"></label>
<label>Group by <select name="group_by">{{$g := .Data.Group}}{{range .Data.Groups}}<option{{if eq . $g}} selected{{end}}>{{.}}</option>{{end}}</select></label>
<button>Show</button></form>
{{template "table" .Data.Table}}
{{template "foot" .}}{{end}}

{{define "fairshare"}}{{template "head" .}}
<p>Live state of each pool: reservation, requests in flight, borrowing, level of fair share (LF) and rank. Reload for a new reading.</p>
{{range .Data}}<h2>{{.Name}}</h2>{{template "table" .Table}}{{end}}
{{template "foot" .}}{{end}}

{{define "audit"}}{{template "head" .}}
<form method="post" action="/ui/audit/verify"><input type="hidden" name="csrf" value="{{.CSRF}}"><button>Verify the chain</button></form>
{{with .Data.Status}}<p>{{if .OK}}<span class="good">Chain verified</span>{{else}}<span class="bad">Verification failed</span>{{end}}:
entries {{.First}}–{{.Last}} ({{.Entries}}), {{.Checkpoints}} signed checkpoints, {{.Unsigned}} entries after the newest checkpoint{{if .Anchored}}, checked against the anchor at seq {{.AnchorSeq}}{{else}}, no anchor export configured{{end}}.
{{if not .Signed}}No signing key: checkpoints are not checked.{{end}}
Checked with the gateway's own key; for an independent check run <code>llmflow-gateway audit verify</code> with a public key kept elsewhere.</p>
{{if .Problems}}<ul>{{range .Problems}}<li class="bad">{{.}}</li>{{end}}</ul>{{end}}{{end}}
{{template "table" .Data.Table}}
{{if .Data.Next}}<p><a href="/ui/audit?from={{.Data.Next}}">Next entries</a></p>{{end}}
{{template "foot" .}}{{end}}

{{define "loggedout"}}{{template "head" .}}<p>You are signed out. <a href="/admin/login?next=ui">Sign in again</a>.</p>{{template "foot" .}}{{end}}
`
