// global_models_test.go global 模型目录探测的 UA 家族覆盖回归。
package upstream

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestGlobalCatalogProbesDesktopUA 锁定「/v3/config 目录按 UA 家族分档」这一上游事实：
// 仅桌面端 UA 下发的模型必须出现在并集里。
//
// 回归目标（上游 #102 / PR #103）：gpt-6-luna 只在桌面端目录下发、实际可调用
// （实测 HTTP 200），只探 IDE + CLI 两路时它永远进不了 /v1/models。
func TestGlobalCatalogProbesDesktopUA(t *testing.T) {
	prev := auth.GlobalEnabled()
	auth.SetGlobalEnabled(true)
	defer auth.SetGlobalEnabled(prev)

	const desktopOnly = "gpt-6-luna"
	// 三路 /v3/config 并发探测 → handler 并发写，必须加锁（CI 曾现 concurrent map writes）。
	var seenMu sync.Mutex
	seenUA := map[string]bool{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/v3/config") {
			// 企业端点家族一律 500：本用例只验证 v3 多路并集，同时覆盖
			// 「家族路失败不拖累 v3 结果」的降级语义。
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		ua := r.Header.Get("User-Agent")
		seenMu.Lock()
		seenUA[ua] = true
		seenMu.Unlock()
		ids := []string{"shared-model"}
		switch {
		case strings.HasPrefix(ua, "CodeBuddyIDE/"):
			ids = append(ids, "ide-only")
		case strings.HasPrefix(ua, "CLI/"):
			ids = append(ids, "cli-only")
		case strings.HasPrefix(ua, "workbuddy-ai/"), strings.HasPrefix(ua, "workbuddy/"):
			ids = append(ids, desktopOnly)
		default:
			t.Errorf("v3/config 探测带了非预期 UA: %q", ua)
		}
		models := make([]map[string]any, 0, len(ids))
		for _, id := range ids {
			models = append(models, map[string]any{
				"id": id, "maxInputTokens": 100000, "maxOutputTokens": 32000,
			})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": 0,
			"data": map[string]any{"models": models},
		})
	}))
	defer srv.Close()

	c := New()
	c.HTTP = srv.Client()
	c.ChatBaseGlobal = srv.URL
	c.GlobalEnabled = true
	a := &auth.Auth{UID: "g1", AccessToken: "at", Domain: "www.workbuddy.ai"}

	names := c.FetchGlobalModels(a)
	got := make(map[string]bool, len(names))
	for _, n := range names {
		got[n] = true
	}
	for _, want := range []string{"shared-model", desktopOnly} {
		if !got[want] {
			t.Errorf("FetchGlobalModels 缺少 %q（并集=%v）", want, names)
		}
	}
	// issue #216：桌面族成功时它是目录权威，IDE/CLI 独有模型不再进目录
	//（实测它们在 chat 端点回 11102 “service info not found”，列出来只会让
	// 客户端选到死模型；官方桌面端 picker 也只显示桌面族那 27 个）。
	for _, unwanted := range []string{"ide-only", "cli-only"} {
		if got[unwanted] {
			t.Errorf("桌面族成功时不应列出非桌面族独有模型 %q（目录=%v）", unwanted, names)
		}
	}
	if src := c.GlobalCatalogSource(); src != globalCatalogSourceDesktop {
		t.Errorf("目录来源=%q want %q", src, globalCatalogSourceDesktop)
	}
	seenMu.Lock()
	seen := make([]string, 0, len(seenUA))
	for ua := range seenUA {
		seen = append(seen, ua)
	}
	seenMu.Unlock()
	if len(seen) != 3 {
		t.Errorf("v3/config 应探 3 种 UA 家族，实际 %d 种: %v", len(seen), seen)
	}
}

// TestGlobalCatalogDesktopProbeFailureFallsBackToUnion 桌面族探测失败时回落
// IDE+CLI 并集（宁可多列也不空表），并把来源标成 v3-union 供面板提示。
func TestGlobalCatalogDesktopProbeFailureFallsBackToUnion(t *testing.T) {
	prev := auth.GlobalEnabled()
	auth.SetGlobalEnabled(true)
	defer auth.SetGlobalEnabled(prev)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/v3/config") {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		ua := r.Header.Get("User-Agent")
		var ids []string
		switch {
		case strings.HasPrefix(ua, "workbuddy-ai/"), strings.HasPrefix(ua, "workbuddy/"):
			// 桌面族失败（模拟上游对该族 500）。
			w.WriteHeader(http.StatusInternalServerError)
			return
		case strings.HasPrefix(ua, "CodeBuddyIDE/"):
			ids = []string{"ide-model"}
		default:
			ids = []string{"cli-model"}
		}
		models := make([]map[string]any, 0, len(ids))
		for _, id := range ids {
			models = append(models, map[string]any{"id": id, "maxInputTokens": 1000, "maxOutputTokens": 1000})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{"models": models}})
	}))
	defer srv.Close()

	c := New()
	c.HTTP = srv.Client()
	c.ChatBaseGlobal = srv.URL
	c.GlobalEnabled = true
	names := c.FetchGlobalModels(&auth.Auth{UID: "g1", AccessToken: "at", Domain: "www.workbuddy.ai"})
	got := map[string]bool{}
	for _, n := range names {
		got[n] = true
	}
	if !got["ide-model"] || !got["cli-model"] {
		t.Fatalf("桌面族失败时应回落 IDE+CLI 并集，实际=%v", names)
	}
	if src := c.GlobalCatalogSource(); src != globalCatalogSourceUnion {
		t.Errorf("目录来源=%q want %q", src, globalCatalogSourceUnion)
	}
}

// TestGlobalCatalogDesktopAuthorityExcludesEnterpriseLegacy 桌面族成功时，企业端点
// **不得**再往目录里加 id（它带着 deep-model / gpt-5.3-codex 这类已下架或 11102 的
// 旧条目）——这正是 issue #216 里「多出两个官方目录没有的模型」的来源。
// 企业端点的同 id 条目仍用于补元数据。
func TestGlobalCatalogDesktopAuthorityExcludesEnterpriseLegacy(t *testing.T) {
	prev := auth.GlobalEnabled()
	auth.SetGlobalEnabled(true)
	defer auth.SetGlobalEnabled(prev)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/v3/config"):
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{
				"models": []map[string]any{{"id": "desk-model", "maxOutputTokens": 32000}},
			}})
		case strings.HasSuffix(r.URL.Path, "/v2/enterprises/personal/models"):
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{
				"models": []map[string]any{
					{"id": "desk-model", "maxInputTokens": 176000},
					{"id": "deep-model"}, {"id": "gpt-5.3-codex"},
				},
			}})
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	c := New()
	c.HTTP = srv.Client()
	c.ChatBaseGlobal = srv.URL
	c.GlobalEnabled = true
	names := c.FetchGlobalModels(&auth.Auth{UID: "g1", AccessToken: "at", Domain: "www.workbuddy.ai"})
	got := map[string]bool{}
	for _, n := range names {
		got[n] = true
	}
	if !got["desk-model"] {
		t.Fatalf("桌面族模型缺失: %v", names)
	}
	for _, legacy := range []string{"deep-model", "gpt-5.3-codex"} {
		if got[legacy] {
			t.Errorf("企业端点旧条目 %q 不应进目录: %v", legacy, names)
		}
	}
}

// TestInvalidateGlobalModelsCacheReprobes 锁定「重新获取」按钮的语义（issue #216 现象 3）：
// 清缓存后必须真的再打一次上游，而不是继续读 1h 缓存。
func TestInvalidateGlobalModelsCacheReprobes(t *testing.T) {
	prev := auth.GlobalEnabled()
	auth.SetGlobalEnabled(true)
	defer auth.SetGlobalEnabled(prev)

	var v3Hits, entHits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/v3/config"):
			atomic.AddInt32(&v3Hits, 1)
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{
				"models": []map[string]any{{"id": "m1"}},
			}})
		case strings.HasSuffix(r.URL.Path, "/v2/enterprises/personal/models"):
			atomic.AddInt32(&entHits, 1)
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{
				"models": []map[string]any{{"id": "m1"}},
			}})
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	c := New()
	c.HTTP = srv.Client()
	c.ChatBaseGlobal = srv.URL
	c.GlobalEnabled = true
	a := &auth.Auth{UID: "g1", AccessToken: "at", Domain: "www.workbuddy.ai"}

	c.FetchGlobalModels(a)
	first := atomic.LoadInt32(&v3Hits)
	if first == 0 {
		t.Fatal("首次探测没有打上游")
	}
	// 缓存命中：不再打上游。
	c.FetchGlobalModels(a)
	if got := atomic.LoadInt32(&v3Hits); got != first {
		t.Fatalf("缓存命中期间不应再探上游：before=%d after=%d", first, got)
	}
	// 清缓存后必须重探。
	c.InvalidateGlobalModelsCache()
	if src := c.GlobalCatalogSource(); src != "" {
		t.Errorf("清缓存后来源应为空，got %q", src)
	}
	c.FetchGlobalModels(a)
	if got := atomic.LoadInt32(&v3Hits); got <= first {
		t.Fatalf("清缓存后应重新探测：before=%d after=%d", first, got)
	}
}
