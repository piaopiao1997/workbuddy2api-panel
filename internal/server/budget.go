// budget.go 当日积分预算闸。缺省 limit<=0 不拦。
// 只加观测到的 usage.credit（缺失≠0）。按 CST 自然日惰性归零，进程内不落盘。
package server

import (
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/cst"
)

// cstDay 上游自然日（CST）日键，口径统一在 internal/cst。
func cstDay(t time.Time) string { return cst.Day(t) }

type dailyBudget struct {
	mu       sync.Mutex
	limit    float64
	day      string
	used     float64
	rejected int64
}

func newDailyBudget(limit float64) *dailyBudget {
	return &dailyBudget{limit: limit, day: cstDay(time.Now())}
}

func (b *dailyBudget) setLimit(limit float64) {
	if b == nil {
		return
	}
	b.mu.Lock()
	b.limit = limit
	b.mu.Unlock()
}

// admit 严格小于才放行。到顶的拒绝单独计数。
func (b *dailyBudget) admit() bool {
	if b == nil {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rollLocked()
	if b.limit <= 0 || b.used < b.limit {
		return true
	}
	b.rejected++
	return false
}

func (b *dailyBudget) add(credit float64, observed bool) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rollLocked()
	if observed && credit > 0 {
		b.used += credit
	}
}

func (b *dailyBudget) snapshot() (used, limit float64, rejected int64) {
	if b == nil {
		return 0, 0, 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rollLocked()
	return b.used, b.limit, b.rejected
}

// SetBudgetLimit 热改当日上限。<=0 关闭闸，已累计的 used 保留。
func (h *Handler) SetBudgetLimit(limit float64) {
	if h == nil || h.budget == nil {
		return
	}
	h.budget.setLimit(limit)
}

func (b *dailyBudget) rollLocked() {
	if today := cstDay(time.Now()); today != b.day {
		b.day = today
		b.used = 0
		b.rejected = 0
	}
}
