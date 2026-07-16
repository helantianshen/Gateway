package observability

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/helantianshen/gateway/internal/dataplane/requestctx"
)

const DefaultRouteSeriesBudget = 1000

// EndpointState 是 endpoint Collector 读取 Phase 4 atomic 状态所需的最小接口。
type EndpointState interface {
	Healthy() bool
	ActiveRequests() int64
}

// EndpointSource 把稳定配置身份与可变 endpoint 状态绑定。
type EndpointSource struct {
	UpstreamID string
	EndpointID string
	State      EndpointState
}

// Metrics 持有单个 Application 私有的 Registry 和全部 Collector。
type Metrics struct {
	registry *prometheus.Registry

	requestsTotal    *prometheus.CounterVec
	requestDuration  *prometheus.HistogramVec
	inflightRequests prometheus.Gauge
	rejectionsTotal  *prometheus.CounterVec
	routeRequests    *prometheus.CounterVec

	upstreamRequests *prometheus.CounterVec
	upstreamDuration *prometheus.HistogramVec
	configVersion    prometheus.Gauge

	routeDetailsEnabled bool
	endpoints           *endpointCollector
}

// NewMetrics 创建并注册一组完全私有的 Gateway 指标。
func NewMetrics(routeCount, routeSeriesBudget int) (*Metrics, error) {
	if routeSeriesBudget <= 0 {
		routeSeriesBudget = DefaultRouteSeriesBudget
	}
	registry := prometheus.NewRegistry()
	metrics := &Metrics{
		registry: registry,
		requestsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "gateway",
			Name:      "requests_total",
			Help:      "Total public requests handled by the gateway.",
		}, []string{"method", "status_class", "upstream_id", "error_kind"}),
		requestDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "gateway",
			Name:      "request_duration_seconds",
			Help:      "End-to-end public request duration in seconds.",
			Buckets:   prometheus.DefBuckets,
		}, []string{"method", "status_class", "upstream_id"}),
		inflightRequests: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "gateway",
			Name:      "inflight_requests",
			Help:      "Current number of public requests in flight.",
		}),
		rejectionsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "gateway",
			Name:      "rejections_total",
			Help:      "Gateway-generated request rejections by stable error code.",
		}, []string{"code"}),
		routeRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "gateway",
			Name:      "route_requests_total",
			Help:      "Requests grouped by configured route identity within a fixed series budget.",
		}, []string{"route_id", "path_template", "status_class"}),
		upstreamRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "gateway",
			Name:      "upstream_requests_total",
			Help:      "Endpoint attempts by configured upstream and endpoint.",
		}, []string{"upstream_id", "endpoint_id", "outcome", "error_kind"}),
		upstreamDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "gateway",
			Name:      "upstream_duration_seconds",
			Help:      "Duration of a single endpoint attempt in seconds.",
			Buckets:   prometheus.DefBuckets,
		}, []string{"upstream_id", "endpoint_id", "outcome"}),
		configVersion: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "gateway",
			Name:      "config_version",
			Help:      "Current process-local compiled configuration version.",
		}),
		routeDetailsEnabled: routeCount <= routeSeriesBudget,
		endpoints:           newEndpointCollector(),
	}

	collectors := []prometheus.Collector{
		metrics.requestsTotal,
		metrics.requestDuration,
		metrics.inflightRequests,
		metrics.rejectionsTotal,
		metrics.routeRequests,
		metrics.upstreamRequests,
		metrics.upstreamDuration,
		metrics.configVersion,
		metrics.endpoints,
	}
	for _, collector := range collectors {
		if err := registry.Register(collector); err != nil {
			return nil, fmt.Errorf("注册 Gateway Prometheus collector 失败: %w", err)
		}
	}
	metrics.configVersion.Set(1)
	return metrics, nil
}

// BindEndpoints 在 listener 创建前绑定配置身份与 atomic 状态。
func (m *Metrics) BindEndpoints(sources []EndpointSource) error {
	if m == nil || m.endpoints == nil {
		return fmt.Errorf("绑定 endpoint 指标失败: Metrics 不能为空")
	}
	return m.endpoints.bind(sources)
}

// Handler 返回只暴露当前私有 Registry 的 Prometheus Handler。
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{EnableOpenMetrics: true})
}

// Registry 返回私有 Registry，供测试和后续受控组合使用。
func (m *Metrics) Registry() *prometheus.Registry {
	if m == nil {
		return nil
	}
	return m.registry
}

// RouteDetailsEnabled 表示 per-route 指标是否仍在预算内。
func (m *Metrics) RouteDetailsEnabled() bool {
	return m != nil && m.routeDetailsEnabled
}

// RequestStarted 实现 middleware.RequestObserver。
func (m *Metrics) RequestStarted() {
	if m != nil {
		m.inflightRequests.Inc()
	}
}

// RequestFinished 实现 middleware.RequestObserver。
func (m *Metrics) RequestFinished(method string, snapshot requestctx.Snapshot, duration time.Duration) {
	if m == nil {
		return
	}
	m.inflightRequests.Dec()

	methodLabel := normalizeMethod(method)
	statusClass := normalizeStatusClass(snapshot.ResponseStatus)
	upstreamID := normalizeEmpty(snapshot.UpstreamID)
	errorKind := normalizeEmpty(snapshot.ErrorKind)

	m.requestsTotal.WithLabelValues(methodLabel, statusClass, upstreamID, errorKind).Inc()
	m.requestDuration.WithLabelValues(methodLabel, statusClass, upstreamID).Observe(duration.Seconds())

	if snapshot.ErrorKind != "" && snapshot.ErrorKind != "CLIENT_CANCELED" && snapshot.ResponseStatus >= 400 {
		m.rejectionsTotal.WithLabelValues(snapshot.ErrorKind).Inc()
	}

	routeID, pathTemplate := "_unmatched", "_unmatched"
	if snapshot.RouteID != "" {
		routeID = snapshot.RouteID
		pathTemplate = normalizeEmpty(snapshot.PathTemplate)
	}
	if !m.routeDetailsEnabled {
		routeID, pathTemplate = "_other", "_other"
	}
	m.routeRequests.WithLabelValues(routeID, pathTemplate, statusClass).Inc()

	if snapshot.Attempts > 0 {
		endpointID := normalizeEmpty(snapshot.EndpointID)
		outcome := normalizeEmpty(snapshot.Outcome)
		m.upstreamRequests.WithLabelValues(upstreamID, endpointID, outcome, errorKind).Inc()
		m.upstreamDuration.WithLabelValues(upstreamID, endpointID, outcome).Observe(snapshot.UpstreamDuration.Seconds())
	}
}

func normalizeMethod(method string) string {
	switch strings.ToUpper(method) {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch,
		http.MethodDelete, http.MethodOptions, http.MethodConnect, http.MethodTrace:
		return strings.ToUpper(method)
	default:
		return "_OTHER"
	}
}

func normalizeStatusClass(status int) string {
	if status < 100 || status > 599 {
		return "_none"
	}
	return fmt.Sprintf("%dxx", status/100)
}

func normalizeEmpty(value string) string {
	if value == "" {
		return "_none"
	}
	return value
}

type endpointCollector struct {
	activeDesc *prometheus.Desc
	healthDesc *prometheus.Desc

	mu      sync.RWMutex
	sources []EndpointSource
}

func newEndpointCollector() *endpointCollector {
	labels := []string{"upstream_id", "endpoint_id"}
	return &endpointCollector{
		activeDesc: prometheus.NewDesc(
			"gateway_upstream_active_requests",
			"Current active requests read directly from endpoint atomic state.",
			labels,
			nil,
		),
		healthDesc: prometheus.NewDesc(
			"gateway_upstream_health",
			"Current endpoint health state (1 healthy, 0 unhealthy).",
			labels,
			nil,
		),
	}
}

func (c *endpointCollector) bind(sources []EndpointSource) error {
	seen := make(map[string]struct{}, len(sources))
	copied := make([]EndpointSource, 0, len(sources))
	for index, source := range sources {
		if source.UpstreamID == "" || source.EndpointID == "" || source.State == nil {
			return fmt.Errorf("绑定 endpoint 指标失败: sources[%d] 身份或状态为空", index)
		}
		key := source.UpstreamID + "\x00" + source.EndpointID
		if _, exists := seen[key]; exists {
			return fmt.Errorf("绑定 endpoint 指标失败: endpoint %q/%q 重复", source.UpstreamID, source.EndpointID)
		}
		seen[key] = struct{}{}
		copied = append(copied, source)
	}

	c.mu.Lock()
	c.sources = copied
	c.mu.Unlock()
	return nil
}

func (c *endpointCollector) Describe(descriptions chan<- *prometheus.Desc) {
	descriptions <- c.activeDesc
	descriptions <- c.healthDesc
}

func (c *endpointCollector) Collect(metrics chan<- prometheus.Metric) {
	c.mu.RLock()
	sources := append([]EndpointSource(nil), c.sources...)
	c.mu.RUnlock()

	for _, source := range sources {
		metrics <- prometheus.MustNewConstMetric(
			c.activeDesc,
			prometheus.GaugeValue,
			float64(source.State.ActiveRequests()),
			source.UpstreamID,
			source.EndpointID,
		)
		health := 0.0
		if source.State.Healthy() {
			health = 1
		}
		metrics <- prometheus.MustNewConstMetric(
			c.healthDesc,
			prometheus.GaugeValue,
			health,
			source.UpstreamID,
			source.EndpointID,
		)
	}
}
