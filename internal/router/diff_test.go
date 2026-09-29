package router_test

import (
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"

	router "github.com/helantianshen/gateway/internal/router"
)

// TestDiff_RandomRoutes 比较独立编译器的冲突结论以及完整请求匹配结果
// 固定随机种子；路径模式、Host、Method、Params 和错误类型均参与比较
func TestDiff_RandomRoutes(t *testing.T) {
	for _, full := range []bool{false, true} {
		t.Run(fmt.Sprintf("full_%v", full), func(t *testing.T) {
			rng := rand.New(rand.NewSource(42))
			for range 300 {
				routes := generateRandomRoutesPathOnly(rng, 3+rng.Intn(8))
				if full {
					routes = generateRandomRoutesFull(rng, 3+rng.Intn(8))
				}
				radix, err := router.Compile(routes)
				ref, refErr := newReferenceMatcher(routes)
				if (err != nil) != (refErr != nil) {
					t.Fatalf("编译结果不同: routes=%+v production=%v reference=%v", routes, err, refErr)
				}
				if err != nil {
					continue
				}
				for range 10 {
					host, method := "", "GET"
					if full {
						host, method = randomHost(rng), randomRequestMethod(rng)
					}
					path := randomSegments(rng, 1+rng.Intn(4))
					got, gotErr := radix.Match(host, method, path)
					want, wantErr := ref.Match(host, method, path)
					assertReferenceResult(t, got, want, host, method, path)
					assertReferenceError(t, gotErr, wantErr)
				}
			}
		})
	}
}

// TestDiff_InsertionOrderIndependence 验证完整结果、参数和错误列表不依赖声明顺序
func TestDiff_InsertionOrderIndependence(t *testing.T) {
	rng := rand.New(rand.NewSource(999))
	for range 100 {
		routes := generateRandomRoutesFull(rng, 5+rng.Intn(10))
		a, errA := router.Compile(routes)
		b, errB := router.Compile(shuffleRoutes(rng, routes))
		if (errA != nil) != (errB != nil) {
			t.Fatal("插入顺序改变冲突结论")
		}
		if errA != nil {
			continue
		}
		for range 20 {
			host, method, path := randomHost(rng), randomRequestMethod(rng), randomSegments(rng, 1+rng.Intn(4))
			resultA, matchErrA := a.Match(host, method, path)
			resultB, matchErrB := b.Match(host, method, path)
			if !reflect.DeepEqual(resultA, resultB) || !reflect.DeepEqual(matchErrA, matchErrB) {
				t.Fatalf("插入顺序改变匹配: %+v/%+v vs %+v/%+v", resultA, matchErrA, resultB, matchErrB)
			}
		}
	}
}

func shuffleRoutes(rng *rand.Rand, routes []router.CompileInput) []router.CompileInput {
	result := make([]router.CompileInput, len(routes))
	copy(result, routes)
	rng.Shuffle(len(result), func(i, j int) {
		result[i], result[j] = result[j], result[i]
	})
	return result
}

// generateRandomRoutesPathOnly 生成仅含路径的路由（host="", method="GET"）
func generateRandomRoutesPathOnly(rng *rand.Rand, count int) []router.CompileInput {
	routes := make([]router.CompileInput, 0, count)
	for i := 0; i < count; i++ {
		routes = append(routes, router.CompileInput{
			RouteID:  fmt.Sprintf("r%d", i),
			Host:     "",
			Method:   "GET",
			Path:     randomPathPattern(rng),
			Upstream: "mock",
			Priority: rng.Intn(3),
		})
	}
	return routes
}

// generateRandomRoutesFull 生成包含 host 和 method 的路由
func generateRandomRoutesFull(rng *rand.Rand, count int) []router.CompileInput {
	routes := make([]router.CompileInput, 0, count)
	for i := 0; i < count; i++ {
		routes = append(routes, router.CompileInput{
			RouteID:      fmt.Sprintf("r%d", i),
			Host:         randomHostPattern(rng),
			Method:       randomRouteMethod(rng),
			Path:         randomPathPattern(rng),
			Upstream:     "mock",
			Priority:     rng.Intn(3),
			PreserveHost: rng.Intn(2) == 0,
		})
	}
	return routes
}

func randomHostPattern(rng *rand.Rand) string {
	switch rng.Intn(3) {
	case 0:
		return ""
	case 1:
		return fmt.Sprintf("%s.example.com", string([]byte{byte('a' + rng.Intn(3))}))
	default:
		return "*.example.com"
	}
}

func randomHost(rng *rand.Rand) string {
	switch rng.Intn(4) {
	case 0:
		return ""
	case 1:
		return "a.example.com"
	case 2:
		return "b.example.com"
	default:
		return "other.com"
	}
}

// randomRouteMethod 返回路由配置中的方法（含空表示任意 Method）
func randomRouteMethod(rng *rand.Rand) string {
	methods := []string{"", "GET", "POST", "HEAD"}
	return methods[rng.Intn(len(methods))]
}

// randomRequestMethod 返回请求方法（不含空，真实 HTTP 请求总有方法）
func randomRequestMethod(rng *rand.Rand) string {
	methods := []string{"GET", "POST", "HEAD", "OPTIONS", "DELETE", "PURGE"}
	return methods[rng.Intn(len(methods))]
}

func randomPathPattern(rng *rand.Rand) string {
	segCount := 1 + rng.Intn(3)
	segs := make([]string, segCount)
	for i := 0; i < segCount; i++ {
		switch rng.Intn(3) {
		case 0:
			segs[i] = fmt.Sprintf("s%d", rng.Intn(3))
		case 1:
			segs[i] = fmt.Sprintf(":p%d", i)
		default:
			if i == segCount-1 {
				segs[i] = "*catch"
			} else {
				segs[i] = fmt.Sprintf("s%d", rng.Intn(3))
			}
		}
	}
	return "/" + strings.Join(segs, "/")
}

func randomSegments(rng *rand.Rand, count int) []string {
	segs := make([]string, count)
	for i := 0; i < count; i++ {
		segs[i] = fmt.Sprintf("s%d", rng.Intn(3))
	}
	return segs
}
