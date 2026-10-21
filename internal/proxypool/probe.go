// probe.go 熔断出口的自动复通探测。
//
// 为什么需要：Pick 只挑非 dead 出口，而 dead 出口**永远不会再被请求打到**，
// 于是 Report 里「成功即复活」那条分支对 dead 出口是死代码 —— 一次熔断就是
// 永久下线，只能靠面板手动 ResetDead。免费代理/机场节点抖动是常态，出口几小时
// 后往往已经恢复，却一直躺在 dead 名单里占着名额，池子越用越窄。
//
// 设计取舍：
//   - 只探测 dead 出口，健康出口零额外流量（探测本身也是出站请求，不免费）
//   - 探测成功才复活；失败**不**累加 fail（否则探测本身会把计数抬到阈值）
//   - 判据是「TCP+TLS 通、拿到任何 HTTP 响应」，不看状态码 —— 401/403/404
//     都证明线路活着，那是上游对请求的判定，不是出口坏
//   - 并发有上限：dead 多时逐个探会把网络打满，且探测失败率随并发上升
package proxypool

import (
	"log"
	"net/http"
	"sync"
	"time"
)

// ProbeConfig 复通探测参数。零值字段在 normalize 里回落默认。
type ProbeConfig struct {
	// Interval 探测周期；<=0 表示关闭探测。
	Interval time.Duration
	// URL 探测目标。默认 https://copilot.tencent.com/ —— 上游主域，
	// 能证明「这条出口到上游的链路是通的」。
	URL string
	// Timeout 单次探测超时。
	Timeout time.Duration
	// Concurrency 同时探测的出口数上限。
	Concurrency int
}

const (
	defaultProbeURL         = "https://copilot.tencent.com/"
	defaultProbeTimeout     = 8 * time.Second
	defaultProbeConcurrency = 4
)

func (c ProbeConfig) normalize() ProbeConfig {
	if c.URL == "" {
		c.URL = defaultProbeURL
	}
	if c.Timeout <= 0 {
		c.Timeout = defaultProbeTimeout
	}
	if c.Concurrency <= 0 {
		c.Concurrency = defaultProbeConcurrency
	}
	return c
}

// StartProbe 启动后台复通探测，返回停止函数（幂等）。
//
// 周期到了先看 DeadList：为空直接跳过，不产生任何请求。这保证「池子健康时
// 探测成本恒为 0」。
func (p *Pool) StartProbe(cfg ProbeConfig) (stop func()) {
	cfg = cfg.normalize()
	if cfg.Interval <= 0 {
		return func() {}
	}
	done := make(chan struct{})
	var once sync.Once
	go func() {
		t := time.NewTicker(cfg.Interval)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				p.probeOnce(cfg)
			}
		}
	}()
	return func() { once.Do(func() { close(done) }) }
}

// probeOnce 对当前所有 dead 出口做一轮探测（并发受限）。
func (p *Pool) probeOnce(cfg ProbeConfig) {
	dead := p.DeadList()
	if len(dead) == 0 {
		return
	}
	sem := make(chan struct{}, cfg.Concurrency)
	var wg sync.WaitGroup
	for _, raw := range dead {
		wg.Add(1)
		sem <- struct{}{}
		go func(raw string) {
			defer wg.Done()
			defer func() { <-sem }()
			if p.probeOne(raw, cfg) {
				if p.Revive(raw) {
					log.Printf("[proxypool] 出口复通: %s（探测 %s 成功）", maskProxy(raw), cfg.URL)
				}
			}
		}(raw)
	}
	wg.Wait()
}

// probeOne 单条出口探测：拿到任何 HTTP 响应即判通。
func (p *Pool) probeOne(raw string, cfg ProbeConfig) bool {
	p.mu.RLock()
	e := p.byRaw[raw]
	p.mu.RUnlock()
	if e == nil {
		return false
	}
	u, err := parseProxy(raw)
	if err != nil {
		return false
	}
	tr := &http.Transport{
		Proxy:               http.ProxyURL(u),
		TLSHandshakeTimeout: cfg.Timeout,
		DisableKeepAlives:   true, // 探测连接用完即弃，不进池
	}
	client := &http.Client{Transport: tr, Timeout: cfg.Timeout}
	resp, err := client.Get(cfg.URL)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return true
}

// DeadList 返回当前 dead 出口的原始配置串（含账密，仅供内部探测使用）。
func (p *Pool) DeadList() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]string, 0, 4)
	for _, e := range p.entries {
		if e.dead.Load() {
			out = append(out, e.raw)
		}
	}
	return out
}

// Revive 手动/探测复活单条出口：清 dead 标记与连续失败计数。
// 返回是否真的从 dead 变为 alive（已是 alive 或出口不存在返回 false）。
func (p *Pool) Revive(raw string) bool {
	if raw == "" {
		return false
	}
	p.mu.RLock()
	e := p.byRaw[raw]
	p.mu.RUnlock()
	if e == nil {
		return false
	}
	was := e.dead.Swap(false)
	e.fail.Store(0)
	return was
}

// SetDirectFallback 设置「池全 dead 时是否允许静默回落直连」。
//
// 默认 false：全 dead 时 PickStrict 返回 ErrAllDead，请求直接失败并带上原因。
// 为什么默认不放行：全池同时挂掉时回落直连，等于把整批账号从宿主机真实 IP
// 集中放出去 —— 上游看到的不是「某个账号换了出口」，而是「一批账号突然从同一个
// 陌生 IP 涌入」，风控命中率最高（issue #39 的结论）。
func (p *Pool) SetDirectFallback(v bool) {
	p.directFallback.Store(v)
}

// DirectFallback 返回当前开关值。
func (p *Pool) DirectFallback() bool {
	return p.directFallback.Load()
}
