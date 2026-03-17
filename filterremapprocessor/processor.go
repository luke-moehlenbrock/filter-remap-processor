package filterremapprocessor

import (
	"context"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	lru "github.com/hashicorp/golang-lru/v2"
	"go.uber.org/zap"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.opentelemetry.io/collector/processor"
	"go.uber.org/multierr"

	"github.com/luke-moehlenbrock/otel-collector-filter-remap-processor/filterremapprocessor/internal/metadata"
	"github.com/luke-moehlenbrock/otel-collector-filter-remap-processor/filterremapprocessor/internal/utils"

	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl/contexts/ottlspan"
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl/contexts/ottlspanevent"
)

type filterRemapProcessor struct {
	ctx                      context.Context
	skipSpanExpr             utils.BoolExpr[ottlspan.TransformContext]      // if evaluates to True, drop span
	skipSpanEventExpr        utils.BoolExpr[ottlspanevent.TransformContext] // if evaluates to True, drop span event
	maxRetentionTicker       utils.Ticker
	processingTicker         utils.Ticker
	tickerFrequency          time.Duration
	nextConsumer             consumer.Traces
	forwardTraceChan         chan *traceData
	numTracesOnMap           *atomic.Uint64
	maxTraceRetention        time.Duration
	lastSpanTimeout          time.Duration
	maxNumTraces             uint64
	avgSpansPerTrace         uint64
	dropRootSpans            bool
	remapOrphanedSpans       bool
	idToTrace                *lru.Cache[pcommon.TraceID, *traceData]
	telemetry                *metadata.TelemetryBuilder
	logger                   *zap.Logger
	flushOnShutdown          bool
	forwardWorkerConcurrency int
	wg                       sync.WaitGroup
	overflowStrategy         OverflowStrategy
}

func newFilterRemapProcessor(ctx context.Context, set processor.Settings, nextConsumer consumer.Traces, cfg *Config) (*filterRemapProcessor, error) {
	var err error

	telemetrySettings := set.TelemetrySettings
	telemetry, err := metadata.NewTelemetryBuilder(telemetrySettings)
	if err != nil {
		return nil, err
	}

	if cfg.ForwardQueueSize == 0 {
		cfg.ForwardQueueSize = max(4096, 2*cfg.ExpectedNewTracesPerSec)
	}

	if cfg.ForwardWorkerConcurrency == 0 {
		cfg.ForwardWorkerConcurrency = min(8, runtime.GOMAXPROCS(0))
	}

	frp := &filterRemapProcessor{
		ctx:                      ctx,
		logger:                   set.Logger,
		nextConsumer:             nextConsumer,
		forwardTraceChan:         make(chan *traceData, cfg.ForwardQueueSize),
		dropRootSpans:            cfg.DropRootSpans,
		remapOrphanedSpans:       cfg.RemapOrphanedSpans,
		maxNumTraces:             cfg.NumTraces,
		maxTraceRetention:        cfg.MaxTraceRetention,
		lastSpanTimeout:          cfg.LastSpanTimeout,
		telemetry:                telemetry,
		numTracesOnMap:           &atomic.Uint64{},
		avgSpansPerTrace:         cfg.ExpectedAverageSpansPerTrace,
		flushOnShutdown:          cfg.FlushOnShutdown,
		forwardWorkerConcurrency: cfg.ForwardWorkerConcurrency,
		wg:                       sync.WaitGroup{},
		overflowStrategy:         cfg.OverflowStrategy,
	}

	// Initialize LRU cache for traces
	cache, err := lru.NewWithEvict(int(cfg.NumTraces), frp.evictTrace)
	if err != nil {
		return nil, err
	}

	frp.idToTrace = cache

	if cfg.Traces.SpanConditions != nil {
		frp.skipSpanExpr, err = utils.NewBoolExprForSpan(cfg.Traces.SpanConditions, utils.StandardSpanFuncs(), cfg.ErrorMode, set.TelemetrySettings)
		if err != nil {
			return nil, err
		}
	}

	if cfg.Traces.SpanEventConditions != nil {
		frp.skipSpanEventExpr, err = utils.NewBoolExprForSpanEvent(cfg.Traces.SpanEventConditions, utils.StandardSpanEventFuncs(), cfg.ErrorMode, set.TelemetrySettings)
		if err != nil {
			return nil, err
		}
	}

	frp.processingTicker = &utils.ProcessorTicker{OnTickFunc: frp.onTick}
	frp.maxRetentionTicker = &utils.ProcessorTicker{OnTickFunc: frp.maxRetentionOnTick}

	if frp.tickerFrequency == 0 {
		frp.tickerFrequency = time.Second
	}

	return frp, nil
}

func (frp *filterRemapProcessor) ConsumeTraces(ctx context.Context, td ptrace.Traces) error {
	resourceSpans := td.ResourceSpans()
	for i := 0; i < resourceSpans.Len(); i++ {
		frp.processTraces(ctx, resourceSpans.At(i))
	}
	return nil
}

func (frp *filterRemapProcessor) filterSpansByTraceId(resourceSpans ptrace.ResourceSpans) (map[pcommon.TraceID][]hierarchyNode, error) {
	traceIdToSpans := make(map[pcommon.TraceID][]hierarchyNode)
	resource := resourceSpans.Resource()
	resourceSchemaUrl := resourceSpans.SchemaUrl()
	//ilss stands for instrumentation library scope spans.
	ilss := resourceSpans.ScopeSpans()
	var errors error
	for j := 0; j < ilss.Len(); j++ {
		scope := ilss.At(j)
		scopeSchemaUrl := scope.SchemaUrl()
		spans := scope.Spans()
		is := scope.Scope()
		spansLen := spans.Len()
		for k := 0; k < spansLen; k++ {
			span := spans.At(k)
			traceId := span.TraceID()
			parentSpanId := span.ParentSpanID()

			startTime := time.Now()
			drop, err := frp.shouldFilterSpan(span, is, resource, scope, resourceSpans)

			if err != nil {
				frp.telemetry.ProcessorFilterRemapOttlEvaluationError.Add(frp.ctx, 1)
				errors = multierr.Append(errors, err)
				continue
			}

			_, ok := traceIdToSpans[traceId]
			if !ok {
				traceIdToSpans[traceId] = make([]hierarchyNode, 0, frp.avgSpansPerTrace)
			}

			frp.telemetry.ProcessorFilterRemapSpanDecisionLatency.Record(frp.ctx, time.Since(startTime).Microseconds())
			if drop {
				hn := &droppedHierarchyNode{
					spanId:       span.SpanID(),
					traceId:      traceId,
					parentSpanId: parentSpanId,
				}
				traceIdToSpans[traceId] = append(traceIdToSpans[traceId], hn)
			} else {
				hn := &retainedHierarchyNode{
					spanData: &spanAndScope{
						span:                 &span,
						instrumentationScope: &is,
						resource:             &resource,
						scopeSchemaUrl:       scopeSchemaUrl,
						resourceSchemaUrl:    resourceSchemaUrl,
					},
				}
				if parentSpanId.IsEmpty() {
					hn.setParentId(parentSpanId, true)
				}
				traceIdToSpans[traceId] = append(traceIdToSpans[traceId], hn)
			}
		}
	}
	return traceIdToSpans, errors
}

func (frp *filterRemapProcessor) processTraces(ctx context.Context, resourceSpans ptrace.ResourceSpans) {
	currTime := time.Now()

	// Group spans per their traceId to minimize contention on traceIdToTrace
	traceIdToHierarchyNodes, err := frp.filterSpansByTraceId(resourceSpans)
	var newTraceIDs int64
	if err != nil {
		frp.logger.Error("Error(s) filtering spans by traceId", zap.Error(err))
	}
	for id, nodes := range traceIdToHierarchyNodes {

		lenSpans := int64(len(nodes))

		// Have we already seen the trace this batch belongs to? If not we need to create a new traceData object and add it to the map
		tData, loaded := frp.idToTrace.Get(id)
		if !loaded {
			td := &traceData{
				Ctx:                  ctx,
				ArrivalTime:          currTime,
				LastSpanArrivalNanos: atomic.Int64{},
				HierarchyMap: hierarchyMap{
					m: make(map[pcommon.SpanID]hierarchyNode),
				},
				SpanCount: atomic.Int64{},
			}

			td.SpanCount.Store(lenSpans)
			td.LastSpanArrivalNanos.Store(currTime.UnixNano())

			frp.idToTrace.Add(id, td)
			newTraceIDs++
			frp.numTracesOnMap.Add(1)
			tData = td
		}

		if loaded {
			tData.SpanCount.Add(lenSpans)
			tData.LastSpanArrivalNanos.Store(currTime.UnixNano())
		}

		for _, hn := range nodes {
			tData.HierarchyMap.set(hn.SpanID(), hn)
		}
	}

	frp.telemetry.ProcessorFilterRemapNewTraceIDReceived.Add(frp.ctx, newTraceIDs)

}

func buildRemappedTrace(spanAndScopes []spanAndScope) ptrace.Traces {
	dest := ptrace.NewTraces()
	resourcePointerToNewResource := make(map[*pcommon.Resource]*ptrace.ResourceSpans)
	scopePointerToNewScope := make(map[*pcommon.InstrumentationScope]*ptrace.ScopeSpans)
	for _, spanAndScope := range spanAndScopes {
		resource, ok := resourcePointerToNewResource[spanAndScope.resource]
		if !ok {
			rs := dest.ResourceSpans().AppendEmpty()
			rs.SetSchemaUrl(spanAndScope.resourceSchemaUrl)
			spanAndScope.resource.CopyTo(rs.Resource())
			resourcePointerToNewResource[spanAndScope.resource] = &rs
			resource = &rs
		}
		// If the scope of the spanAndScope is not in the map, add it to the map and the destination.
		if scope, ok := scopePointerToNewScope[spanAndScope.instrumentationScope]; !ok {
			is := resource.ScopeSpans().AppendEmpty()
			is.SetSchemaUrl(spanAndScope.scopeSchemaUrl)
			spanAndScope.instrumentationScope.CopyTo(is.Scope())
			scopePointerToNewScope[spanAndScope.instrumentationScope] = &is

			sp := is.Spans().AppendEmpty()
			spanAndScope.span.CopyTo(sp)
		} else {
			sp := scope.Spans().AppendEmpty()
			spanAndScope.span.CopyTo(sp)
		}
	}
	return dest
}

func (frp *filterRemapProcessor) shouldFilterSpan(span ptrace.Span, is pcommon.InstrumentationScope, resource pcommon.Resource, scope ptrace.ScopeSpans, resourceSpans ptrace.ResourceSpans) (bool, error) {
	if !frp.dropRootSpans && span.ParentSpanID().IsEmpty() {
		return false, nil
	}
	if frp.skipSpanExpr != nil {
		skipSpan, err := frp.skipSpanExpr.Eval(frp.ctx, ottlspan.NewTransformContext(span, is, resource, scope, resourceSpans))
		if err != nil {
			return false, err
		}

		if skipSpan {
			frp.telemetry.ProcessorFilterRemapCountSpansSampled.Add(frp.ctx, 1)
			return true, nil
		}
	}

	if frp.skipSpanEventExpr != nil {
		var evalErr error
		span.Events().RemoveIf(func(spanEvent ptrace.SpanEvent) bool {
			skipSpanEvent, err := frp.skipSpanEventExpr.Eval(frp.ctx, ottlspanevent.NewTransformContext(spanEvent, span, is, resource, scope, resourceSpans))
			if err != nil {
				evalErr = multierr.Append(evalErr, err)
				return false
			}
			if skipSpanEvent {
				return true
			}
			return false
		})
		if evalErr != nil {
			return false, evalErr
		}
	}
	spanEvents := span.Events()
	for i := 0; i < spanEvents.Len(); i++ {
		if frp.skipSpanEventExpr == nil {
			break
		}
		spanEvent := spanEvents.At(i)
		skipSpanEvent, err := frp.skipSpanEventExpr.Eval(frp.ctx, ottlspanevent.NewTransformContext(spanEvent, span, is, resource, scope, resourceSpans))
		if err != nil {
			return false, err
		}
		if skipSpanEvent {
			frp.telemetry.ProcessorFilterRemapCountSpansSampled.Add(frp.ctx, 1)
			return true, nil
		}
	}
	return false, nil
}

func (frp *filterRemapProcessor) onTick() {
	currTime := time.Now()

	for _, traceId := range frp.idToTrace.Keys() {
		trace, ok := frp.idToTrace.Peek(traceId)
		if !ok {
			continue
		}
		if currTime.UnixNano()-trace.LastSpanArrivalNanos.Load() > frp.lastSpanTimeout.Nanoseconds() {
			//Remove will trigger evictTrace which will remap the trace hierarchy and forward the trace to the next consumer
			frp.idToTrace.Remove(traceId)
		} else {
			// idToTrace.Keys() returns keys from oldest to newest, so as soon as we see a trace where the most recent span is within the last span timeout, we can stop looking
			break
		}

	}

	frp.telemetry.ProcessorFilterRemapTickProcessingTime.Record(frp.ctx, time.Since(currTime).Milliseconds())
	frp.telemetry.ProcessorFilterRemapTracesOnMemory.Record(frp.ctx, int64(frp.numTracesOnMap.Load()))
}

func (frp *filterRemapProcessor) maxRetentionOnTick() {
	currTime := time.Now()
	for _, traceId := range frp.idToTrace.Keys() {
		trace, ok := frp.idToTrace.Peek(traceId)
		if !ok {
			continue
		}
		if time.Since(trace.ArrivalTime) > frp.maxTraceRetention {
			frp.idToTrace.Remove(traceId)
		}
	}
	frp.telemetry.ProcessorFilterRemapMaxRetentionTickProcessingTime.Record(frp.ctx, time.Since(currTime).Milliseconds())
}

// on eviction due to capacity, we still want to remap and forward the trace
func (frp *filterRemapProcessor) evictTrace(_ pcommon.TraceID, trace *traceData) {
	select {
	case frp.forwardTraceChan <- trace:
	default:
		if frp.overflowStrategy == OverflowForward {
			go frp.forwardTrace(trace)
		}
		frp.telemetry.ProcessorFilterRemapForwardQueueOverflows.Add(frp.ctx, 1)
	}
	frp.numTracesOnMap.Add(^uint64(0))
}

func (frp *filterRemapProcessor) forwardTrace(trace *traceData) {
	currTime := time.Now()
	trace.HierarchyMap.remapHierarchy(frp.remapOrphanedSpans)
	allSpansRetained := make([]spanAndScope, 0, trace.SpanCount.Load())
	frp.telemetry.ProcessorFilterRemapSpansPerTrace.Record(frp.ctx, int64(trace.SpanCount.Load()))
	trace.HierarchyMap.RLock()
	for _, node := range trace.HierarchyMap.m {
		switch node.(type) {
		case *retainedHierarchyNode:
			allSpansRetained = append(allSpansRetained, *node.(*retainedHierarchyNode).spanData)
		}
	}
	trace.HierarchyMap.RUnlock()
	startTime := time.Now()
	remappedTrace := buildRemappedTrace(allSpansRetained)
	frp.telemetry.ProcessorFilterRemapTraceRemapLatency.Record(frp.ctx, time.Since(startTime).Microseconds())
	frp.nextConsumer.ConsumeTraces(trace.Ctx, remappedTrace)
	frp.telemetry.ProcessorFilterRemapForwardTraceLatency.Record(frp.ctx, time.Since(currTime).Milliseconds())
}

func (frp *filterRemapProcessor) forwardTraceWorker() {
	defer frp.wg.Done()
	for trace := range frp.forwardTraceChan {
		frp.forwardTrace(trace)
	}
}

func (frp *filterRemapProcessor) Capabilities() consumer.Capabilities {
	return consumer.Capabilities{MutatesData: true}
}

func (frp *filterRemapProcessor) Start(context.Context, component.Host) error {
	frp.processingTicker.Start(frp.tickerFrequency)
	// Start the max retention ticker at half of the max trace retention or 15 seconds, whichever is smaller
	// might be a bit hacky, will want to see how long the max retention ticker takes to run since it has to loop through all traces and use that to come up with a better duration.
	frp.maxRetentionTicker.Start(min(frp.maxTraceRetention/2, 15*time.Second))
	for i := 0; i < frp.forwardWorkerConcurrency; i++ {
		frp.wg.Add(1)
		go frp.forwardTraceWorker()
	}

	return nil
}

func (frp *filterRemapProcessor) Shutdown(context.Context) error {
	frp.processingTicker.Stop()
	frp.maxRetentionTicker.Stop()
	if frp.flushOnShutdown {
		for _, trace := range frp.idToTrace.Values() {
			frp.forwardTrace(trace)
		}
	}
	// Close the forward trace channel to signal the worker to exit
	close(frp.forwardTraceChan)
	frp.wg.Wait()
	return nil
}
