// Package balancer 提供数据面使用的确定性负载均衡算法。
package balancer

import "sync/atomic"

// RoundRobin 是并发安全的普通轮询选择器。
//
// cursor 通常使用单调 uint64 序号；接近回绕点时重基准到当前候选范围。Select 通过
// CAS 预留下一个实际可用的位置，因此多个并发请求不会同时消费同一个游标状态。
type RoundRobin struct {
	cursor atomic.Uint64
}

// Select 从 count 个候选中选择一个 available(index)==true 的索引。
//
// 返回值：
//   - 成功：候选索引和 true；
//   - count 非正、available 为空或全部候选不可用：-1 和 false。
//
// available 必须只读取并发安全状态，并且不能在回调中再次调用同一个 RoundRobin。
// 候选状态允许在 Select 返回后变化；调用方对已选候选执行一次尝试，不在本算法中重试。
func (r *RoundRobin) Select(count int, available func(index int) bool) (int, bool) {
	if count <= 0 || available == nil {
		return -1, false
	}

	candidateCount := uint64(count)
	for {
		start := r.cursor.Load()
		startIndex := start % candidateCount
		selectedIndex := -1
		selectedOffset := 0

		for offset := 0; offset < count; offset++ {
			index := int((startIndex + uint64(offset)) % candidateCount)
			if available(index) {
				selectedIndex = index
				selectedOffset = offset
				break
			}
		}
		if selectedIndex < 0 {
			return -1, false
		}

		// 游标推进到实际命中位置之后。CAS 失败表示其他 goroutine 已完成选择，
		// 必须基于新游标重新检查健康状态，不能返回旧选择。
		advance := uint64(selectedOffset) + 1
		next := start + advance
		if next < start {
			// uint64 回绕会丢失完整轮次对非 2 次幂 count 的模信息；只在此极端
			// 边界把游标重基准到数学意义上的下一个候选。
			next = (startIndex + advance) % candidateCount
		}
		if r.cursor.CompareAndSwap(start, next) {
			return selectedIndex, true
		}
	}
}
