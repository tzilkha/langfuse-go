# Langfuse Go SDK

[![GoDoc](https://godoc.org/github.com/tzilkha/langfuse-go?status.svg)](https://godoc.org/github.com/tzilkha/langfuse-go) [![Go Report Card](https://goreportcard.com/badge/github.com/tzilkha/langfuse-go)](https://goreportcard.com/report/github.com/tzilkha/langfuse-go) [![GitHub release](https://img.shields.io/github/release/tzilkha/langfuse-go.svg)](https://github.com/tzilkha/langfuse-go/releases)

This is [Langfuse](https://langfuse.com)'s **unofficial** Go client. It ships traces to Langfuse over the OpenTelemetry traces endpoint (`/api/public/otel/v1/traces`); scores still use the REST API.

Originally forked from [henomis/langfuse-go](https://github.com/henomis/langfuse-go). The transport has been rewritten on top of the OTel Go SDK and the observation surface extended with the newer Langfuse observation types (`tool`, `agent`, `guardrail`).

## Langfuse

[Langfuse](https://langfuse.com) traces, evals, prompt management and metrics to debug and improve your LLM application.

## Observation support

| **Operation** | **Status** |
| --- | --- |
| Trace        | 🟢 |
| Generation   | 🟢 |
| Span         | 🟢 |
| Event        | 🟢 |
| Tool         | 🟢 |
| Agent        | 🟢 |
| Guardrail    | 🟢 |
| Score        | 🟢 |

Each observation kind sets `langfuse.observation.type` on the underlying OTel span. The other Langfuse observation types (`chain`, `retriever`, `evaluator`, `embedding`) are exposed as `model.ObservationType*` constants but don't yet have dedicated wrapper methods.

## Getting started

### Installation

```
go get github.com/tzilkha/langfuse-go
```

### Configuration

The client is configured from these environment variables:

- `LANGFUSE_HOST` — host of the Langfuse service (defaults to `https://cloud.langfuse.com`).
- `LANGFUSE_PUBLIC_KEY` — your project's public key.
- `LANGFUSE_SECRET_KEY` — your project's secret key.

### Usage

See [`examples/cmd/`](examples/cmd/) for runnable examples.

```go
package main

import (
	"context"

	langfuse "github.com/tzilkha/langfuse-go"
	"github.com/tzilkha/langfuse-go/model"
)

func main() {
	ctx := context.Background()
	l := langfuse.New(ctx)
	defer l.Shutdown(ctx)

	trace, err := l.Trace(&model.Trace{Name: "test-trace"})
	if err != nil {
		panic(err)
	}

	span, err := l.Span(&model.Span{
		TraceID: trace.ID,
		Name:    "test-span",
	}, nil)
	if err != nil {
		panic(err)
	}

	gen, err := l.Generation(&model.Generation{
		TraceID: trace.ID,
		Name:    "test-generation",
		Model:   "gpt-4o-mini",
		ModelParameters: map[string]any{
			"max_tokens":  1000,
			"temperature": 0.9,
		},
		Input: []model.M{
			{"role": "system", "content": "You are a helpful assistant."},
			{"role": "user", "content": "Summarize the OKRs..."},
		},
	}, &span.ID)
	if err != nil {
		panic(err)
	}

	tool, err := l.Tool(&model.Tool{
		TraceID: trace.ID,
		Name:    "lookup_customer",
		Input:   model.M{"customer_id": "123"},
	}, &gen.ID)
	if err != nil {
		panic(err)
	}
	tool.Output = model.M{"name": "Acme"}
	if _, err := l.ToolEnd(tool); err != nil {
		panic(err)
	}

	gen.Output = model.M{"completion": "The Q3 OKRs ..."}
	gen.Usage = model.Usage{PromptTokens: 42, CompletionTokens: 17, TotalTokens: 59}
	if _, err := l.GenerationEnd(gen); err != nil {
		panic(err)
	}

	if _, err := l.Event(&model.Event{
		TraceID:  trace.ID,
		Name:     "test-event",
		Metadata: model.M{"key": "value"},
	}, &span.ID); err != nil {
		panic(err)
	}

	if _, err := l.Score(&model.Score{
		TraceID: trace.ID,
		Name:    "test-score",
		Value:   0.9,
	}); err != nil {
		panic(err)
	}

	if _, err := l.SpanEnd(span); err != nil {
		panic(err)
	}

	l.Flush(ctx)
}
```

#### Notes

- All create methods take `(model, parentID *string)` and return `(model, error)`. The returned struct has `ID` and `TraceID` populated — pass `&obj.ID` as the `parentID` for nested observations, or `nil` to attach directly to the trace's root.
- `ID` and `TraceID` are OpenTelemetry span/trace IDs (hex). User-supplied IDs are not honored — OTel generates them.
- `Flush(ctx)` forces an export of pending spans; `Shutdown(ctx)` flushes and tears down the tracer provider. Always call one of them before your program exits or batched spans may be lost.
- Parent linking goes through `parentID *string`; the SDK resolves it to the right OTel context internally. You can also pass a `TraceID` returned from a previous call without a `parentID` to attach to that trace's root.

## Who uses langfuse-go?

* [LinGoose](https://github.com/henomis/lingoose) Go framework for building awesome LLM apps
