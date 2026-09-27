package router_test

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"

	router "github.com/helantianshen/gateway/internal/router"
)

// TestDiff_RandomRoutes 验证 Radix Tree 和参考 matcher 在随机生成的路由配置和
// 请求路径上产生相同的匹配结果。固定随机种子确保可复现
//
// 测试分两阶段
// 1. 仅路径匹配（固定 host="" 和 method="GET"，隔离 path 逻辑）
// 2. 完整匹配（含 host、method、HEAD 回退）
func TestDiff_RandomRoutes(t *testing.T) {
	rng := rand.New(rand.NewSource(42))

	// 阶段 1：仅路径匹配
	t.Run("path_only", func(t *testing.T) {
		for iter := 0; iter < 300; iter++ {
			routes := generateRandomRoutesPathOnly(rng, 3+rng.Intn(8))

			radix, err := router.Compile(routes)
			if err != nil {
				continue
			}
			ref, err := newReferenceMatcher(routes)
			if err != nil {
				t.Fatalf("参考 matcher 构建失败: %v", err)
			}

			for reqIter := 0; reqIter < 10; reqIter++ {
				segs := randomSegments(rng, 1+rng.Intn(4))

				radixResult, _ := radix.Match("", "GET", segs)
				refResult, refErr := ref.Match("", "GET", segs)

				radixMatched := radixResult != nil
				refMatched := refResult != nil && refErr == nil

				if radixMatched != refMatched {
					t.Errorf("迭代 %d: 匹配不一致 (radix=%v, ref=%v)\n  routes=%v\n  segs=%v",
						iter, radixMatched, refMatched, routes, segs)
					continue
				}
				if radixMatched && refMatched {
					if radixResult.RouteID != refResult.RouteID {
						t.Errorf("迭代 %d: RouteID 不一致 (radix=%q, ref=%q)\n  routes=%v\n  segs=%v",
							iter, radixResult.RouteID, refResult.RouteID, routes, segs)
					}
					if radixResult.PathTemplate != refResult.PathTemplate {
						t.Errorf("迭代 %d: PathTemplate 不一致 (radix=%q, ref=%q)",
							iter, radixResult.PathTemplate, refResult.PathTemplate)
					}
				}
			}
		}
	})

	// 阶段 2：完整匹配（含 host、method、HEAD 回退）
	t.Run("full", func(t *testing.T) {
		for iter := 0; iter < 200; iter++ {
			routes := generateRandomRoutesFull(rng, 3+rng.Intn(8))

			radix, err := router.Compile(routes)
			if err != nil {
				continue
			}
			ref, err := newReferenceMatcher(routes)
			if err != nil {
				t.Fatalf("参考 matcher 构建失败: %v", err)
			}

			for reqIter := 0; reqIter < 10; reqIter++ {
				host := randomHost(rng)
				method := randomRequestMethod(rng)
				segs := randomSegments(rng, 1+rng.Intn(4))

				radixResult, _ := radix.Match(host, method, segs)
				refResult, refErr := ref.Match(host, method, segs)

				radixMatched := radixResult != nil
				refMatched := refResult != nil && refErr == nil

				if radixMatched != refMatched {
					t.Errorf("迭代 %d: 匹配不一致 (radix=%v, ref=%v)\n  host=%q method=%q segs=%v",
						iter, radixMatched, refMatched, host, method, segs)
					continue
				}
				if radixMatched && refMatched {
					if radixResult.RouteID != refResult.RouteID {
						t.Errorf("迭代 %d: RouteID 不一致 (radix=%q, ref=%q)\n  host=%q method=%q segs=%v",
							iter, radixResult.RouteID, refResult.RouteID, host, method, segs)
					}
					if radixResult.PathTemplate != refResult.PathTemplate {
						t.Errorf("迭代 %d: PathTemplate 不一致 (radix=%q, ref=%q)",
							iter, radixResult.PathTemplate, refResult.PathTemplate)
					}
				}
			}
		}
	})
}

// TestDiff_InsertionOrderIndependence 验证同一组路由以不同顺序编译后匹配结果一致
func TestDiff_InsertionOrderIndependence(t *testing.T) {
	rng := rand.New(rand.NewSource(999))
	for iter := 0; iter < 100; iter++ {
		routes := generateRandomRoutesFull(rng, 5+rng.Intn(10))
		shuffled := shuffleRoutes(rng, routes)

		rA, errA := router.Compile(routes)
		rB, errB := router.Compile(shuffled)
		if errA != nil || errB != nil {
			continue
		}

		for reqIter := 0; reqIter < 10; reqIter++ {
			host := randomHost(rng)
			method := randomRequestMethod(rng)
			segs := randomSegments(rng, 1+rng.Intn(4))

			resA, _ := rA.Match(host, method, segs)
			resB, _ := rB.Match(host, method, segs)

			aMatched := resA != nil
			bMatched := resB != nil
			if aMatched != bMatched {
				t.Errorf("迭代 %d: 插入顺序影响匹配 (A=%v, B=%v)\n  host=%q method=%q segs=%v",
					iter, aMatched, bMatched, host, method, segs)
				continue
			}
			if aMatched && resA.RouteID != resB.RouteID {
				t.Errorf("迭代 %d: 插入顺序影响 RouteID (A=%q, B=%q)\n  host=%q method=%q segs=%v",
					iter, resA.RouteID, resB.RouteID, host, method, segs)
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
			PreserveHost: false,
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
	methods := []string{"GET", "POST", "HEAD"}
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
