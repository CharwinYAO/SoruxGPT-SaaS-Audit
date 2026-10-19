package api_test

import (
	"auditlimit/api"
	"auditlimit/config"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/gogf/gf/v2/frame/g"
)

const (
	e2eHost  = "e2e.local"
	e2eToken = "e2e-bearer-tok"
)

var (
	e2eBase     string
	e2eMockURL  string
	e2eOAuthHits int
)

func TestMain(m *testing.M) {
	// Mock OAUTH endpoint — always 200.
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e2eOAuthHits++
		w.WriteHeader(200)
	}))
	e2eMockURL = mock.URL

	// Wire globals.
	prevOAUTH := config.OAUTH
	config.OAUTH = mock.URL
	config.AuthUrlMap[e2eHost] = "auth.example.com"

	// Grab a free port.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Println("listen:", err)
		os.Exit(1)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	s := g.Server("audit-e2e")
	s.SetDumpRouterMap(false)
	s.SetAccessLogEnabled(false)
	s.SetErrorLogEnabled(false)
	s.SetPort(port)
	s.BindHandler("POST:/audit_limit", api.AuditLimit)
	s.BindHandler("POST:/audit_limit/after", api.AuditLimitAfter)
	s.BindHandler("POST:/audit_limit/release", api.AuditLimitRelease)
	if err := s.Start(); err != nil {
		fmt.Println("server start:", err)
		os.Exit(1)
	}
	// wait until socket is accepting
	for i := 0; i < 50; i++ {
		c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 50*time.Millisecond)
		if err == nil {
			c.Close()
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	e2eBase = fmt.Sprintf("http://127.0.0.1:%d", port)

	code := m.Run()

	_ = s.Shutdown()
	mock.Close()
	config.OAUTH = prevOAUTH
	delete(config.AuthUrlMap, e2eHost)

	os.Exit(code)
}

type preCallResp struct {
	status int
	rid    string
	body   string
}

func preCall(t *testing.T, model, token string) preCallResp {
	t.Helper()
	body := map[string]any{
		"action": "next",
		"model":  model,
		"messages": []map[string]any{
			{"content": map[string]any{"parts": []string{"hello"}}},
		},
	}
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", e2eBase+"/audit_limit", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Referer", "https://"+e2eHost+"/c/abc")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("preCall: %v", err)
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	return preCallResp{
		status: resp.StatusCode,
		rid:    resp.Header.Get("X-Reservation-Id"),
		body:   string(rb),
	}
}

func postCB(t *testing.T, path, rid, token string) int {
	t.Helper()
	req, _ := http.NewRequest("POST", e2eBase+path, bytes.NewReader([]byte("{}")))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Referer", "https://"+e2eHost+"/")
	req.Header.Set("Content-Type", "application/json")
	if rid != "" {
		req.Header.Set("X-Reservation-Id", rid)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("postCB %s: %v", path, err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// postReleaseRaw drives /release with NO Authorization and NO Referer — proves
// release works without auth (contract: only the unguessable rid is required).
func postReleaseRaw(t *testing.T, rid string) int {
	t.Helper()
	req, _ := http.NewRequest("POST", e2eBase+"/audit_limit/release", bytes.NewReader([]byte("{}")))
	req.Header.Set("Content-Type", "application/json")
	if rid != "" {
		req.Header.Set("X-Reservation-Id", rid)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("postReleaseRaw: %v", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// TestE2E_ReleaseRefundsQuota — the question "回答出错积分会不会加上去" lives here.
// 3 pre-calls exhaust the bucket → 4th = 429.
// Then 3 /release calls refund all credit → 3 more pre-calls succeed.
// Within a 1h rate window the only way these could succeed is if Refund()
// actually banked the tokens back.
func TestE2E_ReleaseRefundsQuota(t *testing.T) {
	model := "E2E-RELEASE"
	token := "tok-release"
	if err := os.Setenv(model, "3/1h"); err != nil {
		t.Fatal(err)
	}
	defer os.Unsetenv(model)

	var rids []string
	for i := 0; i < 3; i++ {
		r := preCall(t, model, token)
		if r.status != 200 {
			t.Fatalf("pre-call #%d: want 200, got %d body=%s", i+1, r.status, r.body)
		}
		if r.rid == "" {
			t.Fatalf("pre-call #%d: missing X-Reservation-Id", i+1)
		}
		rids = append(rids, r.rid)
	}
	if r := preCall(t, model, token); r.status != 429 {
		t.Fatalf("4th pre-call: want 429, got %d body=%s", r.status, r.body)
	}

	for i, rid := range rids {
		if s := postCB(t, "/audit_limit/release", rid, token); s != 200 {
			t.Fatalf("release #%d: %d", i+1, s)
		}
	}

	for i := 0; i < 3; i++ {
		r := preCall(t, model, token)
		if r.status != 200 {
			t.Fatalf("post-release pre-call #%d: %d body=%s", i+1, r.status, r.body)
		}
	}
	if r := preCall(t, model, token); r.status != 429 {
		t.Fatalf("post-release 4th: want 429, got %d", r.status)
	}
}

// TestE2E_AfterCommitsQuota — /after must NOT refund.
// 3 pre-calls + 3 /after → 4th pre-call = 429 (token stays debited).
func TestE2E_AfterCommitsQuota(t *testing.T) {
	model := "E2E-AFTER"
	token := "tok-after"
	os.Setenv(model, "3/1h")
	defer os.Unsetenv(model)

	var rids []string
	for i := 0; i < 3; i++ {
		r := preCall(t, model, token)
		if r.status != 200 {
			t.Fatalf("pre-call #%d: %d body=%s", i+1, r.status, r.body)
		}
		rids = append(rids, r.rid)
	}
	for _, rid := range rids {
		if s := postCB(t, "/audit_limit/after", rid, token); s != 200 {
			t.Fatalf("after: %d", s)
		}
	}
	if r := preCall(t, model, token); r.status != 429 {
		t.Fatalf("post-commit pre-call: want 429, got %d", r.status)
	}
}

// TestE2E_ReleaseIdempotent — double release / empty rid all 200, no
// double credit (verified by exhausting bucket again).
func TestE2E_ReleaseIdempotent(t *testing.T) {
	model := "E2E-IDEMP"
	token := "tok-idemp"
	os.Setenv(model, "1/1h")
	defer os.Unsetenv(model)

	r := preCall(t, model, token)
	if r.status != 200 {
		t.Fatalf("pre-call: %d body=%s", r.status, r.body)
	}
	rid := r.rid

	for i := 0; i < 3; i++ {
		if s := postCB(t, "/audit_limit/release", rid, token); s != 200 {
			t.Fatalf("release iter %d: %d", i, s)
		}
	}
	if s := postCB(t, "/audit_limit/release", "", token); s != 200 {
		t.Fatalf("release empty: %d", s)
	}

	// Bucket=1, refunded once. Two pre-calls: first ok, second 429.
	if r := preCall(t, model, token); r.status != 200 {
		t.Fatalf("post-idemp pre-call #1: %d body=%s", r.status, r.body)
	}
	if r := preCall(t, model, token); r.status != 429 {
		t.Fatalf("post-idemp pre-call #2: want 429, got %d body=%s", r.status, r.body)
	}
}

// TestE2E_TESTModel_ErrorRefundsQuota — 用户的核心问题：
// 用 config.yaml 里预配置的 TEST 模型（TEST="1/1h"）走完整链路，
// 模拟 "ChatGPT 报错" 场景，验证积分被退回。
//
// 场景脚本（每一步都 t.Logf 输出，跑 -v 时可读）：
//
//	A. TEST 模型 = 1/1h（来自 config/config.yaml，每小时 1 次，1 秒内不可能自然回血）
//	B. 第 1 次正常对话 200 → 桶被打空 → 第 2 次必 429
//	C. 模拟 "ChatGPT 回答报错" / "转发失败" → share-2026 调 /audit_limit/release
//	D. 验证桶恢复了 1 格：第 3 次前调 200，第 4 次又 429（精确退还 1 次）
//	E. 模拟正常完成 → /after 不退积分 → 第 5 次仍 429
//
// 通过 = 报错路径不扣积分；精确退还 1 次；不会多退也不会少退。
func TestE2E_TESTModel_ErrorRefundsQuota(t *testing.T) {
	const (
		model = "TEST"
		token = "tok-test-error-path"
	)
	t.Logf("A. TEST=1/1h（来自 config.yaml，每小时 1 次配额，1 秒内不会自然回血）")

	// B-1. 第 1 次正常前调
	r1 := preCall(t, model, token)
	if r1.status != 200 || r1.rid == "" {
		t.Fatalf("B-1 pre-call: status=%d rid=%q body=%s", r1.status, r1.rid, r1.body)
	}
	t.Logf("B-1. 第 1 次对话: 200 X-Reservation-Id=%s", r1.rid)

	// B-2. 桶已空
	r2 := preCall(t, model, token)
	if r2.status != 429 {
		t.Fatalf("B-2 expected 429 (bucket exhausted), got %d body=%s", r2.status, r2.body)
	}
	t.Logf("B-2. 第 2 次对话: 429（额度耗尽，符合预期）")

	// C. 模拟 ChatGPT 回答报错 → share-2026 主动 release
	if s := postCB(t, "/audit_limit/release", r1.rid, token); s != 200 {
		t.Fatalf("C release(rid1): %d", s)
	}
	t.Logf("C. 模拟「ChatGPT 报错」对 rid1 调 /release: 200（积分应该已退回 1 格）")

	// D-1. 验证退还成功
	r3 := preCall(t, model, token)
	if r3.status != 200 || r3.rid == "" {
		t.Fatalf("D-1 expected 200 after release, got %d body=%s", r3.status, r3.body)
	}
	t.Logf("D-1. release 后第 3 次对话: 200 ✅ 积分确实退回了")

	// D-2. 不会多退
	r4 := preCall(t, model, token)
	if r4.status != 429 {
		t.Fatalf("D-2 expected 429 (no double refund), got %d body=%s", r4.status, r4.body)
	}
	t.Logf("D-2. 第 4 次对话: 429（确认只退了 1 格，不会多退）")

	// E. 成功对话路径：/after 不退积分
	if s := postCB(t, "/audit_limit/after", r3.rid, token); s != 200 {
		t.Fatalf("E /after: %d", s)
	}
	t.Logf("E. 模拟「对话成功完成」对 rid3 调 /after: 200")

	r5 := preCall(t, model, token)
	if r5.status != 429 {
		t.Fatalf("E-after expected 429 (after = commit, no refund), got %d body=%s", r5.status, r5.body)
	}
	t.Logf("E-after. 第 5 次对话: 429 ✅ 成功对话不会退还积分")

	t.Logf("结论：TEST 模型在【报错→release】路径下积分被精确退回；【成功→after】路径下积分被扣除。")
}

// TestE2E_ReleaseNoAuth verifies that /release works without Authorization or
// Referer headers — only the unguessable X-Reservation-Id is required.
// Contract is documented in api/AGENTS.md and the AuditLimitRelease docstring.
func TestE2E_ReleaseNoAuth(t *testing.T) {
	model := "E2E-NOAUTH"
	token := "tok-noauth"
	if err := os.Setenv(model, "1/1h"); err != nil {
		t.Fatal(err)
	}
	defer os.Unsetenv(model)

	// Pre-call still requires auth (unchanged) — exhaust the bucket.
	r1 := preCall(t, model, token)
	if r1.status != 200 || r1.rid == "" {
		t.Fatalf("pre-call: %d body=%s", r1.status, r1.body)
	}
	if r2 := preCall(t, model, token); r2.status != 429 {
		t.Fatalf("expected 429 after exhaust, got %d", r2.status)
	}

	// Release WITHOUT Authorization / Referer.
	if s := postReleaseRaw(t, r1.rid); s != 200 {
		t.Fatalf("release w/o auth: want 200, got %d", s)
	}

	// Quota should be back.
	if r3 := preCall(t, model, token); r3.status != 200 {
		t.Fatalf("post-release pre-call: want 200, got %d body=%s", r3.status, r3.body)
	}

	// Empty rid + no auth → still 200 (idempotent no-op).
	if s := postReleaseRaw(t, ""); s != 200 {
		t.Fatalf("release empty rid w/o auth: %d", s)
	}

	// Unknown rid + no auth → still 200, but no refund.
	if s := postReleaseRaw(t, "deadbeef0000deadbeef0000deadbeef"); s != 200 {
		t.Fatalf("release unknown rid w/o auth: %d", s)
	}
	// Bucket should still be empty (the unknown rid didn't credit anything).
	if r4 := preCall(t, model, token); r4.status != 429 {
		t.Fatalf("unknown rid leaked a credit, got %d", r4.status)
	}
}
