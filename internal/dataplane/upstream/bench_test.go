package upstream

import (
	"fmt"
	"net/url"
	"testing"
	"time"
)

func BenchmarkCompiledUpstreamSelect(b *testing.B) {
	for _, count := range []int{1, 10, 100} {
		b.Run(fmt.Sprintf("healthy_%d", count), func(b *testing.B) {
			compiled := benchmarkCompiledUpstream(b, count)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_, _ = compiled.Select()
			}
		})
	}

	b.Run("sparse_healthy_100", func(b *testing.B) {
		compiled := benchmarkCompiledUpstream(b, 100)
		for index := 0; index < 99; index++ {
			endpoint, _ := compiled.Endpoint(fmt.Sprintf("endpoint-%d", index))
			endpoint.State().SetHealthy(false)
		}
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			_, _ = compiled.Select()
		}
	})
}

func BenchmarkCompiledUpstreamSelectParallel(b *testing.B) {
	compiled := benchmarkCompiledUpstream(b, 10)
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_, _ = compiled.Select()
		}
	})
}

func benchmarkCompiledUpstream(b *testing.B, count int) *CompiledUpstream {
	b.Helper()
	configs := make([]EndpointConfig, 0, count)
	for index := 0; index < count; index++ {
		configs = append(configs, EndpointConfig{
			ID: fmt.Sprintf("endpoint-%d", index),
			Target: url.URL{
				Scheme: "http",
				Host:   fmt.Sprintf("127.0.0.1:%d", 18080+index),
			},
			Weight: 1,
		})
	}
	compiled, err := NewCompiledUpstream("benchmark", configs, 0, nil, time.Second)
	if err != nil {
		b.Fatalf("NewCompiledUpstream: %v", err)
	}
	return compiled
}
