# gateway

An OpenAI-compatible gateway in pure Go (`CGO_ENABLED=0`) for vLLM clusters
shared by several institutions: virtual keys, budgets, GPU-hour accounting
and **fair-share scheduling with reservation and borrowing**.

Status: under construction towards v0.1.0.

```sh
go install github.com/go-llmflow/gateway/cmd/llmflow-gateway@latest
llmflow-gateway check-config --config gateway.yaml
```

License: BSD-3-Clause.
