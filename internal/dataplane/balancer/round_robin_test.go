package balancer

import (
	"sync"
	"sync/atomic"
	"testing"
)

func TestRoundRobinDeterministicSequence(t *testing.T) {
	var rr RoundRobin
	available := func(int) bool { return true }
	want := []int{0, 1, 2, 0, 1, 2, 0, 1, 2}
	for i, expected := range want {
		got, ok := rr.Select(3, available)
		if !ok || got != expected {
			t.Fatalf("第 %d 次选择 = (%d, %v), want (%d, true)", i, got, ok, expected)
		}
	}
}

func TestRoundRobinSkipsUnavailableAndRecovers(t *testing.T) {
	var rr RoundRobin
	healthy := []bool{true, false, true}
	available := func(index int) bool { return healthy[index] }

	for i, expected := range []int{0, 2, 0, 2, 0, 2} {
		got, ok := rr.Select(len(healthy), available)
		if !ok || got != expected {
			t.Fatalf("跳过不可用节点：第 %d 次 = (%d, %v), want (%d, true)", i, got, ok, expected)
		}
	}

	healthy[1] = true
	for i, expected := range []int{0, 1, 2, 0, 1, 2} {
		got, ok := rr.Select(len(healthy), available)
		if !ok || got != expected {
			t.Fatalf("节点恢复后：第 %d 次 = (%d, %v), want (%d, true)", i, got, ok, expected)
		}
	}
}

func TestRoundRobinNoAvailableCandidate(t *testing.T) {
	var rr RoundRobin
	for _, tc := range []struct {
		name      string
		count     int
		available func(int) bool
	}{
		{name: "零候选", count: 0, available: func(int) bool { return true }},
		{name: "负候选", count: -1, available: func(int) bool { return true }},
		{name: "nil 回调", count: 3, available: nil},
		{name: "全部不可用", count: 3, available: func(int) bool { return false }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			index, ok := rr.Select(tc.count, tc.available)
			if ok || index != -1 {
				t.Fatalf("Select = (%d, %v), want (-1, false)", index, ok)
			}
		})
	}
}

func TestRoundRobinConcurrentDistribution(t *testing.T) {
	var rr RoundRobin
	const endpointCount = 3
	const workers = 32
	const selectsPerWorker = 300

	var counts [endpointCount]atomic.Int64
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < selectsPerWorker; j++ {
				index, ok := rr.Select(endpointCount, func(int) bool { return true })
				if !ok {
					t.Errorf("并发选择意外返回无候选")
					return
				}
				counts[index].Add(1)
			}
		}()
	}
	wg.Wait()

	want := int64(workers * selectsPerWorker / endpointCount)
	for index := range counts {
		if got := counts[index].Load(); got != want {
			t.Errorf("endpoint %d 选择次数 = %d, want %d", index, got, want)
		}
	}
}
