// fingerprint_test.go 官方客户端指纹对齐真机（5.8.4 国际版 / 5.8.3 国内版）的回归。
//
// 证据来源：2026-10-20 桌面客户端真实启动流量（mitmproxy 抓到 www.workbuddy.ai
// 183 条请求）+ 客户端 app.asar / cli/dist 静态反编译。以下每个断言都对应一处
// 与真机不一致的地方——不一致本身是「被判定非官方客户端」的候选指纹。
package upstream

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

func globalAuth() *auth.Auth {
	return &auth.Auth{UID: "u-g", AccessToken: "at", Domain: "www.workbuddy.ai", Nickname: "g"}
}

// TestUARealmBrand 品牌段必须是小写连字符 slug，且版本按 realm 分开。
//
// 真机国际版 UA：`workbuddy-ai/5.8.4 workbuddy-ai/5.8.4 CLI/2.161.0`
// （分发包 productName "WorkBuddy AI" 转 slug；国内版 "WorkBuddy" → `workbuddy`）。
// 原先写的是大驼峰 `WorkBuddy/5.5.4 WorkBuddy AI/5.5.4 CLI/2.137.1`：品牌段形态
// 与版本号都对不上真实分发包。
func TestUARealmBrand(t *testing.T) {
	c := New()

	cn := &auth.Auth{UID: "u-c", AccessToken: "at"}
	if got, want := c.userAgent(cn), "workbuddy/5.8.3 workbuddy/5.8.3 CLI/2.161.0"; got != want {
		t.Errorf("CN UA=%q want %q", got, want)
	}
	if got, want := c.userAgent(globalAuth()), "workbuddy-ai/5.8.4 workbuddy-ai/5.8.4 CLI/2.161.0"; got != want {
		t.Errorf("global UA=%q want %q", got, want)
	}
	// 显式配置仍然赢（用户自定义品牌/版本）。
	c.ClientVersion = "9.9.9"
	c.CliVersion = "1.2.3"
	if got, want := c.userAgent(globalAuth()), "workbuddy-ai/9.9.9 workbuddy-ai/9.9.9 CLI/1.2.3"; got != want {
		t.Errorf("override UA=%q want %q", got, want)
	}
}

// TestBillingUAAxiosForGlobal 国际版计费请求的 UA 是 axios 默认串。
//
// 真机实测：`/billing/meter/get-user-resource-*` 与 `-summary` 三件套的 UA 都是
// `axios/1.16.1`（billing 层的 RestOperations 走 axios），与客户端自身 UA 不同。
// CN 保持 `WorkBuddy/<ver>` 不动——线上可用，没有证据说要改。
func TestBillingUAAxiosForGlobal(t *testing.T) {
	c := New()
	if got := c.billingUA(globalAuth()); got != "axios/1.16.1" {
		t.Errorf("global billing UA=%q want axios/1.16.1", got)
	}
	if got := c.billingUA(&auth.Auth{UID: "u-c", AccessToken: "at"}); !strings.HasPrefix(got, "WorkBuddy/") {
		t.Errorf("CN billing UA=%q want WorkBuddy/<ver>", got)
	}
}

// TestIdentityHeadersPersonalSendsNone 个人号三头全不发。
//
// 反编译的 runIdentityHeaders() 只在「有 enterpriseId / idSource / token_source 任一」
// 时才发 X-Userinfo；真机个人号抓包里 X-Userinfo / X-Auth-Method / X-Id-Source 全都不存在。
// 凭「个人号也该有」去补空值是伪造指纹，比不发更容易被识别。
func TestIdentityHeadersPersonalSendsNone(t *testing.T) {
	c := New()
	a := globalAuth()
	req := httptest.NewRequest("POST", "https://www.workbuddy.ai/v2/chat/completions", nil)
	c.injectIdentityHeaders(req, a)
	for _, h := range []string{"X-Userinfo", "X-Auth-Method", "X-Id-Source"} {
		if v := req.Header.Get(h); v != "" {
			t.Errorf("个人号不该发 %s=%q", h, v)
		}
	}
}

// TestIdentityHeadersEnterpriseUserinfo 企业号发 X-Userinfo，内容是 base64(JSON{uin,owner_uin})。
// uin 段取 uid（反编译逻辑是 {uin: n.uid}），owner_uin 取 enterpriseId。
func TestIdentityHeadersEnterpriseUserinfo(t *testing.T) {
	c := New()
	a := globalAuth()
	a.EnterpriseID = "ent-1"
	req := httptest.NewRequest("POST", "https://www.workbuddy.ai/v2/chat/completions", nil)
	c.injectIdentityHeaders(req, a)

	raw := req.Header.Get("X-Userinfo")
	if raw == "" {
		t.Fatal("企业号应发 X-Userinfo")
	}
	dec, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		t.Fatalf("X-Userinfo 不是标准 base64: %v", err)
	}
	var payload map[string]string
	if err := json.Unmarshal(dec, &payload); err != nil {
		t.Fatalf("X-Userinfo 不是 JSON: %v (%s)", err, dec)
	}
	if payload["uin"] != "u-g" {
		t.Errorf("uin=%q want u-g（反编译逻辑是 {uin: uid}）", payload["uin"])
	}
	if payload["owner_uin"] != "ent-1" {
		t.Errorf("owner_uin=%q want ent-1", payload["owner_uin"])
	}
	if _, ok := payload["token_source"]; ok {
		t.Errorf("无 token_source 时不该写入该键（不是写空串）：%v", payload)
	}
}

// TestIdentityHeadersTokenSourceFromJWT X-Auth-Method 取自 JWT 的 token_source 声明。
func TestIdentityHeadersTokenSourceFromJWT(t *testing.T) {
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"token_source":"enterprise_switch"}`))
	a := &auth.Auth{UID: "u1", AccessToken: "h." + payload + ".s"}
	req := httptest.NewRequest("POST", "https://example/v2/chat/completions", nil)
	New().injectIdentityHeaders(req, a)

	if got := req.Header.Get("X-Auth-Method"); got != "enterprise_switch" {
		t.Fatalf("X-Auth-Method=%q want enterprise_switch", got)
	}
	dec, _ := base64.StdEncoding.DecodeString(req.Header.Get("X-Userinfo"))
	var m map[string]string
	_ = json.Unmarshal(dec, &m)
	if m["token_source"] != "enterprise_switch" || m["uin"] != "u1" {
		t.Fatalf("X-Userinfo=%s", dec)
	}
}

// TestChatCarriesPrivateData 真机 chat 出站恒带 X-Private-Data: true。
func TestChatCarriesPrivateData(t *testing.T) {
	c := New()
	c.ChatBaseCN = "https://cn.example"
	a := &auth.Auth{UID: "u1", AccessToken: "at"}
	req := httptest.NewRequest("POST", "https://cn.example/v2/chat/completions", nil)
	c.ChatHeaders(req, a, "", ChatMeta{ConversationID: "c1", ConversationRequestID: "r1"})
	if got := req.Header.Get("X-Private-Data"); got != "true" {
		t.Fatalf("X-Private-Data=%q want true", got)
	}
}

// TestDesktopReportDomainIsHost X-Domain 必须是裸主机名，不是带 scheme 的 URL。
// 真机：`X-Domain: www.workbuddy.ai`；原实现塞的是 desktopBase（https://...）。
func TestDesktopReportDomainIsHost(t *testing.T) {
	var gotDomain string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotDomain = r.Header.Get("X-Domain")
		w.Write([]byte(`{"code":0,"msg":"OK"}`))
	}))
	defer srv.Close()

	// 用真 base 形式（带 scheme）验证剥离逻辑：client 的 base 就是 URL。
	c := &Client{HTTP: srv.Client(), ChatBaseGlobal: srv.URL, GlobalEnabled: true}
	if err := c.ReportDesktopEvent(globalAuth(), DesktopEvent{"eventCode": "x"}); err != nil {
		t.Fatalf("report: %v", err)
	}
	// Hostname() 剥掉 scheme 与端口（真机 X-Domain 是纯主机名，无端口）。
	u, _ := url.Parse(srv.URL)
	want := u.Hostname()
	if gotDomain != want {
		t.Fatalf("X-Domain=%q want %q（裸 host，不带 scheme）", gotDomain, want)
	}
}

// TestGlobalBillingSummary 国际版余额走 summary + paid/free 明细：
// remain/total 取 summary 的 Cycle{Remain,Total}Capacity（字符串小数），
// 到期批次按 PackageCode 与明细关联。
func TestGlobalBillingSummary(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		switch {
		case strings.HasSuffix(r.URL.Path, "/get-user-resource-summary"):
			w.Write([]byte(`{"code":0,"msg":"OK","data":{"Packages":[` +
				`{"PackageCode":"PKG-A","CycleTotalCapacity":"540","CycleRemainCapacity":"335.47000026","CycleUsedCapacity":"204.52999974","CapacityUnit":"credit"},` +
				`{"PackageCode":"PKG-B","CycleTotalCapacity":"100","CycleRemainCapacity":"0","CycleUsedCapacity":"100"}` +
				`],"SubscriptionPackageName":"免费版","IsPaidUser":false}}`))
		case strings.HasSuffix(r.URL.Path, "/get-user-resource-free-packages"):
			soon := time.Now().Add(48 * time.Hour).Format("2006-01-02 15:04:05")
			w.Write([]byte(`{"code":0,"msg":"OK","data":{"Accounts":[` +
				`{"PackageCode":"PKG-A","PackageName":"权益赠送包","CycleCapacitySize":540,"CycleCapacityRemain":335,` +
				`"CycleEndTime":"` + soon + `","DeductionEndTime":0,"ExpiredTime":""}` +
				`],"TotalCount":1}}`))
		case strings.HasSuffix(r.URL.Path, "/get-user-resource-paid-packages"):
			w.Write([]byte(`{"code":0,"msg":"OK","data":{"Accounts":[],"TotalCount":0}}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()

	c := &Client{HTTP: srv.Client(), BillingBaseGlobal: srv.URL, GlobalEnabled: true}
	remain, total, expiring, earliestAt, earliestRemaining, buckets, err :=
		c.UserResourceSnapshot(globalAuth(), 168*time.Hour)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	// 540 + 100 = 640 total；335.47 + 0 → 截断 335 remain。
	if total != 640 {
		t.Errorf("total=%d want 640", total)
	}
	if remain != 335 {
		t.Errorf("remain=%d want 335（字符串小数按整数截断，与既有口径一致）", remain)
	}
	if len(buckets) != 1 || buckets[0].Remain != 335 {
		t.Fatalf("buckets=%+v want 单批 335", buckets)
	}
	if earliestAt.IsZero() || earliestRemaining != 335 {
		t.Errorf("earliestAt=%v earliestRemaining=%d want 非零/335", earliestAt, earliestRemaining)
	}
	// 48h 后到期、窗口 168h → 该批整笔计入 expiring。
	if expiring != 335 {
		t.Errorf("expiring=%d want 335（48h < 168h 窗口）", expiring)
	}
	// 必须打的是国际版新三件套，不能退回 CN 的 get-user-resource。
	for _, p := range paths {
		if strings.HasSuffix(p, "/billing/meter/get-user-resource") {
			t.Fatalf("国际版不该再走 CN 分页路径：%v", paths)
		}
	}
}

// TestGlobalBillingFallsBackToCNPath summary 失败时回落 CN 分页路径（老路径兼容尝试）。
func TestGlobalBillingFallsBackToCNPath(t *testing.T) {
	var sawCN bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/get-user-resource-summary") {
			w.WriteHeader(500)
			return
		}
		if strings.Contains(r.URL.Path, "get-user-resource") &&
			!strings.Contains(r.URL.Path, "paid-packages") &&
			!strings.Contains(r.URL.Path, "free-packages") {
			sawCN = true
			w.Write([]byte(`{"code":0,"data":{"Response":{"Data":{"Accounts":` +
				`[{"CycleCapacitySize":1000,"CycleCapacityRemain":900,"CycleCapacityUsed":100}]}}}}`))
			return
		}
		w.WriteHeader(404)
	}))
	defer srv.Close()

	c := &Client{HTTP: srv.Client(), BillingBaseGlobal: srv.URL, GlobalEnabled: true}
	remain, total, _, _, _, _, err := c.UserResourceSnapshot(globalAuth(), 0)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if !sawCN {
		t.Fatal("summary 失败时应回落到 CN 分页路径")
	}
	if remain != 900 || total != 1000 {
		t.Errorf("remain=%d total=%d want 900/1000", remain, total)
	}
}
