// 账号移除前的取证备份（2026-10-20 实案）。
//
// 背景：老公在面板删了 15 个号，事后想复盘「这号当时是 disabled 还是健康、
// reason 写了什么」，发现 state.json 被就地改写、容器重建又把日志带走 ——
// 一点痕迹都没有，只能靠 8 天前本机的一份 auths 快照反推名单元。
//
// 本文件锁定四条行为：
//  1. BackupStateTo 真的落一份含当前状态（含 DisabledReason）的文件，权限 0600；
//  2. 同一秒内多次备份不互相覆盖（批量删号时会发生）；
//  3. 备份份数有上限（stateBackupKeep），超出按 mtime 删最旧；
//  4. 没配 state 路径时返回错误而不是 panic / 静默成功；reason 不得逃出目录。
package pool

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestBackupStateToWritesSnapshot 备份必须包含被删账号的禁用原因。
func TestBackupStateToWritesSnapshot(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	defer p.Close()

	p.Add(&auth.Auth{UID: "u-ban", Nickname: "banned"})
	p.Disable("u-ban", "account banned by upstream (11140 request illegal), re-login required")

	dst, err := p.BackupStateTo("remove")
	if err != nil {
		t.Fatalf("BackupStateTo: %v", err)
	}
	if !strings.HasPrefix(dst, fp+".bak-") {
		t.Fatalf("备份路径应以 %s.bak- 开头，got %s", fp, dst)
	}

	st, err := os.Stat(dst)
	if err != nil {
		t.Fatalf("备份文件不存在: %v", err)
	}
	if perm := st.Mode().Perm(); perm != 0o600 {
		t.Errorf("备份权限=%o want 600（含明文凭证）", perm)
	}

	raw, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	if !strings.Contains(body, "u-ban") {
		t.Error("备份里应包含被删账号")
	}
	if !strings.Contains(body, "11140 request illegal") {
		t.Error("备份必须保留 disabled_reason —— 这正是本次缺失的关键证据")
	}
}

// TestBackupStateToNoOverwriteInSameSecond 同一秒内连续备份必须各留一份。
func TestBackupStateToNoOverwriteInSameSecond(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	defer p.Close()
	p.Add(&auth.Auth{UID: "u1", Nickname: "n1"})

	seen := map[string]bool{}
	for i := 0; i < 5; i++ {
		dst, err := p.BackupStateTo("remove")
		if err != nil {
			t.Fatalf("第 %d 次备份失败: %v", i+1, err)
		}
		if seen[dst] {
			t.Fatalf("第 %d 次备份覆盖了同名文件: %s", i+1, dst)
		}
		seen[dst] = true
	}
	if len(seen) != 5 {
		t.Fatalf("应有 5 份独立备份，实际 %d", len(seen))
	}
}

// TestBackupStateToPrunesOldFiles 超过上限时按 mtime 删最旧，最新的必须保留。
func TestBackupStateToPrunesOldFiles(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	defer p.Close()
	p.Add(&auth.Auth{UID: "u1", Nickname: "n1"})

	// 手工造 stateBackupKeep+5 份「旧备份」，mtime 逐份递增（越往后越新）
	base := time.Now().Add(-time.Hour)
	for i := 0; i < stateBackupKeep+5; i++ {
		name := filepath.Join(dir, fmt.Sprintf("state.json.bak-20200101-0000%02d-manual", i))
		if err := os.WriteFile(name, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		mt := base.Add(time.Duration(i) * time.Minute)
		if err := os.Chtimes(name, mt, mt); err != nil {
			t.Fatal(err)
		}
	}
	pre, _ := filepath.Glob(fp + ".bak-*")
	if len(pre) != stateBackupKeep+5 {
		t.Fatalf("准备阶段份数=%d want %d", len(pre), stateBackupKeep+5)
	}

	// 一次备份触发 prune
	dst, err := p.BackupStateTo("remove")
	if err != nil {
		t.Fatalf("BackupStateTo: %v", err)
	}

	got, _ := filepath.Glob(fp + ".bak-*")
	if len(got) > stateBackupKeep {
		t.Fatalf("备份份数=%d 超过上限 %d", len(got), stateBackupKeep)
	}
	// 刚写的那份不能被自己删掉
	found := false
	for _, g := range got {
		if g == dst {
			found = true
		}
	}
	if !found {
		t.Fatalf("最新备份 %s 被误删", dst)
	}
	// 最旧的两份应该被删掉
	for i := 0; i < 2; i++ {
		old := filepath.Join(dir, fmt.Sprintf("state.json.bak-20200101-0000%02d-manual", i))
		if _, err := os.Stat(old); !os.IsNotExist(err) {
			t.Errorf("最旧的备份 %s 应被清理", filepath.Base(old))
		}
	}
}

// TestBackupStateToNoStateFile 未配置 state 路径时必须返回错误（而不是静默成功）。
func TestBackupStateToNoStateFile(t *testing.T) {
	p := New("")
	defer p.Close()
	if _, err := p.BackupStateTo("remove"); err == nil {
		t.Fatal("没有 state 路径时应返回错误")
	}
}

// TestBackupStateToSanitizesReason reason 里带路径分隔符等字符时不得逃出目录。
func TestBackupStateToSanitizesReason(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	defer p.Close()
	p.Add(&auth.Auth{UID: "u1", Nickname: "n1"})

	dst, err := p.BackupStateTo("../../evil/../x")
	if err != nil {
		t.Fatalf("BackupStateTo: %v", err)
	}
	if filepath.Dir(dst) != dir {
		t.Fatalf("备份跑出了目标目录: %s（期望目录 %s）", dst, dir)
	}
	if strings.Contains(filepath.Base(dst), "..") || strings.ContainsAny(filepath.Base(dst), `/\`) {
		t.Fatalf("reason 未被过滤干净: %s", filepath.Base(dst))
	}
}
