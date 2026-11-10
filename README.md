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

- All create methods take `(model, parentID *string)` and return `(model, error)`. The returned struct has `ID` and `TraceID` populated — pass `&obj.ID` as the `parentID` for nested observations, or `nil` to make it a top-level observation of the trace.
- `ID` and `TraceID` are OpenTelemetry span/trace IDs (hex). User-supplied IDs are not honored — OTel generates them.
- `Flush(ctx)` forces an export of pending spans; `Shutdown(ctx)` flushes and tears down the tracer provider. Always call one of them before your program exits or batched spans may be lost.
- Parent linking goes through `parentID *string`; the SDK resolves it to the right OTel context internally. You can also pass a `TraceID` returned from a previous call without a `parentID` to add a top-level observation to that trace.

### How traces are exported

`Trace()` reserves a trace ID and exports nothing by itself. Every observation you create for that trace without a parent becomes a top-level observation of the trace. It is a real OTel root span with the trace's ID, and it carries the trace-level attributes (name, user, session, tags, release, version, input, output, metadata, public). Langfuse [reads those attributes from any span of the trace](https://langfuse.com/integrations/native/opentelemetry#trace-level-attributes). Children with an explicit `parentID` work as usual.

> **Changed in this version.** `Trace()` used to export a root span named after the trace, end it immediately and parent every observation to it. Langfuse stored that span as an observation of its own, with a 0.00s duration and no input or output. That added one [billable unit](https://langfuse.com/docs/administration/billable-units) to every trace and pushed your real observations one level deeper. `Trace.Timestamp` set that span's start time; it is now ignored, so set `StartTime` on your observations instead. If you query the root observation by the trace's name, query your own top-level observation instead.

```go
trace, _ := l.Trace(&model.Trace{Name: "chat", UserID: "u1", Tags: []string{"prod"}})

span, _ := l.Span(&model.Span{TraceID: trace.ID, Name: "handle-request"}, nil) // top-level observation
gen, _ := l.Generation(&model.Generation{TraceID: trace.ID, Name: "llm"}, &span.ID)
_, _ = l.GenerationEnd(gen)
_, _ = l.SpanEnd(span)

// Free the SDK's state for the trace once you won't create more observations for it.
_ = l.TraceEnd(trace.ID)
```

- The trace attributes go on **every** top-level observation of the trace, not only the first. That covers fields Langfuse reads only from the root span (such as `version`), and it doesn't matter which root Langfuse ends up picking. Child observations don't carry them.
- An observation created with neither a known `TraceID` nor an open `parentID` starts a new trace named after the observation, and becomes its root. Its returned `TraceID` works like one from `Trace()`, including `TraceEnd`.
- On a top-level observation, the trace version and the observation version share the `langfuse.version` attribute. If the observation has its own `Version`, it replaces the trace version on that span.
- Each top-level observation holds at most 128 attributes (OpenTelemetry's default, or `OTEL_SPAN_ATTRIBUTE_COUNT_LIMIT`), and the OpenTelemetry SDK silently drops attributes past that. So the trace attributes on a top-level observation are capped at the limit minus 14, which keeps room for the observation's own fields. The fixed fields come first and metadata keys after them, in sorted order: at the default limit, metadata beyond about 105 keys is dropped and a line is logged.
- Trace fields are fixed when `Trace()` is called and can't be updated later. If a trace's final output is only known at the end, set it as the output of the top-level observation: Langfuse uses the root observation's output when the trace has none.
- The SDK keeps the trace's state until you call `TraceEnd(id)`. Call it once you won't create more observations for the trace, or a long-running process accumulates state. Observations that are still open can be ended as usual. Observations created for that ID after `TraceEnd` are treated like any unknown trace ID and start a new trace.
- An observation whose `parentID` has already ended becomes another top-level observation of the same trace. It doesn't start a new trace.
- If you never create an observation for a trace, nothing reaches Langfuse.

## Who uses langfuse-go?

* [LinGoose](https://github.com/henomis/lingoose) Go framework for building awesome LLM apps
