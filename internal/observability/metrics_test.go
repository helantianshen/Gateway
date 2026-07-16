package observability

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"go.uber.org/zap"

	"github.com/helantianshen/gateway/internal/dataplane/requestctx"
)

func TestMetricsUsesPrivateRepeatableRegistry(t *testing.T) {
	first, err := NewMetrics(10, DefaultRouteSeriesBudget)
	if err != nil {
		t.Fatalf("首次 NewMetrics: %v", err)
	}
	second, err := NewMetrics(10, DefaultRouteSeriesBudget)
	if err != nil {
		t.Fatalf("第二次 NewMetrics: %v", err)
	}
	if first.Registry() == second.Registry() {
		t.Fatal("两个 Metrics 共享了 Registry")
	}
	if !first.RouteDetailsEnabled() || !second.RouteDetailsEnabled() {
		t.Fatal("预算内 route metrics 被禁用")
	}
}

func TestMetricsObservesRequestAndNormalizesLabels(t *testing.T) {
	metrics, err := NewMetrics(2, DefaultRouteSeriesBudget)
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}
	metrics.RequestStarted()
	if got := testutil.ToFloat64(metrics.inflightRequests); got != 1 {
		t.Fatalf("inflight started = %v, want 1", got)
	}
	metrics.RequestFinished("CUSTOM-UNBOUNDED-METHOD", requestctx.Snapshot{
		RouteID:          "users",
		PathTemplate:     "/users/:id",
		UpstreamID:       "user-service",
		EndpointID:       "user-1",
		Attempts:         1,
		ErrorKind:        "BAD_GATEWAY",
		Outcome:          "gateway_error",
		ResponseStatus:   http.StatusBadGateway,
		UpstreamDuration: 5 * time.Millisecond,
	}, 10*time.Millisecond)
	if got := testutil.ToFloat64(metrics.inflightRequests); got != 0 {
		t.Fatalf("inflight finished = %v, want 0", got)
	}

	body := scrapeMetrics(t, metrics)
	for _, want := range []string{
		`gateway_requests_total{error_kind="BAD_GATEWAY",method="_OTHER",status_class="5xx",upstream_id="user-service"} 1`,
		`gateway_rejections_total{code="BAD_GATEWAY"} 1`,
		`gateway_route_requests_total{path_template="/users/:id",route_id="users",status_class="5xx"} 1`,
		`gateway_upstream_requests_total{endpoint_id="user-1",error_kind="BAD_GATEWAY",outcome="gateway_error",upstream_id="user-service"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics 缺少 %q\n%s", want, body)
		}
	}
	for _, forbidden := range []string{"request_id", "/users/42", "CUSTOM-UNBOUNDED-METHOD"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("metrics 包含禁止值 %q", forbidden)
		}
	}
}

func TestMetricsRecordsPreWriteAbortWithoutSuccessStatus(t *testing.T) {
	metrics, err := NewMetrics(1, DefaultRouteSeriesBudget)
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}
	metrics.RequestStarted()
	metrics.RequestFinished(http.MethodGet, requestctx.Snapshot{
		RouteID:        "aborted-route",
		PathTemplate:   "/stream",
		ErrorKind:      "RESPONSE_ABORTED",
		Outcome:        "aborted",
		ResponseStatus: 0,
	}, time.Millisecond)

	body := scrapeMetrics(t, metrics)
	for _, want := range []string{
		`gateway_requests_total{error_kind="RESPONSE_ABORTED",method="GET",status_class="_none",upstream_id="_none"} 1`,
		`gateway_route_requests_total{path_template="/stream",route_id="aborted-route",status_class="_none"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("pre-write abort metrics 缺少 %q\n%s", want, body)
		}
	}
	if strings.Contains(body, `gateway_rejections_total{code="RESPONSE_ABORTED"}`) {
		t.Fatalf("pre-write abort 被错误计为 Gateway rejection:\n%s", body)
	}
}

func TestMetricsAggregatesRoutesOverBudget(t *testing.T) {
	metrics, err := NewMetrics(1001, 1000)
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}
	if metrics.RouteDetailsEnabled() {
		t.Fatal("超预算 route metrics 仍启用明细")
	}
	metrics.RequestStarted()
	metrics.RequestFinished(http.MethodGet, requestctx.Snapshot{
		RouteID:        "high-cardinality-route",
		PathTemplate:   "/configured/template",
		ResponseStatus: http.StatusOK,
		Outcome:        "success",
	}, time.Millisecond)

	body := scrapeMetrics(t, metrics)
	if !strings.Contains(body, `gateway_route_requests_total{path_template="_other",route_id="_other",status_class="2xx"} 1`) {
		t.Fatalf("超预算 route 未聚合到 _other:\n%s", body)
	}
	if strings.Contains(body, "high-cardinality-route") || strings.Contains(body, "/configured/template") {
		t.Fatalf("超预算 route 泄露明细 label:\n%s", body)
	}
}

func TestEndpointCollectorReadsAtomicStateAtScrapeTime(t *testing.T) {
	metrics, err := NewMetrics(1, 1000)
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}
	state := &fakeEndpointState{}
	state.healthy.Store(true)
	state.active.Store(3)
	if err := metrics.BindEndpoints([]EndpointSource{{UpstreamID: "api", EndpointID: "api-1", State: state}}); err != nil {
		t.Fatalf("BindEndpoints: %v", err)
	}

	body := scrapeMetrics(t, metrics)
	if !strings.Contains(body, `gateway_upstream_active_requests{endpoint_id="api-1",upstream_id="api"} 3`) ||
		!strings.Contains(body, `gateway_upstream_health{endpoint_id="api-1",upstream_id="api"} 1`) {
		t.Fatalf("初始 endpoint 指标错误:\n%s", body)
	}

	state.healthy.Store(false)
	state.active.Store(0)
	body = scrapeMetrics(t, metrics)
	if !strings.Contains(body, `gateway_upstream_active_requests{endpoint_id="api-1",upstream_id="api"} 0`) ||
		!strings.Contains(body, `gateway_upstream_health{endpoint_id="api-1",upstream_id="api"} 0`) {
		t.Fatalf("更新后 endpoint 指标错误:\n%s", body)
	}
}

func TestBindEndpointsRejectsInvalidSources(t *testing.T) {
	metrics, err := NewMetrics(1, 1000)
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}
	state := &fakeEndpointState{}
	for _, sources := range [][]EndpointSource{
		{{UpstreamID: "", EndpointID: "e", State: state}},
		{{UpstreamID: "u", EndpointID: "", State: state}},
		{{UpstreamID: "u", EndpointID: "e", State: nil}},
		{
			{UpstreamID: "u", EndpointID: "e", State: state},
			{UpstreamID: "u", EndpointID: "e", State: state},
		},
	} {
		if err := metrics.BindEndpoints(sources); err == nil {
			t.Fatalf("非法 sources 未返回错误: %+v", sources)
		}
	}
}

func TestNewProductionLogger(t *testing.T) {
	logger, err := NewProductionLogger()
	if err != nil {
		t.Fatalf("NewProductionLogger: %v", err)
	}
	logger.Info("test", zap.String("field", "value"))
	if err := SyncLogger(logger); err != nil {
		t.Fatalf("SyncLogger: %v", err)
	}
	if err := SyncLogger(nil); err != nil {
		t.Fatalf("SyncLogger(nil): %v", err)
	}
}

func scrapeMetrics(t *testing.T, metrics *Metrics) string {
	t.Helper()
	recorder := httptest.NewRecorder()
	metrics.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "http://admin.local/metrics", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("metrics 状态码 = %d; body=%s", recorder.Code, recorder.Body.String())
	}
	return recorder.Body.String()
}

type fakeEndpointState struct {
	healthy atomic.Bool
	active  atomic.Int64
}

func (s *fakeEndpointState) Healthy() bool         { return s.healthy.Load() }
func (s *fakeEndpointState) ActiveRequests() int64 { return s.active.Load() }
