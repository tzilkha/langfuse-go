package langfuse

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/tzilkha/langfuse-go/model"
)

const (
	defaultFlushInterval = 500 * time.Millisecond
)

const (
	defaultLangfuseHost = "https://cloud.langfuse.com"
	otelTracesPath      = "/api/public/otel/v1/traces"
	scoresPath          = "/api/public/scores"
	tracerName          = "github.com/tzilkha/langfuse-go"
)

// spanEntry is an OTel span we are tracking so future child observations
// can resolve it as a parent by Langfuse-style string ID.
type spanEntry struct {
	span oteltrace.Span
	ctx  context.Context
}

type Langfuse struct {
	flushInterval time.Duration

	host      string
	publicKey string
	secretKey string

	tp     *sdktrace.TracerProvider
	tracer oteltrace.Tracer

	httpClient *http.Client

	mu    sync.Mutex
	spans map[string]*spanEntry // observationID (OTel SpanID hex) -> entry
	roots map[string]*spanEntry // traceID (OTel TraceID hex)     -> root entry
}

func New(ctx context.Context) *Langfuse {
	host := os.Getenv("LANGFUSE_HOST")
	if host == "" {
		host = defaultLangfuseHost
	}

	l := &Langfuse{
		flushInterval: defaultFlushInterval,
		host:          host,
		publicKey:     os.Getenv("LANGFUSE_PUBLIC_KEY"),
		secretKey:     os.Getenv("LANGFUSE_SECRET_KEY"),
		httpClient:    http.DefaultClient,
		spans:         make(map[string]*spanEntry),
		roots:         make(map[string]*spanEntry),
	}

	if err := l.initTracer(ctx); err != nil {
		fmt.Println(err)
	}

	return l
}

func (l *Langfuse) initTracer(ctx context.Context) error {
	u, err := url.Parse(l.host)
	if err != nil {
		return fmt.Errorf("invalid LANGFUSE_HOST %q: %w", l.host, err)
	}

	endpoint := u.Host
	if endpoint == "" {
		endpoint = u.Path
	}

	opts := []otlptracehttp.Option{
		otlptracehttp.WithEndpoint(endpoint),
		otlptracehttp.WithURLPath(otelTracesPath),
		otlptracehttp.WithHeaders(map[string]string{
			"Authorization": basicAuth(l.publicKey, l.secretKey),
		}),
	}
	if strings.EqualFold(u.Scheme, "http") {
		opts = append(opts, otlptracehttp.WithInsecure())
	}

	exporter, err := otlptracehttp.New(ctx, opts...)
	if err != nil {
		return fmt.Errorf("creating OTLP exporter: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(
			exporter,
			sdktrace.WithBatchTimeout(l.flushInterval),
		),
	)

	l.tp = tp
	l.tracer = tp.Tracer(tracerName)
	return nil
}

func (l *Langfuse) WithFlushInterval(d time.Duration) *Langfuse {
	l.flushInterval = d
	if l.tp != nil {
		_ = l.tp.Shutdown(context.Background())
		_ = l.initTracer(context.Background())
	}
	return l
}

// Trace creates a root span representing the trace and immediately ends it.
// Subsequent observations created without a parentID but with this trace's
// ID will be linked as children via OTel context.
func (l *Langfuse) Trace(t *model.Trace) (*model.Trace, error) {
	name := t.Name
	if name == "" {
		name = "trace"
	}

	startTime := time.Now()
	if t.Timestamp != nil {
		startTime = *t.Timestamp
	}

	ctx, span := l.tracer.Start(
		context.Background(),
		name,
		oteltrace.WithTimestamp(startTime),
	)

	setTraceAttributes(span, t)

	traceID := span.SpanContext().TraceID().String()
	t.ID = traceID

	entry := &spanEntry{span: span, ctx: ctx}

	l.mu.Lock()
	l.roots[traceID] = entry
	l.mu.Unlock()

	// Root span is closed immediately; children created from `ctx` will still
	// inherit its SpanContext as their parent.
	span.End(oteltrace.WithTimestamp(startTime))

	return t, nil
}

func (l *Langfuse) Generation(g *model.Generation, parentID *string) (*model.Generation, error) {
	g.Type = model.ObservationTypeGeneration

	parentCtx, traceID, err := l.resolveParent(g.TraceID, parentID, g.Name)
	if err != nil {
		return nil, err
	}
	g.TraceID = traceID

	startTime := time.Now()
	if g.StartTime != nil {
		startTime = *g.StartTime
	}

	ctx, span := l.tracer.Start(
		parentCtx,
		g.Name,
		oteltrace.WithTimestamp(startTime),
	)

	setObservationAttributes(span, model.ObservationTypeGeneration, g.Level, g.StatusMessage, g.Version, g.Input, g.Output, g.Metadata)
	setGenerationAttributes(span, g)

	g.ID = span.SpanContext().SpanID().String()
	if parentID != nil && *parentID != "" {
		g.ParentObservationID = *parentID
	}

	l.mu.Lock()
	l.spans[g.ID] = &spanEntry{span: span, ctx: ctx}
	l.mu.Unlock()

	return g, nil
}

func (l *Langfuse) GenerationEnd(g *model.Generation) (*model.Generation, error) {
	if g.ID == "" {
		return nil, fmt.Errorf("generation ID is required")
	}

	entry, ok := l.takeSpan(g.ID)
	if !ok {
		return nil, fmt.Errorf("generation %q not found", g.ID)
	}

	setObservationAttributes(entry.span, model.ObservationTypeGeneration, g.Level, g.StatusMessage, g.Version, g.Input, g.Output, g.Metadata)
	setGenerationAttributes(entry.span, g)

	endTime := time.Now()
	if g.EndTime != nil {
		endTime = *g.EndTime
	}
	entry.span.End(oteltrace.WithTimestamp(endTime))

	return g, nil
}

func (l *Langfuse) Span(s *model.Span, parentID *string) (*model.Span, error) {
	s.Type = model.ObservationTypeSpan

	parentCtx, traceID, err := l.resolveParent(s.TraceID, parentID, s.Name)
	if err != nil {
		return nil, err
	}
	s.TraceID = traceID

	startTime := time.Now()
	if s.StartTime != nil {
		startTime = *s.StartTime
	}

	ctx, span := l.tracer.Start(parentCtx, s.Name, oteltrace.WithTimestamp(startTime))
	setObservationAttributes(span, model.ObservationTypeSpan, s.Level, s.StatusMessage, s.Version, s.Input, s.Output, s.Metadata)

	s.ID = span.SpanContext().SpanID().String()
	if parentID != nil && *parentID != "" {
		s.ParentObservationID = *parentID
	}

	l.mu.Lock()
	l.spans[s.ID] = &spanEntry{span: span, ctx: ctx}
	l.mu.Unlock()

	return s, nil
}

func (l *Langfuse) SpanEnd(s *model.Span) (*model.Span, error) {
	if s.ID == "" {
		return nil, fmt.Errorf("span ID is required")
	}
	entry, ok := l.takeSpan(s.ID)
	if !ok {
		return nil, fmt.Errorf("span %q not found", s.ID)
	}
	setObservationAttributes(entry.span, model.ObservationTypeSpan, s.Level, s.StatusMessage, s.Version, s.Input, s.Output, s.Metadata)
	endTime := time.Now()
	if s.EndTime != nil {
		endTime = *s.EndTime
	}
	entry.span.End(oteltrace.WithTimestamp(endTime))
	return s, nil
}

func (l *Langfuse) Tool(t *model.Tool, parentID *string) (*model.Tool, error) {
	t.Type = model.ObservationTypeTool

	parentCtx, traceID, err := l.resolveParent(t.TraceID, parentID, t.Name)
	if err != nil {
		return nil, err
	}
	t.TraceID = traceID

	startTime := time.Now()
	if t.StartTime != nil {
		startTime = *t.StartTime
	}

	ctx, span := l.tracer.Start(parentCtx, t.Name, oteltrace.WithTimestamp(startTime))
	setObservationAttributes(span, model.ObservationTypeTool, t.Level, t.StatusMessage, t.Version, t.Input, t.Output, t.Metadata)

	t.ID = span.SpanContext().SpanID().String()
	if parentID != nil && *parentID != "" {
		t.ParentObservationID = *parentID
	}

	l.mu.Lock()
	l.spans[t.ID] = &spanEntry{span: span, ctx: ctx}
	l.mu.Unlock()

	return t, nil
}

func (l *Langfuse) ToolEnd(t *model.Tool) (*model.Tool, error) {
	if t.ID == "" {
		return nil, fmt.Errorf("tool ID is required")
	}
	entry, ok := l.takeSpan(t.ID)
	if !ok {
		return nil, fmt.Errorf("tool %q not found", t.ID)
	}
	setObservationAttributes(entry.span, model.ObservationTypeTool, t.Level, t.StatusMessage, t.Version, t.Input, t.Output, t.Metadata)
	endTime := time.Now()
	if t.EndTime != nil {
		endTime = *t.EndTime
	}
	entry.span.End(oteltrace.WithTimestamp(endTime))
	return t, nil
}

func (l *Langfuse) Event(e *model.Event, parentID *string) (*model.Event, error) {
	e.Type = model.ObservationTypeEvent

	parentCtx, traceID, err := l.resolveParent(e.TraceID, parentID, e.Name)
	if err != nil {
		return nil, err
	}
	e.TraceID = traceID

	startTime := time.Now()
	if e.StartTime != nil {
		startTime = *e.StartTime
	}

	_, span := l.tracer.Start(
		parentCtx,
		e.Name,
		oteltrace.WithTimestamp(startTime),
	)

	setObservationAttributes(span, model.ObservationTypeEvent, e.Level, e.StatusMessage, e.Version, e.Input, e.Output, e.Metadata)
	e.ID = span.SpanContext().SpanID().String()
	if parentID != nil && *parentID != "" {
		e.ParentObservationID = *parentID
	}
	span.End(oteltrace.WithTimestamp(startTime))

	return e, nil
}

// resolveParent figures out the OTel context to start a new span from, given
// the user-supplied (possibly empty) trace ID and parent observation ID. If
// neither is known, a fresh trace is created lazily.
func (l *Langfuse) resolveParent(traceID string, parentID *string, fallbackName string) (context.Context, string, error) {
	l.mu.Lock()
	if parentID != nil && *parentID != "" {
		if e, ok := l.spans[*parentID]; ok {
			tid := e.span.SpanContext().TraceID().String()
			ctx := e.ctx
			l.mu.Unlock()
			return ctx, tid, nil
		}
	}
	if traceID != "" {
		if e, ok := l.roots[traceID]; ok {
			ctx := e.ctx
			l.mu.Unlock()
			return ctx, traceID, nil
		}
	}
	l.mu.Unlock()

	// Need a new trace.
	t := &model.Trace{Name: fallbackName}
	if _, err := l.Trace(t); err != nil {
		return nil, "", err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.roots[t.ID]
	if !ok {
		return nil, "", fmt.Errorf("failed to create trace")
	}
	return e.ctx, t.ID, nil
}

func (l *Langfuse) takeSpan(id string) (*spanEntry, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.spans[id]
	if !ok {
		return nil, false
	}
	delete(l.spans, id)
	return e, true
}

// Score posts to the Langfuse REST scores endpoint. OTel has no equivalent
// concept, so this stays on the legacy HTTP API.
func (l *Langfuse) Score(s *model.Score) (*model.Score, error) {
	if s.TraceID == "" {
		return nil, fmt.Errorf("trace ID is required")
	}
	if s.ID == "" {
		s.ID = uuid.New().String()
	}

	body, err := json.Marshal(s)
	if err != nil {
		return nil, fmt.Errorf("marshalling score: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, strings.TrimRight(l.host, "/")+scoresPath, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", basicAuth(l.publicKey, l.secretKey))

	resp, err := l.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("score request failed: %s", resp.Status)
	}
	return s, nil
}

// Flush forces any pending spans to be exported and waits for completion or
// the context's deadline.
func (l *Langfuse) Flush(ctx context.Context) {
	if l.tp == nil {
		return
	}
	// End any still-open root spans so they get exported.
	l.mu.Lock()
	for _, e := range l.roots {
		// Already ended in Trace(); calling End again is a no-op for OTel
		// SDK spans, but kept here for safety in case behavior changes.
		_ = e
	}
	l.mu.Unlock()

	_ = l.tp.ForceFlush(ctx)
}

// Shutdown flushes pending spans and tears down the tracer provider.
func (l *Langfuse) Shutdown(ctx context.Context) error {
	if l.tp == nil {
		return nil
	}
	return l.tp.Shutdown(ctx)
}

// --- attribute helpers ---------------------------------------------------

func setTraceAttributes(span oteltrace.Span, t *model.Trace) {
	if t.Name != "" {
		span.SetAttributes(attribute.String("langfuse.trace.name", t.Name))
	}
	if t.UserID != "" {
		span.SetAttributes(attribute.String("langfuse.trace.user_id", t.UserID))
	}
	if t.SessionID != "" {
		span.SetAttributes(attribute.String("langfuse.trace.session_id", t.SessionID))
	}
	if len(t.Tags) > 0 {
		span.SetAttributes(attribute.StringSlice("langfuse.trace.tags", t.Tags))
	}
	if t.Public {
		span.SetAttributes(attribute.Bool("langfuse.trace.public", true))
	}
	if t.Release != "" {
		span.SetAttributes(attribute.String("langfuse.release", t.Release))
	}
	if t.Version != "" {
		span.SetAttributes(attribute.String("langfuse.version", t.Version))
	}
	if v, ok := jsonAttr(t.Input); ok {
		span.SetAttributes(attribute.String("langfuse.trace.input", v))
	}
	if v, ok := jsonAttr(t.Output); ok {
		span.SetAttributes(attribute.String("langfuse.trace.output", v))
	}
	if v, ok := jsonAttr(t.Metadata); ok {
		span.SetAttributes(attribute.String("langfuse.trace.metadata", v))
	}
}

func setObservationAttributes(
	span oteltrace.Span,
	obsType model.ObservationType,
	level model.ObservationLevel,
	statusMessage string,
	version string,
	input any,
	output any,
	metadata any,
) {
	span.SetAttributes(attribute.String("langfuse.observation.type", string(obsType)))
	if level != "" {
		span.SetAttributes(attribute.String("langfuse.observation.level", string(level)))
	}
	if statusMessage != "" {
		span.SetAttributes(attribute.String("langfuse.observation.status_message", statusMessage))
	}
	if version != "" {
		span.SetAttributes(attribute.String("langfuse.version", version))
	}
	if v, ok := jsonAttr(input); ok {
		span.SetAttributes(attribute.String("langfuse.observation.input", v))
	}
	if v, ok := jsonAttr(output); ok {
		span.SetAttributes(attribute.String("langfuse.observation.output", v))
	}
	if v, ok := jsonAttr(metadata); ok {
		span.SetAttributes(attribute.String("langfuse.observation.metadata", v))
	}
}

func setGenerationAttributes(span oteltrace.Span, g *model.Generation) {
	if g.Model != "" {
		span.SetAttributes(attribute.String("langfuse.observation.model.name", g.Model))
	}
	if len(g.ModelParameters) > 0 {
		if v, ok := jsonAttr(g.ModelParameters); ok {
			span.SetAttributes(attribute.String("langfuse.observation.model.parameters", v))
		}
	}
	if g.CompletionStartTime != nil {
		span.SetAttributes(attribute.String(
			"langfuse.observation.completion_start_time",
			g.CompletionStartTime.UTC().Format(time.RFC3339Nano),
		))
	}

	usage := mergeUsage(g.Usage, g.UsageDetails)
	if len(usage) > 0 {
		if v, ok := jsonAttr(usage); ok {
			span.SetAttributes(attribute.String("langfuse.observation.usage_details", v))
		}
	}
	if len(g.CostDetails) > 0 {
		if v, ok := jsonAttr(g.CostDetails); ok {
			span.SetAttributes(attribute.String("langfuse.observation.cost_details", v))
		}
	}
	if g.PromptName != "" {
		span.SetAttributes(attribute.String("langfuse.observation.prompt.name", g.PromptName))
	}
	if g.PromptVersion != 0 {
		span.SetAttributes(attribute.Int("langfuse.observation.prompt.version", g.PromptVersion))
	}
}

func mergeUsage(u model.Usage, details map[string]int) map[string]int {
	out := map[string]int{}
	for k, v := range details {
		out[k] = v
	}
	if u.Input != 0 {
		out["input"] = u.Input
	}
	if u.Output != 0 {
		out["output"] = u.Output
	}
	if u.Total != 0 {
		out["total"] = u.Total
	}
	if u.PromptTokens != 0 {
		out["promptTokens"] = u.PromptTokens
	}
	if u.CompletionTokens != 0 {
		out["completionTokens"] = u.CompletionTokens
	}
	if u.TotalTokens != 0 {
		out["totalTokens"] = u.TotalTokens
	}
	return out
}

func jsonAttr(v any) (string, bool) {
	if v == nil {
		return "", false
	}
	if s, ok := v.(string); ok {
		if s == "" {
			return "", false
		}
		return s, true
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "", false
	}
	if len(b) == 0 || string(b) == "null" {
		return "", false
	}
	return string(b), true
}

func basicAuth(publicKey, secretKey string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(publicKey+":"+secretKey))
}
