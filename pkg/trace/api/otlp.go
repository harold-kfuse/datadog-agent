// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

package api

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	pb "github.com/DataDog/datadog-agent/pkg/proto/pbgo/trace"
	"github.com/DataDog/datadog-agent/pkg/trace/api/internal/header"
	"github.com/DataDog/datadog-agent/pkg/trace/config"
	"github.com/DataDog/datadog-agent/pkg/trace/info"
	"github.com/DataDog/datadog-agent/pkg/trace/log"
	"github.com/DataDog/datadog-agent/pkg/trace/sampler"
	"github.com/DataDog/datadog-agent/pkg/trace/timing"
	"github.com/DataDog/datadog-agent/pkg/trace/traceutil"
	"github.com/DataDog/datadog-go/v5/statsd"

	"github.com/DataDog/opentelemetry-mapping-go/pkg/otlp/attributes"
	"github.com/DataDog/opentelemetry-mapping-go/pkg/otlp/attributes/source"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.opentelemetry.io/collector/pdata/ptrace/ptraceotlp"
	semconv117 "go.opentelemetry.io/collector/semconv/v1.17.0"
	semconv "go.opentelemetry.io/collector/semconv/v1.6.1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// keyStatsComputed specifies the resource attribute key which indicates if stats have been
// computed for the resource spans.
const keyStatsComputed = "_dd.stats_computed"

var _ (ptraceotlp.GRPCServer) = (*OTLPReceiver)(nil)

// OTLPReceiver implements an OpenTelemetry Collector receiver which accepts incoming
// data on two ports for both plain HTTP and gRPC.
type OTLPReceiver struct {
	ptraceotlp.UnimplementedGRPCServer
	wg          sync.WaitGroup      // waits for a graceful shutdown
	grpcsrv     *grpc.Server        // the running GRPC server on a started receiver, if enabled
	out         chan<- *Payload     // the outgoing payload channel
	conf        *config.AgentConfig // receiver config
	cidProvider IDProvider          // container ID provider
	statsd      statsd.ClientInterface
	timing      timing.Reporter
}

// NewOTLPReceiver returns a new OTLPReceiver which sends any incoming traces down the out channel.
func NewOTLPReceiver(out chan<- *Payload, cfg *config.AgentConfig, statsd statsd.ClientInterface, timing timing.Reporter) *OTLPReceiver {
	computeTopLevelBySpanKindVal := 0.0
	if cfg.HasFeature("enable_otlp_compute_top_level_by_span_kind") {
		computeTopLevelBySpanKindVal = 1.0
	}
	_ = statsd.Gauge("datadog.trace_agent.otlp.compute_top_level_by_span_kind", computeTopLevelBySpanKindVal, nil, 1)
	return &OTLPReceiver{out: out, conf: cfg, cidProvider: NewIDProvider(cfg.ContainerProcRoot), statsd: statsd, timing: timing}
}

// Start starts the OTLPReceiver, if any of the servers were configured as active.
func (o *OTLPReceiver) Start() {
	cfg := o.conf.OTLPReceiver
	if cfg.GRPCPort != 0 {
		ln, err := net.Listen("tcp", fmt.Sprintf("%s:%d", cfg.BindHost, cfg.GRPCPort))
		if err != nil {
			log.Criticalf("Error starting OpenTelemetry gRPC server: %v", err)
		} else {
			o.grpcsrv = grpc.NewServer(
				grpc.MaxRecvMsgSize(10*1024*1024),
				grpc.MaxConcurrentStreams(1), // Each payload must be sent to processing stage before we decode the next.
			)
			ptraceotlp.RegisterGRPCServer(o.grpcsrv, o)
			o.wg.Add(1)
			go func() {
				defer o.wg.Done()
				if err := o.grpcsrv.Serve(ln); err != nil {
					log.Criticalf("Error starting OpenTelemetry gRPC server: %v", err)
				}
			}()
			log.Debugf("Listening to core Agent for OTLP traces on internal gRPC port (http://%s:%d, internal use only). Check core Agent logs for information on the OTLP ingest status.", cfg.BindHost, cfg.GRPCPort)
		}
	}
}

// Stop stops any running server.
func (o *OTLPReceiver) Stop() {
	if o.grpcsrv != nil {
		go o.grpcsrv.Stop()
	}
	o.wg.Wait()
}

// Export implements ptraceotlp.Server
func (o *OTLPReceiver) Export(ctx context.Context, in ptraceotlp.ExportRequest) (ptraceotlp.ExportResponse, error) {
	defer o.timing.Since("datadog.trace_agent.otlp.process_grpc_request_ms", time.Now())
	md, _ := metadata.FromIncomingContext(ctx)
	_ = o.statsd.Count("datadog.trace_agent.otlp.payload", 1, tagsFromHeaders(http.Header(md)), 1)
	o.processRequest(ctx, http.Header(md), in)
	return ptraceotlp.NewExportResponse(), nil
}

func tagsFromHeaders(h http.Header) []string {
	tags := []string{"endpoint_version:opentelemetry_grpc_v1"}
	if v := fastHeaderGet(h, header.Lang); v != "" {
		tags = append(tags, "lang:"+v)
	}
	if v := fastHeaderGet(h, header.LangVersion); v != "" {
		tags = append(tags, "lang_version:"+v)
	}
	if v := fastHeaderGet(h, header.LangInterpreter); v != "" {
		tags = append(tags, "interpreter:"+v)
	}
	if v := fastHeaderGet(h, header.LangInterpreterVendor); v != "" {
		tags = append(tags, "lang_vendor:"+v)
	}
	return tags
}

// fastHeaderGet returns the given key from the header, avoiding the caonical transformation of key
// that is normally applied by http.Header.Get.
func fastHeaderGet(h http.Header, canonicalKey string) string {
	if h == nil {
		return ""
	}
	v, ok := h[canonicalKey]
	if !ok || len(v) == 0 {
		return ""
	}
	return v[0]
}

// processRequest processes the incoming request in.
func (o *OTLPReceiver) processRequest(ctx context.Context, header http.Header, in ptraceotlp.ExportRequest) {
	for i := 0; i < in.Traces().ResourceSpans().Len(); i++ {
		rspans := in.Traces().ResourceSpans().At(i)
		o.ReceiveResourceSpans(ctx, rspans, header)
	}
}

// knuthFactor represents a large, prime number ideal for Knuth's Multiplicative Hashing.
// Warning: do not change this number. It is shared with other probabilistic samplers
// in the agent, the Datadog libraries, and in OpenTelemetry. This ensures consistency
// in a distributed system.
const knuthFactor = uint64(1111111111111111111)

// samplingRate returns the rate as defined by the probabilistic sampler.
func (o *OTLPReceiver) samplingRate() float64 {
	rate := o.conf.OTLPReceiver.ProbabilisticSampling / 100
	if rate <= 0 || rate >= 1 {
		// assume that the user wants to keep the trace since he has sent it from
		// his SDK and introduced no sampling mechanisms anywhere else.
		return 1
	}
	return rate
}

// sample returns the sampling priority to apply to a trace with the trace ID tid.
func (o *OTLPReceiver) sample(tid uint64) sampler.SamplingPriority {
	rate := o.samplingRate()
	if rate == 1 {
		return sampler.PriorityAutoKeep
	}
	// the trace ID (tid) is hashed using Knuth's multiplicative hash
	hash := tid * knuthFactor
	if hash < uint64(rate*math.MaxUint64) {
		// if the hash result falls into the rate percentage of the entire distribution
		// of possibly trace IDs (uint64), we sample it.
		return sampler.PriorityAutoKeep
	}
	return sampler.PriorityAutoDrop
}

// ReceiveResourceSpans processes the given rspans and returns the source that it identified from processing them.
func (o *OTLPReceiver) ReceiveResourceSpans(ctx context.Context, rspans ptrace.ResourceSpans, httpHeader http.Header) source.Source {
	// each rspans is coming from a different resource and should be considered
	// a separate payload; typically there is only one item in this slice
	src, srcok := o.conf.OTLPReceiver.AttributesTranslator.ResourceToSource(ctx, rspans.Resource(), traceutil.SignalTypeSet)
	hostFromMap := func(m map[string]string, key string) {
		// hostFromMap sets the hostname to m[key] if it is set.
		if v, ok := m[key]; ok {
			src = source.Source{Kind: source.HostnameKind, Identifier: v}
			srcok = true
		}
	}

	attr := rspans.Resource().Attributes()
	rattr := make(map[string]string, attr.Len())
	attr.Range(func(k string, v pcommon.Value) bool {
		rattr[k] = v.AsString()
		return true
	})
	if !srcok {
		hostFromMap(rattr, "_dd.hostname")
	}
	env := rattr[string(semconv.AttributeDeploymentEnvironment)]
	lang := rattr[string(semconv.AttributeTelemetrySDKLanguage)]
	if lang == "" {
		lang = fastHeaderGet(httpHeader, header.Lang)
	}
	_, containerID := getFirstFromMap(rattr, semconv.AttributeContainerID, semconv.AttributeK8SPodUID)
	if containerID == "" {
		containerID = o.cidProvider.GetContainerID(ctx, httpHeader)
	}
	tagstats := &info.TagStats{
		Tags: info.Tags{
			Lang:            lang,
			LangVersion:     fastHeaderGet(httpHeader, header.LangVersion),
			Interpreter:     fastHeaderGet(httpHeader, header.LangInterpreter),
			LangVendor:      fastHeaderGet(httpHeader, header.LangInterpreterVendor),
			TracerVersion:   fmt.Sprintf("otlp-%s", rattr[string(semconv.AttributeTelemetrySDKVersion)]),
			EndpointVersion: "opentelemetry_grpc_v1",
		},
		Stats: info.NewStats(),
	}
	tracesByID := make(map[uint64]pb.Trace)
	priorityByID := make(map[uint64]sampler.SamplingPriority)
	var spancount int64
	for i := 0; i < rspans.ScopeSpans().Len(); i++ {
		libspans := rspans.ScopeSpans().At(i)
		lib := libspans.Scope()
		for i := 0; i < libspans.Spans().Len(); i++ {
			spancount++
			span := libspans.Spans().At(i)
			traceID := traceIDToUint64(span.TraceID())
			if tracesByID[traceID] == nil {
				tracesByID[traceID] = pb.Trace{}
			}
			ddspan := o.convertSpan(rattr, lib, span)
			if !srcok {
				// if we didn't find a hostname at the resource level
				// try and see if the span has a hostname set
				hostFromMap(ddspan.Meta, "_dd.hostname")
			}
			if env == "" {
				// no env at resource level, try the first span
				if v := ddspan.Meta["env"]; v != "" {
					env = v
				}
			}
			if containerID == "" {
				// no cid at resource level, grab what we can
				_, containerID = getFirstFromMap(ddspan.Meta, semconv.AttributeContainerID, semconv.AttributeK8SPodUID)
			}
			if p, ok := ddspan.Metrics["_sampling_priority_v1"]; ok {
				priorityByID[traceID] = sampler.SamplingPriority(p)
			}
			tracesByID[traceID] = append(tracesByID[traceID], ddspan)
		}
	}
	tags := tagstats.AsTags()
	_ = o.statsd.Count("datadog.trace_agent.otlp.spans", spancount, tags, 1)
	_ = o.statsd.Count("datadog.trace_agent.otlp.traces", int64(len(tracesByID)), tags, 1)
	p := Payload{
		Source:                 tagstats,
		ClientComputedStats:    rattr[keyStatsComputed] != "" || httpHeader.Get(header.ComputedStats) != "",
		ClientComputedTopLevel: o.conf.HasFeature("enable_otlp_compute_top_level_by_span_kind") || httpHeader.Get(header.ComputedTopLevel) != "",
	}
	if env == "" {
		env = o.conf.DefaultEnv
	}

	// Get the hostname or set to empty if source is empty
	var hostname string
	if srcok {
		switch src.Kind {
		case source.HostnameKind:
			hostname = src.Identifier
		default:
			// We are not on a hostname (serverless), hence the hostname is empty
			hostname = ""
		}
	} else {
		// fallback hostname
		hostname = o.conf.Hostname
		src = source.Source{Kind: source.HostnameKind, Identifier: hostname}
	}
	p.TracerPayload = &pb.TracerPayload{
		Hostname:        hostname,
		Chunks:          o.createChunks(tracesByID, priorityByID),
		Env:             traceutil.NormalizeTag(env),
		ContainerID:     containerID,
		LanguageName:    tagstats.Lang,
		LanguageVersion: tagstats.LangVersion,
		TracerVersion:   tagstats.TracerVersion,
	}
	ctags := attributes.ContainerTagsFromResourceAttributes(attr)
	payloadTags := flatten(ctags)
	if tags := getContainerTags(o.conf.ContainerTags, containerID); tags != "" {
		appendTags(payloadTags, tags)
	} else {
		// we couldn't obtain any container tags
		if src.Kind == source.AWSECSFargateKind {
			// but we have some information from the source provider that we can add
			appendTags(payloadTags, src.Tag())
		}
	}
	if payloadTags.Len() > 0 {
		p.TracerPayload.Tags = map[string]string{
			tagContainersTags: payloadTags.String(),
		}
	}

	o.out <- &p
	return src
}

func appendTags(str *strings.Builder, tags string) {
	if str.Len() > 0 {
		str.WriteByte(',')
	}
	str.WriteString(tags)
}

func flatten(m map[string]string) *strings.Builder {
	var str strings.Builder
	for k, v := range m {
		if str.Len() > 0 {
			str.WriteByte(',')
		}
		str.WriteString(k)
		str.WriteString(":")
		str.WriteString(v)
	}
	return &str
}

// createChunks creates a set of pb.TraceChunk's based on two maps:
// - a map from trace ID to the spans sharing that trace ID
// - a map of user-set sampling priorities by trace ID, if set
func (o *OTLPReceiver) createChunks(tracesByID map[uint64]pb.Trace, prioritiesByID map[uint64]sampler.SamplingPriority) []*pb.TraceChunk {
	traceChunks := make([]*pb.TraceChunk, 0, len(tracesByID))
	for k, spans := range tracesByID {
		if len(spans) == 0 {
			continue
		}
		rate := strconv.FormatFloat(o.samplingRate(), 'f', 2, 64)
		chunk := &pb.TraceChunk{
			Tags:  map[string]string{"_dd.otlp_sr": rate},
			Spans: spans,
		}
		if p, ok := prioritiesByID[k]; ok {
			// a manual decision has been made by the user
			chunk.Priority = int32(p)
			traceutil.SetMeta(spans[0], "_dd.p.dm", "-4")
		} else {
			// we use the probabilistic sampler to decide
			chunk.Priority = int32(o.sample(k))
			traceutil.SetMeta(spans[0], "_dd.p.dm", "-9")
		}
		traceChunks = append(traceChunks, chunk)
	}
	return traceChunks
}

// marshalEvents marshals events into JSON.
func marshalEvents(events ptrace.SpanEventSlice) string {
	var str strings.Builder
	str.WriteString("[")
	for i := 0; i < events.Len(); i++ {
		e := events.At(i)
		if i > 0 {
			str.WriteString(",")
		}
		var wrote bool
		str.WriteString("{")
		if v := e.Timestamp(); v != 0 {
			str.WriteString(`"time_unix_nano":`)
			str.WriteString(strconv.FormatUint(uint64(v), 10))
			wrote = true
		}
		if v := e.Name(); v != "" {
			if wrote {
				str.WriteString(",")
			}
			str.WriteString(`"name":"`)
			str.WriteString(v)
			str.WriteString(`"`)
			wrote = true
		}
		if e.Attributes().Len() > 0 {
			if wrote {
				str.WriteString(",")
			}
			str.WriteString(`"attributes":{`)
			j := 0
			e.Attributes().Range(func(k string, v pcommon.Value) bool {
				if j > 0 {
					str.WriteString(",")
				}
				str.WriteString(`"`)
				str.WriteString(k)
				str.WriteString(`":"`)
				str.WriteString(v.AsString())
				str.WriteString(`"`)
				j++
				return true
			})
			str.WriteString("}")
			wrote = true
		}
		if v := e.DroppedAttributesCount(); v != 0 {
			if wrote {
				str.WriteString(",")
			}
			str.WriteString(`"dropped_attributes_count":`)
			str.WriteString(strconv.FormatUint(uint64(v), 10))
		}
		str.WriteString("}")
	}
	str.WriteString("]")
	return str.String()
}

// marshalLinks marshals span links into JSON.
func marshalLinks(links ptrace.SpanLinkSlice) string {
	var str strings.Builder
	str.WriteString("[")
	for i := 0; i < links.Len(); i++ {
		l := links.At(i)
		if i > 0 {
			str.WriteString(",")
		}
		t := l.TraceID()
		str.WriteString(`{"trace_id":"`)
		str.WriteString(hex.EncodeToString(t[:]))
		s := l.SpanID()
		str.WriteString(`","span_id":"`)
		str.WriteString(hex.EncodeToString(s[:]))
		str.WriteString(`"`)
		if ts := l.TraceState().AsRaw(); len(ts) > 0 {
			str.WriteString(`,"trace_state":"`)
			str.WriteString(ts)
			str.WriteString(`"`)
		}
		if l.Attributes().Len() > 0 {
			str.WriteString(`,"attributes":{`)
			var b bool
			l.Attributes().Range(func(k string, v pcommon.Value) bool {
				if b {
					str.WriteString(",")
				}
				b = true
				str.WriteString(`"`)
				str.WriteString(k)
				str.WriteString(`":"`)
				str.WriteString(v.AsString())
				str.WriteString(`"`)
				return true
			})
			str.WriteString("}")
		}
		if l.DroppedAttributesCount() > 0 {
			str.WriteString(`,"dropped_attributes_count":`)
			str.WriteString(strconv.FormatUint(uint64(l.DroppedAttributesCount()), 10))
		}
		str.WriteString("}")
	}
	str.WriteString("]")
	return str.String()
}

// setMetaOTLP sets the k/v OTLP attribute pair as a tag on span s.
func setMetaOTLP(s *pb.Span, k, v string) {
	switch k {
	case "operation.name":
		s.Name = v
	case "service.name":
		s.Service = v
	case "resource.name":
		s.Resource = v
	case "span.type":
		s.Type = v
	case "analytics.event":
		if v, err := strconv.ParseBool(v); err == nil {
			if v {
				s.Metrics[sampler.KeySamplingRateEventExtraction] = 1
			} else {
				s.Metrics[sampler.KeySamplingRateEventExtraction] = 0
			}
		}
	default:
		s.Meta[k] = v
	}
}

// setMetricOTLP sets the k/v OTLP attribute pair as a metric on span s.
func setMetricOTLP(s *pb.Span, k string, v float64) {
	switch k {
	case "sampling.priority":
		s.Metrics["_sampling_priority_v1"] = v
	default:
		s.Metrics[k] = v
	}
}

// convertSpan converts the span in to a Datadog span, and uses the rattr resource tags and the lib instrumentation
// library attributes to further augment it.
func (o *OTLPReceiver) convertSpan(rattr map[string]string, lib pcommon.InstrumentationScope, in ptrace.Span) *pb.Span {
	traceID := [16]byte(in.TraceID())
	span := &pb.Span{
		TraceID:  traceIDToUint64(traceID),
		SpanID:   spanIDToUint64(in.SpanID()),
		ParentID: spanIDToUint64(in.ParentSpanID()),
		Start:    int64(in.StartTimestamp()),
		Duration: int64(in.EndTimestamp()) - int64(in.StartTimestamp()),
		Meta:     make(map[string]string, len(rattr)),
		Metrics:  map[string]float64{},
	}
	for k, v := range rattr {
		setMetaOTLP(span, k, v)
	}

	spanKind := in.Kind()
	if o.conf.HasFeature("enable_otlp_compute_top_level_by_span_kind") {
		computeTopLevelAndMeasured(span, spanKind)
	}

	setMetaOTLP(span, "otel.trace_id", hex.EncodeToString(traceID[:]))
	setMetaOTLP(span, "span.kind", spanKindName(spanKind))
	if _, ok := span.Meta["version"]; !ok {
		if ver := rattr[string(semconv.AttributeServiceVersion)]; ver != "" {
			setMetaOTLP(span, "version", ver)
		}
	}
	if in.Events().Len() > 0 {
		setMetaOTLP(span, "events", marshalEvents(in.Events()))
	}
	if in.Links().Len() > 0 {
		setMetaOTLP(span, "_dd.span_links", marshalLinks(in.Links()))
	}
	in.Attributes().Range(func(k string, v pcommon.Value) bool {
		switch v.Type() {
		case pcommon.ValueTypeDouble:
			setMetricOTLP(span, k, v.Double())
		case pcommon.ValueTypeInt:
			setMetricOTLP(span, k, float64(v.Int()))
		default:
			setMetaOTLP(span, k, v.AsString())
		}
		return true
	})
	if _, ok := span.Meta["env"]; !ok {
		if env := span.Meta[string(semconv.AttributeDeploymentEnvironment)]; env != "" {
			setMetaOTLP(span, "env", traceutil.NormalizeTag(env))
		}
	}
	if in.TraceState().AsRaw() != "" {
		setMetaOTLP(span, "w3c.tracestate", in.TraceState().AsRaw())
	}
	if lib.Name() != "" {
		setMetaOTLP(span, semconv.OtelLibraryName, lib.Name())
	}
	if lib.Version() != "" {
		setMetaOTLP(span, semconv.OtelLibraryVersion, lib.Version())
	}
	setMetaOTLP(span, semconv.OtelStatusCode, in.Status().Code().String())
	if msg := in.Status().Message(); msg != "" {
		setMetaOTLP(span, semconv.OtelStatusDescription, msg)
	}
	status2Error(in.Status(), in.Events(), span)
	if span.Name == "" {
		name := in.Name()
		if !o.conf.OTLPReceiver.SpanNameAsResourceName {
			name = spanKindName(in.Kind())
			if lib.Name() != "" {
				name = lib.Name() + "." + name
			} else {
				name = "opentelemetry." + name
			}
		}
		if v, ok := o.conf.OTLPReceiver.SpanNameRemappings[name]; ok {
			name = v
		}
		span.Name = name
	}
	if span.Service == "" {
		span.Service = "OTLPResourceNoServiceName"
	}
	if span.Resource == "" {
		if r := resourceFromTags(span.Meta); r != "" {
			span.Resource = r
		} else {
			span.Resource = in.Name()
		}
	}
	if span.Type == "" {
		span.Type = spanKind2Type(in.Kind(), span)
	}
	return span
}

// resourceFromTags attempts to deduce a more accurate span resource from the given list of tags meta.
// If this is not possible, it returns an empty string.
func resourceFromTags(meta map[string]string) string {
	if m := meta[string(semconv.AttributeHTTPMethod)]; m != "" {
		// use the HTTP method + route (if available)
		if _, route := getFirstFromMap(meta, semconv.AttributeHTTPRoute, "grpc.path"); route != "" {
			return m + " " + route
		}
		return m
	} else if m := meta[string(semconv.AttributeMessagingOperation)]; m != "" {
		// use the messaging operation
		if _, dest := getFirstFromMap(meta, semconv.AttributeMessagingDestination, semconv117.AttributeMessagingDestinationName); dest != "" {
			return m + " " + dest
		}
		return m
	} else if m := meta[string(semconv.AttributeRPCMethod)]; m != "" {
		// use the RPC method
		if svc := meta[string(semconv.AttributeRPCService)]; svc != "" {
			// ...and service if available
			return m + " " + svc
		}
		return m
	}
	return ""
}

// getFirstFromMap checks each key in the given keys in the map and returns the first key-value pair whose
// key matches, or empty strings if none matches.
func getFirstFromMap(m map[string]string, keys ...string) (string, string) {
	for _, key := range keys {
		if val := m[key]; val != "" {
			return key, val
		}
	}
	return "", ""
}

// status2Error checks the given status and events and applies any potential error and messages
// to the given span attributes.
func status2Error(status ptrace.Status, events ptrace.SpanEventSlice, span *pb.Span) {
	if status.Code() != ptrace.StatusCodeError {
		return
	}
	span.Error = 1
	for i := 0; i < events.Len(); i++ {
		e := events.At(i)
		if strings.ToLower(e.Name()) != "exception" {
			continue
		}
		attrs := e.Attributes()
		if v, ok := attrs.Get(semconv.AttributeExceptionMessage); ok {
			span.Meta["error.msg"] = v.AsString()
		}
		if v, ok := attrs.Get(semconv.AttributeExceptionType); ok {
			span.Meta["error.type"] = v.AsString()
		}
		if v, ok := attrs.Get(semconv.AttributeExceptionStacktrace); ok {
			span.Meta["error.stack"] = v.AsString()
		}
	}
	if _, ok := span.Meta["error.msg"]; !ok {
		// no error message was extracted, find alternatives
		if status.Message() != "" {
			// use the status message
			span.Meta["error.msg"] = status.Message()
		} else if httpcode, ok := span.Meta["http.status_code"]; ok {
			// we have status code that we can use as details
			if httptext, ok := span.Meta["http.status_text"]; ok {
				span.Meta["error.msg"] = fmt.Sprintf("%s %s", httpcode, httptext)
			} else {
				span.Meta["error.msg"] = httpcode
			}
		}
	}
}

// spanKind2Type returns a span's type based on the given kind and other present properties.
func spanKind2Type(kind ptrace.SpanKind, span *pb.Span) string {
	var typ string
	switch kind {
	case ptrace.SpanKindServer:
		typ = "web"
	case ptrace.SpanKindClient:
		typ = "http"
		db, ok := span.Meta[string(semconv.AttributeDBSystem)]
		if !ok {
			break
		}
		switch db {
		case "redis", "memcached":
			typ = "cache"
		default:
			typ = "db"
		}
	default:
		typ = "custom"
	}
	return typ
}

func traceIDToUint64(b [16]byte) uint64 {
	return binary.BigEndian.Uint64(b[len(b)-8:])
}

func spanIDToUint64(b [8]byte) uint64 {
	return binary.BigEndian.Uint64(b[:])
}

var spanKindNames = map[int32]string{
	0: "unspecified",
	1: "internal",
	2: "server",
	3: "client",
	4: "producer",
	5: "consumer",
}

// spanKindName converts the given SpanKind to a valid Datadog span name.
func spanKindName(k ptrace.SpanKind) string {
	name, ok := spanKindNames[int32(k)]
	if !ok {
		return "unknown"
	}
	return name
}

// computeTopLevelAndMeasured updates the span's top-level and measured attributes.
//
// An OTLP span is considered top-level if it is a root span or has a span kind of server or consumer.
// An OTLP span is marked as measured if it has a span kind of client or producer.
func computeTopLevelAndMeasured(span *pb.Span, spanKind ptrace.SpanKind) {
	if span.ParentID == 0 {
		// span is a root span
		traceutil.SetTopLevel(span, true)
	}
	if spanKind == ptrace.SpanKindServer || spanKind == ptrace.SpanKindConsumer {
		// span is a server-side span
		traceutil.SetTopLevel(span, true)
	}
	if spanKind == ptrace.SpanKindClient || spanKind == ptrace.SpanKindProducer {
		// span is a client-side span, not top-level but we still want stats
		traceutil.SetMeasured(span, true)
	}
}
