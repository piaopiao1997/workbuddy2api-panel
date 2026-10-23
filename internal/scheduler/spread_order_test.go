package scheduler

import (
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/cst"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

// TestSpreadOrderedAvoidsTailPileup 复现 2026-10-23 的「假摊开」，并验证 ordered() 修复。
//
// wait() 只能往后等（sleepCtx 对 d<=0 立即放行），所以实际执行时刻是「前面所有偏移
// 的运行最大值」。按 UID 顺序遍历时，前几个号的随机偏移就能把时钟顶到窗口尾部，其余
// 账号的偏移全成过去、被一次性放行 —— 实测 340 号：前 37 分钟只签 5 个，最后 11 分钟
// 挤了 335 个。ordered() 按偏移升序重排后，运行最大值恒等于当前偏移，窗口才是真摊开。
func TestSpreadOrderedAvoidsTailPileup(t *testing.T) {
	// 造 340 个形如真实 uid 的账号（8 位 hex），并按 UID 升序给出池顺序 —— 与线上
	// Pool.List() 的顺序一致。
	uids := make([]string, 0, 340)
	for i := 0; i < 340; i++ {
		uids = append(uids, fmt.Sprintf("%08x", i*7919))
	}
	sort.Strings(uids)

	window := 40 * time.Minute
	now := time.Date(2026, 10, 23, 8, 0, 0, 0, cst.Loc)
	plan := newAccountSpread(taskCheckin, uids, window, time.Second, now)
	if plan == nil {
		t.Fatal("窗口 > 0 应生成计划")
	}

	list := make([]pool.Status, len(uids))
	for i, uid := range uids {
		list[i] = pool.Status{UID: uid}
	}

	// simulate 按给定顺序遍历，返回「运行最大值推进到的时刻」与前 3/4 窗口内完成的条数。
	simulate := func(order []pool.Status) (tail time.Duration, early int) {
		clock := time.Duration(0)
		for _, st := range order {
			off := plan.at[st.UID].Sub(now)
			if off > clock {
				clock = off
			}
			if clock < window*3/4 {
				early++
			}
		}
		return clock, early
	}

	oldTail, oldEarly := simulate(list)
	newTail, newEarly := simulate(plan.ordered(list))

	// 旧行为：前几个号的随机偏移就把时钟顶到窗口后段，绝大多数账号堆在尾部。
	if oldTail < window*3/4 {
		t.Fatalf("旧遍历顺序的时钟只推进到 %v（窗口 %v），复现前提不成立", oldTail, window)
	}
	if oldEarly > len(list)/4 {
		t.Fatalf("旧遍历顺序意外地均匀（前 3/4 窗口 %d 条），复现前提不成立", oldEarly)
	}

	// 新行为：运行最大值恒等于当前偏移 —— 不越界、且铺满整个窗口。
	if newTail >= window {
		t.Fatalf("新顺序尾部 %v 越出窗口 %v", newTail, window)
	}
	if newEarly < len(list)/2 {
		t.Fatalf("新顺序仍有堆积：前 3/4 窗口只有 %d/%d 条", newEarly, len(list))
	}
	if newEarly <= oldEarly {
		t.Fatalf("新顺序未改善：early %d vs 旧 %d", newEarly, oldEarly)
	}
}

// TestSpreadOrderedKeepsPlanOutsidersLast 计划外账号（本轮新加入）排最后，且不丢、不重。
func TestSpreadOrderedKeepsPlanOutsidersLast(t *testing.T) {
	now := time.Date(2026, 10, 23, 8, 0, 0, 0, cst.Loc)
	plan := newAccountSpread(taskCheckin, []string{"a", "b", "c"}, 30*time.Minute, 0, now)

	list := []pool.Status{{UID: "new1"}, {UID: "a"}, {UID: "new2"}, {UID: "b"}, {UID: "c"}}
	got := plan.ordered(list)

	if len(got) != len(list) {
		t.Fatalf("长度变了：%d → %d", len(list), len(got))
	}
	seen := make(map[string]int, len(got))
	for _, st := range got {
		seen[st.UID]++
	}
	for _, uid := range []string{"new1", "a", "new2", "b", "c"} {
		if seen[uid] != 1 {
			t.Fatalf("%s 出现 %d 次（应为 1）", uid, seen[uid])
		}
	}
	// 计划外账号必须落在末尾，且保持原有相对顺序。
	if got[3].UID != "new1" || got[4].UID != "new2" {
		t.Fatalf("计划外账号未排在末尾或顺序被打乱：%v %v", got[3].UID, got[4].UID)
	}
	// 计划内账号按偏移升序。
	if !plan.at[got[0].UID].Before(plan.at[got[1].UID]) && !plan.at[got[0].UID].Equal(plan.at[got[1].UID]) {
		t.Fatalf("计划内账号未按偏移升序")
	}
	if plan.at[got[1].UID].After(plan.at[got[2].UID]) {
		t.Fatalf("计划内账号未按偏移升序")
	}
	// 原切片不得被就地修改。
	if list[0].UID != "new1" || list[1].UID != "a" {
		t.Fatalf("ordered 就地修改了入参")
	}
}

// TestSpreadOrderedNilPlan nil 计划（手动触发 / 窗口关闭）原样返回，不排序、不复制。
func TestSpreadOrderedNilPlan(t *testing.T) {
	var plan *accountSpread
	list := []pool.Status{{UID: "c"}, {UID: "a"}, {UID: "b"}}
	got := plan.ordered(list)
	if len(got) != 3 || got[0].UID != "c" || got[2].UID != "b" {
		t.Fatalf("nil 计划应原样返回：%v", got)
	}
}
