package langfuse

import (
	"bytes"
	"context"
	crand "crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"sort"
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

	mu     sync.Mutex
	spans  map[string]*spanEntry           // observationID (OTel SpanID hex) -> entry
	traces map[string][]attribute.KeyValue // traceID (OTel TraceID hex) -> trace attributes

	attrLimit int // per-span attribute limit, negative if unlimited
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
		traces:        make(map[string][]attribute.KeyValue),
	}

	if err := l.initTracer(ctx); err != nil {
		fmt.Println(err)
	}

	return l
}

func (l *Langfuse) initTracer(ctx context.Context) error {
	limits := sdktrace.NewSpanLimits() // the SDK defaults, kept to know the attribute limit
	l.attrLimit = limits.AttributeCountLimit

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
			"Authorization":                basicAuth(l.publicKey, l.secretKey),
			"x-langfuse-ingestion-version": "4",
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
		sdktrace.WithIDGenerator(idGenerator{}),
		sdktrace.WithRawSpanLimits(limits),
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

// Trace reserves a trace ID without exporting anything. Observations created
// for it without a parent become its top-level observations and carry the
// trace attributes; the first one is the trace's root.
func (l *Langfuse) Trace(t *model.Trace) (*model.Trace, error) {
	t.ID = randomTraceID().String()

	// Leave room on each top-level span for the observation's own attributes:
	// the OTel SDK silently drops new attributes past its limit.
	attrs := traceAttributes(t)
	if keep := max(l.attrLimit-observationAttrs, 0); l.attrLimit >= 0 && len(attrs) > keep {
		log.Printf("langfuse: trace %s: %d trace attributes dropped (span attribute limit %d)",
			t.ID, len(attrs)-keep, l.attrLimit)
		attrs = append([]attribute.KeyValue(nil), attrs[:keep]...) // copy: don't retain the dropped tail
	}

	l.mu.Lock()
	l.traces[t.ID] = attrs
	l.mu.Unlock()

	return t, nil
}

// TraceEnd frees the client's state for a trace. Call it once no more
// observations will be created for the trace.
func (l *Langfuse) TraceEnd(traceID string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.traces[traceID]; !ok {
		return fmt.Errorf("trace %q not found", traceID)
	}
	delete(l.traces, traceID)
	return nil
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

	ctx, span := l.startObservationSpan(
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

	ctx, span := l.startObservationSpan(parentCtx, s.Name, oteltrace.WithTimestamp(startTime))
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

	ctx, span := l.startObservationSpan(parentCtx, t.Name, oteltrace.WithTimestamp(startTime))
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

func (l *Langfuse) Agent(a *model.Agent, parentID *string) (*model.Agent, error) {
	a.Type = model.ObservationTypeAgent

	parentCtx, traceID, err := l.resolveParent(a.TraceID, parentID, a.Name)
	if err != nil {
		return nil, err
	}
	a.TraceID = traceID

	startTime := time.Now()
	if a.StartTime != nil {
		startTime = *a.StartTime
	}

	ctx, span := l.startObservationSpan(parentCtx, a.Name, oteltrace.WithTimestamp(startTime))
	setObservationAttributes(span, model.ObservationTypeAgent, a.Level, a.StatusMessage, a.Version, a.Input, a.Output, a.Metadata)

	a.ID = span.SpanContext().SpanID().String()
	if parentID != nil && *parentID != "" {
		a.ParentObservationID = *parentID
	}

	l.mu.Lock()
	l.spans[a.ID] = &spanEntry{span: span, ctx: ctx}
	l.mu.Unlock()

	return a, nil
}

func (l *Langfuse) AgentEnd(a *model.Agent) (*model.Agent, error) {
	if a.ID == "" {
		return nil, fmt.Errorf("agent ID is required")
	}
	entry, ok := l.takeSpan(a.ID)
	if !ok {
		return nil, fmt.Errorf("agent %q not found", a.ID)
	}
	setObservationAttributes(entry.span, model.ObservationTypeAgent, a.Level, a.StatusMessage, a.Version, a.Input, a.Output, a.Metadata)
	endTime := time.Now()
	if a.EndTime != nil {
		endTime = *a.EndTime
	}
	entry.span.End(oteltrace.WithTimestamp(endTime))
	return a, nil
}

func (l *Langfuse) Guardrail(g *model.Guardrail, parentID *string) (*model.Guardrail, error) {
	g.Type = model.ObservationTypeGuardrail

	parentCtx, traceID, err := l.resolveParent(g.TraceID, parentID, g.Name)
	if err != nil {
		return nil, err
	}
	g.TraceID = traceID

	startTime := time.Now()
	if g.StartTime != nil {
		startTime = *g.StartTime
	}

	ctx, span := l.startObservationSpan(parentCtx, g.Name, oteltrace.WithTimestamp(startTime))
	setObservationAttributes(span, model.ObservationTypeGuardrail, g.Level, g.StatusMessage, g.Version, g.Input, g.Output, g.Metadata)

	g.ID = span.SpanContext().SpanID().String()
	if parentID != nil && *parentID != "" {
		g.ParentObservationID = *parentID
	}

	l.mu.Lock()
	l.spans[g.ID] = &spanEntry{span: span, ctx: ctx}
	l.mu.Unlock()

	return g, nil
}

func (l *Langfuse) GuardrailEnd(g *model.Guardrail) (*model.Guardrail, error) {
	if g.ID == "" {
		return nil, fmt.Errorf("guardrail ID is required")
	}
	entry, ok := l.takeSpan(g.ID)
	if !ok {
		return nil, fmt.Errorf("guardrail %q not found", g.ID)
	}
	setObservationAttributes(entry.span, model.ObservationTypeGuardrail, g.Level, g.StatusMessage, g.Version, g.Input, g.Output, g.Metadata)
	endTime := time.Now()
	if g.EndTime != nil {
		endTime = *g.EndTime
	}
	entry.span.End(oteltrace.WithTimestamp(endTime))
	return g, nil
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

	_, span := l.startObservationSpan(
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
// the parent isn't open, the span becomes a top-level observation of the
// trace; if the trace isn't known either, a fresh trace is created lazily.
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
	_, known := l.traces[traceID]
	l.mu.Unlock()

	if !known {
		t := &model.Trace{Name: fallbackName}
		if _, err := l.Trace(t); err != nil {
			return nil, "", err
		}
		traceID = t.ID
	}
	tid, err := oteltrace.TraceIDFromHex(traceID)
	if err != nil {
		return nil, "", fmt.Errorf("invalid trace ID %q: %w", traceID, err)
	}
	return context.WithValue(context.Background(), traceIDKey{}, tid), traceID, nil
}

// startObservationSpan starts a span and, if it is a top-level observation,
// sets its trace's attributes before the caller sets the observation's own.
func (l *Langfuse) startObservationSpan(
	parentCtx context.Context, name string, opts ...oteltrace.SpanStartOption,
) (context.Context, oteltrace.Span) {
	ctx, span := l.tracer.Start(parentCtx, name, opts...)
	// A child's context inherits traceIDKey too; only parent-less spans qualify.
	tid, ok := parentCtx.Value(traceIDKey{}).(oteltrace.TraceID)
	if ok && !oteltrace.SpanContextFromContext(parentCtx).IsValid() {
		l.mu.Lock()
		attrs := l.traces[tid.String()]
		l.mu.Unlock()
		span.SetAttributes(attrs...)
	}
	return ctx, span
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

// observationAttrs is the most attributes an observation sets itself: 13
// langfuse.observation.* keys for a generation, plus langfuse.version.
const observationAttrs = 14

// traceAttributes returns the trace's attributes, most important first, so
// truncation drops metadata before the fixed fields.
func traceAttributes(t *model.Trace) []attribute.KeyValue {
	var kvs []attribute.KeyValue
	if t.Name != "" {
		kvs = append(kvs, attribute.String("langfuse.trace.name", t.Name))
	}
	if t.UserID != "" {
		kvs = append(kvs, attribute.String("langfuse.user.id", t.UserID)) // was langfuse.trace.user.id
	}
	if t.SessionID != "" {
		kvs = append(kvs, attribute.String("langfuse.session.id", t.SessionID)) // was langfuse.trace.session.id
	}
	if len(t.Tags) > 0 {
		kvs = append(kvs, attribute.StringSlice("langfuse.trace.tags", t.Tags))
	}
	if t.Public {
		kvs = append(kvs, attribute.Bool("langfuse.trace.public", true))
	}
	if t.Release != "" {
		kvs = append(kvs, attribute.String("langfuse.release", t.Release))
	}
	if t.Version != "" {
		kvs = append(kvs, attribute.String("langfuse.version", t.Version))
	}
	if v, ok := jsonAttr(t.Input); ok {
		kvs = append(kvs, attribute.String("langfuse.observation.input", v))
	}
	if v, ok := jsonAttr(t.Output); ok {
		kvs = append(kvs, attribute.String("langfuse.observation.output", v))
	}
	// metadata: one attribute per top-level key, prefixed key segment
	meta := asStringMap(t.Metadata)
	keys := make([]string, 0, len(meta))
	for k := range meta {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		kvs = append(kvs, attribute.String("langfuse.trace.metadata."+k, meta[k]))
	}
	return kvs
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

func asStringMap(v any) map[string]string {
	out := map[string]string{}
	if v == nil {
		return out
	}
	// normalize through JSON so any map/struct works
	b, err := json.Marshal(v)
	if err != nil {
		return out
	}
	var m map[string]any
	if json.Unmarshal(b, &m) != nil {
		return out
	}
	for k, val := range m {
		if s, ok := val.(string); ok {
			out[k] = s
		} else if vb, merr := json.Marshal(val); merr == nil {
			out[k] = string(vb) // nested values JSON-encoded
		}
	}
	return out
}

// idGenerator makes a parent-less span use the trace ID in its start context
// (see resolveParent). OTel otherwise gives every root span a random trace
// ID, which would put each top-level observation in a trace of its own.
type idGenerator struct{}

type traceIDKey struct{}

func (idGenerator) NewIDs(ctx context.Context) (oteltrace.TraceID, oteltrace.SpanID) {
	if tid, ok := ctx.Value(traceIDKey{}).(oteltrace.TraceID); ok {
		return tid, randomSpanID()
	}
	return randomTraceID(), randomSpanID()
}

func (idGenerator) NewSpanID(context.Context, oteltrace.TraceID) oteltrace.SpanID {
	return randomSpanID()
}

func randomTraceID() oteltrace.TraceID {
	var id oteltrace.TraceID
	for !id.IsValid() {
		_, _ = crand.Read(id[:])
	}
	return id
}

func randomSpanID() oteltrace.SpanID {
	var id oteltrace.SpanID
	for !id.IsValid() {
		_, _ = crand.Read(id[:])
	}
	return id
}
