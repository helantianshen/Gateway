package balancer

import (
	"fmt"
	"testing"
)

func BenchmarkRoundRobin(b *testing.B) {
	for _, count := range []int{1, 10, 100} {
		b.Run(fmt.Sprintf("healthy_%d", count), func(b *testing.B) {
			var rr RoundRobin
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_, _ = rr.Select(count, func(int) bool { return true })
			}
		})
	}

	b.Run("sparse_healthy_100", func(b *testing.B) {
		var rr RoundRobin
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			_, _ = rr.Select(100, func(index int) bool { return index == 99 })
		}
	})
}

func BenchmarkRoundRobinParallel(b *testing.B) {
	var rr RoundRobin
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_, _ = rr.Select(10, func(int) bool { return true })
		}
	})
}
