# 2026-09-27 专项复现证据

对应 [审查报告](../../docs/10-current-architecture-review.md)。基线与环境见报告。以下是期望正确行为的探针，基线故意出现失败；不属于仓库现有测试集。没有修改生产代码。

## 重跑

在仓库根目录，将下方 Go 代码块保存为 `.agent/review_probe_test.go`，执行 `go test -v -count=3 ./.agent`，完成后仅删除该临时文件。不要覆盖已有同名文件；不要与其他修改全局标准 logger 的测试并行执行。点目录通常不被 `go test ./...` 枚举，因此必须显式传入路径。

R1 的 20ms/300ms 是复现参数，不是性能基准。R2 的 sentinel 是人工构造内容，无真实敏感数据。

## 探针源码

```go
package review_test

import (
	"bufio"
	"bytes"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/helantianshen/gateway/internal/config"
	"github.com/helantianshen/gateway/internal/dataplane/middleware"
	"github.com/helantianshen/gateway/internal/dataplane/proxy"
	"github.com/helantianshen/gateway/internal/dataplane/requestctx"
	"github.com/helantianshen/gateway/internal/dataplane/server"
	"github.com/helantianshen/gateway/internal/dataplane/transport"
)

type probeObserver struct{ finished chan requestctx.Snapshot }

func (o *probeObserver) RequestStarted() {}
func (o *probeObserver) RequestFinished(_ string, s requestctx.Snapshot, _ time.Duration) {
	o.finished <- s
}

func TestReviewInformationalStatus(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(103)
		w.WriteHeader(502)
	}))
	defer upstream.Close()
	target, _ := url.Parse(upstream.URL)
	observer := &probeObserver{finished: make(chan requestctx.Snapshot, 1)}
	gateway := newProbeServer(middleware.NewPublicHandler(newProbeProxy(t, target, time.Second), middleware.Options{Observer: observer}))
	defer gateway.Close()
	resp, err := gateway.Client().Get(gateway.URL)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	snapshot := <-observer.finished
	t.Logf("client=%d observed=%d request_id=%q", resp.StatusCode, snapshot.ResponseStatus, resp.Header.Get("X-Request-ID"))
	if snapshot.ResponseStatus != resp.StatusCode {
		t.Errorf("final status mismatch")
	}
	if resp.Header.Get("X-Request-ID") == "" {
		t.Errorf("final response lost request ID")
	}
}

func TestReviewConnectionRequestID(t *testing.T) {
	ids := make(chan string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { ids <- r.Header.Get("X-Request-ID"); w.WriteHeader(204) }))
	defer upstream.Close()
	target, _ := url.Parse(upstream.URL)
	gateway := newProbeServer(middleware.NewPublicHandler(newProbeProxy(t, target, time.Second), middleware.Options{}))
	defer gateway.Close()
	req, _ := http.NewRequest("GET", gateway.URL, nil)
	req.Header.Set("Connection", "X-Request-ID")
	req.Header.Set("X-Request-ID", "review-id")
	resp, err := gateway.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	got := <-ids
	t.Logf("response ID=%q upstream ID=%q", resp.Header.Get("X-Request-ID"), got)
	if got != "review-id" {
		t.Errorf("upstream request ID lost")
	}
}

func TestReviewYAMLMergeWeight(t *testing.T) {
	file := filepath.Join(t.TempDir(), "config.yaml")
	content := `api_version: v1
upstreams:
  - id: u
    endpoints:
      - <<: &base {weight: 0}
        id: a
        url: http://127.0.0.1:18080
routes:
  - id: r
    path: /
    upstream: u
policies:
  request_timeout: 3s
`
	if err := os.WriteFile(file, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	spec, err := config.LoadConfig(file)
	if err != nil {
		t.Fatal(err)
	}
	got := spec.Upstreams[0].Endpoints[0].Weight
	t.Logf("merged explicit weight=0 decoded weight=%d validate=%v", got, config.Validate(spec, file))
	if got != 0 {
		t.Errorf("explicit merged weight overwritten")
	}
}

func TestReviewProxyErrorLog(t *testing.T) {
	var captured bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&captured)
	defer log.SetOutput(previous)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		rw.WriteString("HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n1\r\nx\r\n0\r\nREVIEW_SECRET_SENTINEL\r\n\r\n")
		rw.Flush()
	}))
	defer upstream.Close()
	target, _ := url.Parse(upstream.URL)
	observer := &probeObserver{finished: make(chan requestctx.Snapshot, 1)}
	gateway := newProbeServer(middleware.NewPublicHandler(newProbeProxy(t, target, time.Second), middleware.Options{Observer: observer}))
	defer gateway.Close()
	resp, err := gateway.Client().Get(gateway.URL)
	if err == nil {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	<-observer.finished
	t.Logf("standard logger output=%q", captured.String())
	if strings.Contains(captured.String(), "REVIEW_SECRET_SENTINEL") {
		t.Error("raw upstream data leaked to standard logger")
	}
}

func TestReviewStalledUpload(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.Copy(io.Discard, r.Body) }))
	defer upstream.Close()
	target, _ := url.Parse(upstream.URL)
	gateway := newProbeServer(middleware.NewPublicHandler(newProbeProxy(t, target, 20*time.Millisecond), middleware.Options{}))
	defer gateway.Close()
	conn, err := net.Dial("tcp", strings.TrimPrefix(gateway.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(300 * time.Millisecond))
	io.WriteString(conn, "POST / HTTP/1.1\r\nHost: gateway.local\r\nContent-Length: 100\r\n\r\nx")
	response, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("20ms timeout did not complete within 300ms: %v", err)
	}
	response.Body.Close()
	t.Logf("status=%d", response.StatusCode)
}

func newProbeProxy(t *testing.T, target *url.URL, timeout time.Duration) *proxy.Proxy {
	tr := transport.New()
	t.Cleanup(tr.CloseIdleConnections)
	return proxy.New(target, tr, timeout, false)
}

func newProbeServer(handler http.Handler) *httptest.Server {
	s := httptest.NewUnstartedServer(handler)
	s.Config = server.NewPublicServer(handler)
	s.Start()
	return s
}
```

## 实际运行输出

```text
=== RUN   TestReviewInformationalStatus
    review_probe_test.go:50: client=502 observed=103 request_id=""
    review_probe_test.go:52: final status mismatch
    review_probe_test.go:55: final response lost request ID
--- FAIL: TestReviewInformationalStatus (0.00s)
=== RUN   TestReviewConnectionRequestID
    review_probe_test.go:75: response ID="review-id" upstream ID=""
    review_probe_test.go:77: upstream request ID lost
--- FAIL: TestReviewConnectionRequestID (0.00s)
=== RUN   TestReviewYAMLMergeWeight
    review_probe_test.go:105: merged explicit weight=0 decoded weight=100 validate=<nil>
    review_probe_test.go:107: explicit merged weight overwritten
--- FAIL: TestReviewYAMLMergeWeight (0.00s)
=== RUN   TestReviewProxyErrorLog
    review_probe_test.go:136: standard logger output="2026/09/27 16:11:20 httputil: ReverseProxy read error during body copy: malformed MIME header: missing colon: \"REVIEW_SECRET_SENTINEL\"\n"
    review_probe_test.go:138: raw upstream data leaked to standard logger
--- FAIL: TestReviewProxyErrorLog (0.00s)
=== RUN   TestReviewStalledUpload
    review_probe_test.go:157: 20ms timeout did not complete within 300ms: read tcp 127.0.0.1:35084->127.0.0.1:35873: i/o timeout
--- FAIL: TestReviewStalledUpload (0.30s)
=== RUN   TestReviewInformationalStatus
    review_probe_test.go:50: client=502 observed=103 request_id=""
    review_probe_test.go:52: final status mismatch
    review_probe_test.go:55: final response lost request ID
--- FAIL: TestReviewInformationalStatus (0.00s)
=== RUN   TestReviewConnectionRequestID
    review_probe_test.go:75: response ID="review-id" upstream ID=""
    review_probe_test.go:77: upstream request ID lost
--- FAIL: TestReviewConnectionRequestID (0.00s)
=== RUN   TestReviewYAMLMergeWeight
    review_probe_test.go:105: merged explicit weight=0 decoded weight=100 validate=<nil>
    review_probe_test.go:107: explicit merged weight overwritten
--- FAIL: TestReviewYAMLMergeWeight (0.00s)
=== RUN   TestReviewProxyErrorLog
    review_probe_test.go:136: standard logger output="2026/09/27 16:11:21 httputil: ReverseProxy read error during body copy: malformed MIME header: missing colon: \"REVIEW_SECRET_SENTINEL\"\n"
    review_probe_test.go:138: raw upstream data leaked to standard logger
--- FAIL: TestReviewProxyErrorLog (0.00s)
=== RUN   TestReviewStalledUpload
    review_probe_test.go:157: 20ms timeout did not complete within 300ms: read tcp 127.0.0.1:50452->127.0.0.1:40987: i/o timeout
--- FAIL: TestReviewStalledUpload (0.30s)
=== RUN   TestReviewInformationalStatus
    review_probe_test.go:50: client=502 observed=103 request_id=""
    review_probe_test.go:52: final status mismatch
    review_probe_test.go:55: final response lost request ID
--- FAIL: TestReviewInformationalStatus (0.00s)
=== RUN   TestReviewConnectionRequestID
    review_probe_test.go:75: response ID="review-id" upstream ID=""
    review_probe_test.go:77: upstream request ID lost
--- FAIL: TestReviewConnectionRequestID (0.00s)
=== RUN   TestReviewYAMLMergeWeight
    review_probe_test.go:105: merged explicit weight=0 decoded weight=100 validate=<nil>
    review_probe_test.go:107: explicit merged weight overwritten
--- FAIL: TestReviewYAMLMergeWeight (0.00s)
=== RUN   TestReviewProxyErrorLog
    review_probe_test.go:136: standard logger output="2026/09/27 16:11:21 httputil: ReverseProxy read error during body copy: malformed MIME header: missing colon: \"REVIEW_SECRET_SENTINEL\"\n"
    review_probe_test.go:138: raw upstream data leaked to standard logger
--- FAIL: TestReviewProxyErrorLog (0.00s)
=== RUN   TestReviewStalledUpload
    review_probe_test.go:157: 20ms timeout did not complete within 300ms: read tcp 127.0.0.1:37066->127.0.0.1:46259: i/o timeout
--- FAIL: TestReviewStalledUpload (0.30s)
FAIL
FAIL	github.com/helantianshen/gateway/.agent	0.913s
FAIL
```
