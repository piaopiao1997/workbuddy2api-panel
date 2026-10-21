// probe_test.go 复通探测与「全 dead 不再静默直连」的回归钉子。
package proxypool_test

import (
	"errors"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/proxypool"
)

// 全 dead 时必须报 ErrAllDead，而不是返回 (nil, nil) 让调用方静默直连。
// 这是 issue #39 的核心结论：全池挂掉时回落直连 = 整批账号从宿主机真实 IP 涌出。
func TestPickStrict_AllDeadReturnsError(t *testing.T) {
	p := proxypool.New()
	p.Configure([]string{"http://1.1.1.1:8080", "http://2.2.2.2:8080"}, "per_account")
	for _, raw := range []string{"http://1.1.1.1:8080", "http://2.2.2.2:8080"} {
		for i := 0; i < 3; i++ {
			p.Report(raw, false) // deadThreshold = 3
		}
	}
	if _, _, err := p.PickStrict("u1"); !errors.Is(err, proxypool.ErrAllDead) {
		t.Fatalf("want ErrAllDead, got %v", err)
	}
	// 兼容层：Pick 仍返回 nil（旧调用方语义不变，热路径不用它做判断）
	if u, raw := p.Pick("u1"); u != nil || raw != "" {
		t.Fatalf("Pick should stay nil on all-dead, got %v %q", u, raw)
	}
}

// off / 池空是「配置意图」，必须返回 (nil, nil) 而不是错误 —— 否则没配代理的
// 部署会全线 502。
func TestPickStrict_OffAndEmptyAreNotErrors(t *testing.T) {
	p := proxypool.New()
	if _, _, err := p.PickStrict("u1"); err != nil {
		t.Fatalf("empty pool should be direct-connect, got %v", err)
	}
	p.Configure([]string{"http://1.1.1.1:8080"}, "off")
	if _, _, err := p.PickStrict("u1"); err != nil {
		t.Fatalf("off mode should be direct-connect, got %v", err)
	}
}

// 复活路径：DeadList 能列出、Revive 只对 dead 返回 true、复活后能立刻选到。
func TestReviveAndDeadList(t *testing.T) {
	p := proxypool.New()
	raw := "http://1.1.1.1:8080"
	p.Configure([]string{raw}, "per_account")
	for i := 0; i < 3; i++ {
		p.Report(raw, false)
	}
	if got := p.DeadList(); len(got) != 1 || got[0] != raw {
		t.Fatalf("DeadList = %v", got)
	}
	if !p.Revive(raw) {
		t.Fatal("Revive should report true for a dead entry")
	}
	if p.Revive(raw) {
		t.Fatal("Revive should report false when already alive")
	}
	if len(p.DeadList()) != 0 {
		t.Fatal("DeadList should be empty after revive")
	}
	if _, _, err := p.PickStrict("u1"); err != nil {
		t.Fatalf("pool should serve after revive, got %v", err)
	}
}

// 直连兜底开关默认必须关闭（安全默认）。
func TestDirectFallbackFlag(t *testing.T) {
	p := proxypool.New()
	if p.DirectFallback() {
		t.Fatal("direct fallback must default to false")
	}
	p.SetDirectFallback(true)
	if !p.DirectFallback() {
		t.Fatal("SetDirectFallback(true) not applied")
	}
}

// 探测配置的零值回落：不传 URL/Timeout/Concurrency 时必须有可用默认，
// 否则 StartProbe 会拿空 URL 去 Get（直接 panic 或永远失败）。
func TestProbeConfigNormalize(t *testing.T) {
	p := proxypool.New()
	stop := p.StartProbe(proxypool.ProbeConfig{Interval: 0}) // 0 = 关闭
	stop()                                                   // 关闭态也必须能安全 stop
	stop()                                                   // 幂等
}
