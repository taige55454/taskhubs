package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

type apiTestEnv struct {
	store   *Store
	srv     *Server
	handler http.Handler
}

func newAPITestEnv(t *testing.T) *apiTestEnv {
	t.Helper()
	st, loc := newTestStore(t)
	fixed := time.Date(2026, 11, 25, 9, 30, 0, 0, loc)
	st.SetClock(func() time.Time { return fixed })

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := NewServer(st, "8080", logger)
	return &apiTestEnv{
		store:   st,
		srv:     srv,
		handler: srv.Routes(),
	}
}

func (e *apiTestEnv) doJSON(t *testing.T, method, path string, body any, cookie *http.Cookie, csrf string) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != nil {
		switch v := body.(type) {
		case string:
			reader = strings.NewReader(v)
		default:
			b, _ := json.Marshal(body)
			reader = bytes.NewReader(b)
		}
	}
	req := httptest.NewRequest(method, path, reader)
	req.RemoteAddr = "192.168.1.88:54321"
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	if csrf != "" {
		req.Header.Set("X-CSRF-Token", csrf)
	}
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	return rec
}

func loginViaAPI(t *testing.T, e *apiTestEnv, nickname, password string) (*http.Cookie, string) {
	t.Helper()
	rec := e.doJSON(t, http.MethodPost, "/api/login", map[string]string{
		"nickname": nickname,
		"password": password,
	}, nil, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("登录 %s 失败: code=%d, body=%s", nickname, rec.Code, rec.Body.String())
	}
	var resp struct {
		CSRFToken string `json:"csrf_token"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	var sidCookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookieName {
			sidCookie = c
		}
	}
	if sidCookie == nil || resp.CSRFToken == "" {
		t.Fatal("登录后未返回 Cookie 或 CSRF Token")
	}
	return sidCookie, resp.CSRFToken
}

// 1. 测试全量 24 个业务与后台 HTTP 接口完整闭环
func TestAllHTTPEndpointsEndToEnd(t *testing.T) {
	e := newAPITestEnv(t)
	ctx := context.Background()

	// 静态文件、Service Worker 缓存头与健康检查
	resRoot := e.doJSON(t, http.MethodGet, "/", nil, nil, "")
	if resRoot.Code != http.StatusOK || resRoot.Header().Get("X-Frame-Options") != "SAMEORIGIN" {
		t.Fatalf("首页或安全响应头异常: code=%d", resRoot.Code)
	}
	resSW := e.doJSON(t, http.MethodGet, "/sw.js", nil, nil, "")
	if resSW.Code != http.StatusOK || resSW.Header().Get("Cache-Control") != "no-cache" {
		t.Fatalf("sw.js 缓存控制头异常: %v", resSW.Header())
	}
	resHz := e.doJSON(t, http.MethodGet, "/healthz", nil, nil, "")
	if resHz.Code != http.StatusOK {
		t.Fatalf("healthz 失败: %d", resHz.Code)
	}

	// 未登录访问受保护接口应返回 401
	if r := e.doJSON(t, http.MethodGet, "/api/tasks", nil, nil, ""); r.Code != http.StatusUnauthorized {
		t.Fatalf("未登录应返回 401，实际 %d", r.Code)
	}

	// 管理员登录与 Bootstrap 检查
	admCookie, admCSRF := loginViaAPI(t, e, "主机管理员", DefaultAdminInitialPassword)
	resBoot := e.doJSON(t, http.MethodGet, "/api/bootstrap", nil, admCookie, "")
	if resBoot.Code != http.StatusOK {
		t.Fatal("bootstrap 请求失败")
	}

	// 缺少或错误 CSRF Token 的写操作必须拦截返回 403
	if r := e.doJSON(t, http.MethodPost, "/api/admin/posts", map[string]any{"name": "X"}, admCookie, "bad_csrf"); r.Code != http.StatusForbidden {
		t.Fatalf("错误 CSRF 应返回 403，实际 %d", r.Code)
	}

	// 1) 新增岗位
	if r := e.doJSON(t, http.MethodPost, "/api/admin/posts", map[string]any{
		"name": "前台接待岗", "color": "#007AFF", "sort_order": 10, "is_active": true,
	}, admCookie, admCSRF); r.Code != http.StatusOK {
		t.Fatalf("保存岗位失败: %s", r.Body.String())
	}

	// 2) 新增免密伙伴与设密伙伴
	for _, p := range []map[string]any{
		{"nickname": "免密小美", "in_rota": true, "is_active": true},
		{"nickname": "设密小帅", "password": "shuai123", "in_rota": true, "is_active": true},
	} {
		if r := e.doJSON(t, http.MethodPost, "/api/admin/people", p, admCookie, admCSRF); r.Code != http.StatusOK {
			t.Fatalf("保存伙伴失败: %s", r.Body.String())
		}
	}

	// 验证 /quick-login 免密直接种 Cookie 跳首页 vs 设密跳带参登录页
	qlFree := e.doJSON(t, http.MethodGet, "/quick-login?u=%E5%85%8D%E5%AF%86%E5%B0%8F%E7%BE%8E", nil, nil, "")
	if qlFree.Code != http.StatusFound || qlFree.Header().Get("Location") != "/" || len(qlFree.Result().Cookies()) == 0 {
		t.Fatalf("免密快捷登录重定向异常: code=%d, loc=%s", qlFree.Code, qlFree.Header().Get("Location"))
	}
	qlPwd := e.doJSON(t, http.MethodGet, "/quick-login?u=%E8%AE%BE%E5%AF%86%E5%B0%8F%E5%B8%85", nil, nil, "")
	if qlPwd.Code != http.StatusFound || !strings.HasPrefix(qlPwd.Header().Get("Location"), "/?u=") {
		t.Fatalf("设密账号快捷登录应跳转密码输入页: loc=%s", qlPwd.Header().Get("Location"))
	}
	qlEmpty := e.doJSON(t, http.MethodGet, "/quick-login", nil, nil, "")
	if qlEmpty.Code != http.StatusFound || qlEmpty.Header().Get("Location") != "/" {
		t.Fatal("空参 quick-login 应跳回 /")
	}

	// 3) 管理员一键清除「设密小帅」的密码
	people, _ := e.store.ListPeople(ctx, true)
	posts, _ := e.store.ListPosts(ctx, true)
	postID := posts[0].ID
	var meiID, shuaiID int64
	for _, p := range people {
		if p.Nickname == "免密小美" {
			meiID = p.ID
		}
		if p.Nickname == "设密小帅" {
			shuaiID = p.ID
		}
	}
	if r := e.doJSON(t, http.MethodPost, "/api/admin/people/clear-password", map[string]any{"person_id": shuaiID}, admCookie, admCSRF); r.Code != http.StatusOK {
		t.Fatalf("一键清除密码接口失败: %s", r.Body.String())
	}

	// 普通成员登录：测试修改自己密码 + 越权访问管理员接口返回 403
	meiCookie, meiCSRF := loginViaAPI(t, e, "免密小美", "")
	if r := e.doJSON(t, http.MethodPost, "/api/admin/backup", map[string]any{}, meiCookie, meiCSRF); r.Code != http.StatusForbidden {
		t.Fatalf("普通成员访问管理员接口应返回 403，实际 %d", r.Code)
	}
	if r := e.doJSON(t, http.MethodPost, "/api/me/password", map[string]any{"new_password": "mei8888"}, meiCookie, meiCSRF); r.Code != http.StatusOK {
		t.Fatalf("成员设置密码失败: %s", r.Body.String())
	}

	// 4) 排班查询（含轮询草稿）、保存排班、导入排班
	if r := e.doJSON(t, http.MethodGet, "/api/admin/schedule?days=3&draft=1", nil, admCookie, ""); r.Code != http.StatusOK {
		t.Fatal("获取排班草稿失败")
	}
	if r := e.doJSON(t, http.MethodPost, "/api/admin/schedule", map[string]any{
		"entries": []map[string]any{{"schedule_date": "2026-11-25", "post_id": postID, "person_id": meiID}},
	}, admCookie, admCSRF); r.Code != http.StatusOK {
		t.Fatal("保存排班失败")
	}
	if r := e.doJSON(t, http.MethodPost, "/api/admin/schedule/import", map[string]any{
		"text": "2026-11-26,前台接待岗,设密小帅",
	}, admCookie, admCSRF); r.Code != http.StatusOK {
		t.Fatalf("导入排班失败: %s", r.Body.String())
	}

	// 5) 新增周期模板、查询模板、手动触发周期生成
	if r := e.doJSON(t, http.MethodPost, "/api/admin/templates", map[string]any{
		"post_id": postID, "title": "早班吧台消毒", "cycle_type": "daily", "need_review": true,
	}, admCookie, admCSRF); r.Code != http.StatusOK {
		t.Fatalf("新增周期模板失败: %s", r.Body.String())
	}
	if r := e.doJSON(t, http.MethodGet, "/api/admin/templates", nil, admCookie, ""); r.Code != http.StatusOK {
		t.Fatal("查询模板列表失败")
	}
	if r := e.doJSON(t, http.MethodPost, "/api/admin/generate", map[string]any{}, admCookie, admCSRF); r.Code != http.StatusOK {
		t.Fatal("手动触发生成失败")
	}

	// 6) 派发临时任务、任务列表查询、交接、标记无此项、撤销无此项、完成、撤销完成、取消临时任务
	adhocRes := e.doJSON(t, http.MethodPost, "/api/adhoc", map[string]any{
		"title": "临时接货清点", "assignee_id": meiID, "need_review": true, "reviewer_id": shuaiID,
	}, admCookie, admCSRF)
	if adhocRes.Code != http.StatusOK {
		t.Fatalf("派发临时任务失败: %s", adhocRes.Body.String())
	}
	var adhocData struct {
		ID int64 `json:"id"`
	}
	_ = json.Unmarshal(adhocRes.Body.Bytes(), &adhocData)

	if r := e.doJSON(t, http.MethodGet, "/api/tasks?scope=all", nil, meiCookie, ""); r.Code != http.StatusOK {
		t.Fatal("查询任务列表失败")
	}
	if r := e.doJSON(t, http.MethodPost, "/api/tasks/not-applicable", map[string]any{"task_id": adhocData.ID, "note": "货未到"}, meiCookie, meiCSRF); r.Code != http.StatusOK {
		t.Fatalf("标记无此项失败: %s", r.Body.String())
	}
	if r := e.doJSON(t, http.MethodPost, "/api/tasks/revert-na", map[string]any{"task_id": adhocData.ID}, meiCookie, meiCSRF); r.Code != http.StatusOK {
		t.Fatalf("撤销无此项失败: %s", r.Body.String())
	}
	if r := e.doJSON(t, http.MethodPost, "/api/tasks/handover", map[string]any{
		"task_ids": []int64{adhocData.ID}, "to_person_id": shuaiID, "note": "拜托代收",
	}, meiCookie, meiCSRF); r.Code != http.StatusOK {
		t.Fatalf("交接任务失败: %s", r.Body.String())
	}
	if r := e.doJSON(t, http.MethodGet, "/api/handovers", nil, meiCookie, ""); r.Code != http.StatusOK {
		t.Fatal("查询交接记录失败")
	}
	if r := e.doJSON(t, http.MethodPost, "/api/tasks/complete", map[string]any{"task_id": adhocData.ID}, admCookie, admCSRF); r.Code != http.StatusOK {
		t.Fatal("完成任务失败")
	}
	if r := e.doJSON(t, http.MethodPost, "/api/tasks/revert-complete", map[string]any{"task_id": adhocData.ID}, admCookie, admCSRF); r.Code != http.StatusOK {
		t.Fatal("撤销完成失败")
	}
	if r := e.doJSON(t, http.MethodPost, "/api/tasks/cancel", map[string]any{"task_id": adhocData.ID}, admCookie, admCSRF); r.Code != http.StatusOK {
		t.Fatal("取消临时任务失败")
	}
	// 对已取消任务再次点击完成应返回 409 Conflict (ErrInvalidStatus)
	if r := e.doJSON(t, http.MethodPost, "/api/tasks/complete", map[string]any{"task_id": adhocData.ID}, admCookie, admCSRF); r.Code != http.StatusConflict {
		t.Fatalf("已取消任务完成应返回 409 Conflict，实际 %d", r.Code)
	}

	// 7) 历史手账、手动备份、端口热切换与退出登录
	if r := e.doJSON(t, http.MethodGet, "/api/admin/history-book", nil, admCookie, ""); r.Code != http.StatusOK {
		t.Fatal("获取历史手账失败")
	}
	if r := e.doJSON(t, http.MethodPost, "/api/admin/backup", map[string]any{}, admCookie, admCSRF); r.Code != http.StatusOK {
		t.Fatal("手动安全备份失败")
	}

	// 未注入 portChanger 时返回 500；注入后成功切换端口并校验端口范围
	if r := e.doJSON(t, http.MethodPost, "/api/admin/port", map[string]any{"port": "80"}, admCookie, admCSRF); r.Code != http.StatusBadRequest {
		t.Fatal("小于1024端口应返回 400")
	}
	if r := e.doJSON(t, http.MethodPost, "/api/admin/port", map[string]any{"port": "8088"}, admCookie, admCSRF); r.Code != http.StatusInternalServerError {
		t.Fatal("未设置 portChanger 时应返回 500")
	}
	e.srv.SetPortChanger(func(newPort string) error {
		e.srv.SetPort(newPort)
		return nil
	})
	if r := e.doJSON(t, http.MethodPost, "/api/admin/port", map[string]any{"port": "8088"}, admCookie, admCSRF); r.Code != http.StatusOK || e.srv.GetPort() != "8088" {
		t.Fatal("在线热切换端口接口失败")
	}

	// 畸形 JSON 请求体统一拦截测试
	if r := e.doJSON(t, http.MethodPost, "/api/adhoc", "{bad_json", admCookie, admCSRF); r.Code != http.StatusBadRequest {
		t.Fatal("非法 JSON 应返回 400")
	}

	// 退出登录销毁会话
	if r := e.doJSON(t, http.MethodPost, "/api/logout", map[string]any{}, meiCookie, meiCSRF); r.Code != http.StatusOK {
		t.Fatal("退出登录失败")
	}
}

// 2. 测试登录防爆破限流器（连续 6 次密码错误锁定 60 秒 + 超过 128 条过期 IP 自动懒清理）
func TestLoginRateLimiterAndLazyCleanup(t *testing.T) {
	e := newAPITestEnv(t)

	// 预塞入 130 条超过 30 分钟的陈旧 IP 记录，验证防内存泄漏懒清理
	e.srv.rateMu.Lock()
	for i := 0; i < 130; i++ {
		e.srv.attempts["10.0.0."+strconv.Itoa(i)] = &loginAttempt{
			fails:    1,
			lastSeen: time.Now().Add(-40 * time.Minute),
		}
	}
	e.srv.rateMu.Unlock()

	for i := 0; i < 6; i++ {
		r := e.doJSON(t, http.MethodPost, "/api/login", map[string]string{
			"nickname": "主机管理员",
			"password": "wrong_password",
		}, nil, "")
		if r.Code != http.StatusUnauthorized {
			t.Fatalf("第 %d 次错密应返回 401，实际 %d", i+1, r.Code)
		}
	}

	// 第 7 次请求必须被限流器直接拦截返回 429 Too Many Requests
	lockedRes := e.doJSON(t, http.MethodPost, "/api/login", map[string]string{
		"nickname": "主机管理员",
		"password": DefaultAdminInitialPassword,
	}, nil, "")
	if lockedRes.Code != http.StatusTooManyRequests {
		t.Fatalf("连续 6 次失败后应触发 429 锁定，实际 %d", lockedRes.Code)
	}
}

// 3. 测试二维码 SVG 生成器全版本规格（Version 1 ~ Version 5 含定位校正块）及超长报错 + 数据库断开健康检查降级
func TestQRAllVersionsAndUnhealthyCheck(t *testing.T) {
	e := newAPITestEnv(t)

	// 分别覆盖 QR Version 1 (<=17B), Ver 2 (<=32B), Ver 3 (<=53B), Ver 4 (<=78B), Ver 5 (<=106B)
	for _, length := range []int{10, 26, 45, 70, 100} {
		txt := strings.Repeat("A", length)
		r := e.doJSON(t, http.MethodGet, "/api/qr?text="+txt, nil, nil, "")
		if r.Code != http.StatusOK || !strings.Contains(r.Body.String(), "<svg") {
			t.Fatalf("长度 %d 的二维码生成失败: code=%d", length, r.Code)
		}
	}
	// user 参数分支
	if r := e.doJSON(t, http.MethodGet, "/api/qr?user=张三", nil, nil, ""); r.Code != http.StatusOK {
		t.Fatal("专属用户二维码生成失败")
	}
	// 超过 106 字节应安全报错 400
	tooLong := strings.Repeat("X", 110)
	if r := e.doJSON(t, http.MethodGet, "/api/qr?text="+tooLong, nil, nil, ""); r.Code != http.StatusBadRequest {
		t.Fatalf("超长二维码应返回 400，实际 %d", r.Code)
	}

	// 关闭底层数据库后，/healthz 应返回 503 Service Unavailable
	_ = e.store.Close()
	if r := e.doJSON(t, http.MethodGet, "/healthz", nil, nil, ""); r.Code != http.StatusServiceUnavailable {
		t.Fatalf("数据库关闭后 /healthz 应返回 503，实际 %d", r.Code)
	}
}
