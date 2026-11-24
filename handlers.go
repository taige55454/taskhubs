package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

const sessionCookieName = "taskhub_sid"

type Server struct {
	mu          sync.RWMutex
	store       *Store
	port        string
	portChanger func(newPort string) error
	logger      *slog.Logger
}

func NewServer(store *Store, port string, logger *slog.Logger) *Server {
	return &Server{
		store:  store,
		port:   port,
		logger: logger,
	}
}

func (s *Server) SetPortChanger(fn func(newPort string) error) {
	s.mu.Lock()
	s.portChanger = fn
	s.mu.Unlock()
}

func (s *Server) SetPort(port string) {
	s.mu.Lock()
	s.port = port
	s.mu.Unlock()
}

func (s *Server) GetPort() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.port
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /quick-login", s.handleQuickLogin)
	mux.HandleFunc("GET /api/qr", s.handleQR)

	mux.HandleFunc("GET /api/bootstrap", s.handleBootstrap)
	mux.HandleFunc("POST /api/login", s.handleLogin)
	mux.HandleFunc("POST /api/logout", s.withAuth(s.handleLogout, false))
	mux.HandleFunc("POST /api/me/password", s.withAuth(s.handleChangeOwnPassword, false))

	mux.HandleFunc("GET /api/tasks", s.withAuth(s.handleListTasks, false))
	mux.HandleFunc("POST /api/tasks/complete", s.withAuth(s.handleCompleteTask, false))
	mux.HandleFunc("POST /api/tasks/not-applicable", s.withAuth(s.handleMarkNA, false))
	mux.HandleFunc("POST /api/tasks/revert-na", s.withAuth(s.handleRevertNA, false))
	mux.HandleFunc("POST /api/tasks/revert-complete", s.withAuth(s.handleRevertComplete, true))
	mux.HandleFunc("POST /api/tasks/cancel", s.withAuth(s.handleCancelAdhoc, true))
	mux.HandleFunc("POST /api/tasks/handover", s.withAuth(s.handleHandover, false))
	mux.HandleFunc("GET /api/handovers", s.withAuth(s.handleListHandovers, false))

	// 主机管理端专属接口
	mux.HandleFunc("POST /api/adhoc", s.withAuth(s.handleCreateAdhoc, true))
	mux.HandleFunc("POST /api/admin/generate", s.withAuth(s.handleManualGenerate, true))
	mux.HandleFunc("GET /api/admin/schedule", s.withAuth(s.handleGetSchedule, true))
	mux.HandleFunc("POST /api/admin/schedule", s.withAuth(s.handleSaveSchedule, true))
	mux.HandleFunc("POST /api/admin/schedule/import", s.withAuth(s.handleImportSchedule, true))
	mux.HandleFunc("GET /api/admin/history-book", s.withAuth(s.handleHistoryBook, true))
	mux.HandleFunc("POST /api/admin/people", s.withAuth(s.handleSavePerson, true))
	mux.HandleFunc("POST /api/admin/posts", s.withAuth(s.handleSavePost, true))
	mux.HandleFunc("GET /api/admin/templates", s.withAuth(s.handleListTemplates, true))
	mux.HandleFunc("POST /api/admin/templates", s.withAuth(s.handleSaveTemplate, true))
	mux.HandleFunc("POST /api/admin/backup", s.withAuth(s.handleManualBackup, true))
	mux.HandleFunc("POST /api/admin/port", s.withAuth(s.handleChangePort, true))

	subFS, _ := fs.Sub(webFS, "web")
	fileServer := http.FileServer(http.FS(subFS))
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/sw.js" {
			w.Header().Set("Cache-Control", "no-cache")
		}
		fileServer.ServeHTTP(w, r)
	}))

	return mux
}

type authedHandler func(w http.ResponseWriter, r *http.Request, sess *SessionInfo)

func (s *Server) withAuth(next authedHandler, adminOnly bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(sessionCookieName)
		if err != nil {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "请先登录"})
			return
		}
		sess, err := s.store.GetSession(r.Context(), c.Value)
		if err != nil {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "登录状态已失效，请重新登录"})
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			csrf := r.Header.Get("X-CSRF-Token")
			if csrf == "" || csrf != sess.CSRFToken {
				writeJSON(w, http.StatusForbidden, map[string]any{"error": "CSRF 安全校验失败，请刷新页面"})
				return
			}
		}
		if adminOnly && !sess.Person.IsAdmin {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "此操作仅限主机管理员执行"})
			return
		}
		next(w, r, sess)
	}
}

func setSessionCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/",
		MaxAge:   365 * 24 * 3600,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

func writeJSON(w http.ResponseWriter, code int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(payload)
}

func mapErrStatus(err error) int {
	switch {
	case errors.Is(err, ErrBadInput):
		return http.StatusBadRequest
	case errors.Is(err, ErrAuthFailed):
		return http.StatusUnauthorized
	case errors.Is(err, ErrForbidden):
		return http.StatusForbidden
	case errors.Is(err, ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, ErrInvalidStatus):
		return http.StatusConflict
	default:
		return http.StatusBadRequest
	}
}

// ==================== Handlers ====================

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if err := s.store.readDB.PingContext(r.Context()); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "unhealthy", "error": err.Error()})
		return
	}
	exe, _ := os.Executable()
	writeJSON(w, http.StatusOK, map[string]any{
		"status":        "ok",
		"business_date": s.store.TodayStr(),
		"timezone":      s.store.loc.String(),
		"port":          s.GetPort(),
		"process_name":  filepath.Base(exe),
		"pid":           os.Getpid(),
	})
}

func (s *Server) handleBootstrap(w http.ResponseWriter, r *http.Request) {
	peopleAll, _ := s.store.ListPeople(r.Context(), false)
	postsAll, _ := s.store.ListPosts(r.Context(), false)

	var currUser *Person
	var csrf string
	if c, err := r.Cookie(sessionCookieName); err == nil {
		if sess, err := s.store.GetSession(r.Context(), c.Value); err == nil {
			currUser = &sess.Person
			csrf = sess.CSRFToken
		}
	}

	port := s.GetPort()
	lanIP := detectLANIP()
	lanBase := fmt.Sprintf("http://%s:%s", lanIP, port)
	exe, _ := os.Executable()

	var backups []string
	if currUser != nil && currUser.IsAdmin {
		backups, _ = s.store.ListBackups()
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"business_date":     s.store.TodayStr(),
		"timezone":          s.store.loc.String(),
		"port":              port,
		"lan_ip":            lanIP,
		"lan_url":           lanBase,
		"process_name":      filepath.Base(exe),
		"pid":               os.Getpid(),
		"default_admin_pwd": s.store.IsUsingDefaultAdminPwd(r.Context()),
		"current_user":      currUser,
		"csrf_token":        csrf,
		"people":            peopleAll,
		"posts":             postsAll,
		"backups":           backups,
	})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Nickname string `json:"nickname"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求格式错误"})
		return
	}
	sess, err := s.store.Login(r.Context(), req.Nickname, req.Password)
	if err != nil {
		writeJSON(w, mapErrStatus(err), map[string]any{"error": err.Error()})
		return
	}
	setSessionCookie(w, sess.Token)
	writeJSON(w, http.StatusOK, map[string]any{
		"user":       sess.Person,
		"csrf_token": sess.CSRFToken,
	})
}

func (s *Server) handleQuickLogin(w http.ResponseWriter, r *http.Request) {
	nickname := strings.TrimSpace(r.URL.Query().Get("u"))
	if nickname != "" {
		// 仅未设密码的普通成员可直接免密扫码登录；已设密码或管理员跳转登录页唤起密码框
		if s.store.CanPasswordlessQuickLogin(r.Context(), nickname) {
			if sess, err := s.store.Login(r.Context(), nickname, ""); err == nil {
				setSessionCookie(w, sess.Token)
				http.Redirect(w, r, "/", http.StatusFound)
				return
			}
		}
		http.Redirect(w, r, "/?u="+url.QueryEscape(nickname), http.StatusFound)
		return
	}
	http.Redirect(w, r, "/", http.StatusFound)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request, sess *SessionInfo) {
	_ = s.store.DeleteSession(r.Context(), sess.Token)
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleChangeOwnPassword(w http.ResponseWriter, r *http.Request, sess *SessionInfo) {
	var req struct {
		OldPassword   string `json:"old_password"`
		NewPassword   string `json:"new_password"`
		ClearPassword bool   `json:"clear_password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "参数错误"})
		return
	}
	if err := s.store.ChangeOwnPassword(r.Context(), sess.Person.ID, req.OldPassword, req.NewPassword, req.ClearPassword); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleListTasks(w http.ResponseWriter, r *http.Request, sess *SessionInfo) {
	q := r.URL.Query()
	date := q.Get("date")
	if date == "" {
		date = s.store.TodayStr()
	}
	scope := q.Get("scope")
	if scope == "" {
		scope = "mine"
	}
	postID, _ := strconv.ParseInt(q.Get("post_id"), 10, 64)

	tasks, err := s.store.ListTasks(r.Context(), date, scope, sess.Person.ID, postID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	stats, _ := s.store.GetDailyStats(r.Context(), date)
	_, schedules, _ := s.store.GetScheduleRange(r.Context(), date, 1)
	writeJSON(w, http.StatusOK, map[string]any{
		"business_date": s.store.TodayStr(),
		"query_date":    date,
		"tasks":         tasks,
		"stats":         stats,
		"schedules":     schedules,
	})
}

func (s *Server) handleCompleteTask(w http.ResponseWriter, r *http.Request, sess *SessionInfo) {
	var req struct {
		TaskID      int64 `json:"task_id"`
		CompletedBy int64 `json:"completed_by"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "参数错误"})
		return
	}
	t, err := s.store.CompleteTask(r.Context(), req.TaskID, req.CompletedBy, sess.Person)
	if err != nil {
		writeJSON(w, mapErrStatus(err), map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"task": t})
}

func (s *Server) handleMarkNA(w http.ResponseWriter, r *http.Request, sess *SessionInfo) {
	var req struct {
		TaskID int64  `json:"task_id"`
		Note   string `json:"note"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "参数错误"})
		return
	}
	t, err := s.store.MarkNotApplicable(r.Context(), req.TaskID, req.Note, sess.Person)
	if err != nil {
		writeJSON(w, mapErrStatus(err), map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"task": t})
}

func (s *Server) handleRevertNA(w http.ResponseWriter, r *http.Request, sess *SessionInfo) {
	var req struct {
		TaskID int64 `json:"task_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "参数错误"})
		return
	}
	t, err := s.store.RevertNotApplicable(r.Context(), req.TaskID, sess.Person, s.store.Now())
	if err != nil {
		writeJSON(w, mapErrStatus(err), map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"task": t})
}

func (s *Server) handleRevertComplete(w http.ResponseWriter, r *http.Request, sess *SessionInfo) {
	var req struct {
		TaskID int64 `json:"task_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "参数错误"})
		return
	}
	t, err := s.store.RevertCompleted(r.Context(), req.TaskID, sess.Person)
	if err != nil {
		writeJSON(w, mapErrStatus(err), map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"task": t})
}

func (s *Server) handleCancelAdhoc(w http.ResponseWriter, r *http.Request, sess *SessionInfo) {
	var req struct {
		TaskID int64 `json:"task_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "参数错误"})
		return
	}
	t, err := s.store.CancelAdhocTask(r.Context(), req.TaskID, sess.Person)
	if err != nil {
		writeJSON(w, mapErrStatus(err), map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"task": t})
}

func (s *Server) handleHandover(w http.ResponseWriter, r *http.Request, sess *SessionInfo) {
	var req struct {
		TaskIDs    []int64 `json:"task_ids"`
		ToPersonID int64   `json:"to_person_id"`
		Note       string  `json:"note"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "参数错误"})
		return
	}
	cnt, err := s.store.HandoverTasks(r.Context(), req.TaskIDs, req.ToPersonID, req.Note, sess.Person)
	if err != nil {
		writeJSON(w, mapErrStatus(err), map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"handed_over": cnt})
}

func (s *Server) handleListHandovers(w http.ResponseWriter, r *http.Request, sess *SessionInfo) {
	date := r.URL.Query().Get("date")
	records, err := s.store.ListHandoverRecords(r.Context(), date)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"records": records})
}

func (s *Server) handleCreateAdhoc(w http.ResponseWriter, r *http.Request, sess *SessionInfo) {
	var req AdhocInput
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "参数错误"})
		return
	}
	id, err := s.store.CreateAdhocTask(r.Context(), req)
	if err != nil {
		writeJSON(w, mapErrStatus(err), map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id})
}

func (s *Server) handleManualGenerate(w http.ResponseWriter, r *http.Request, sess *SessionInfo) {
	n, err := s.store.GeneratePeriodicTasks(r.Context(), s.store.Now())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"generated": n})
}

func (s *Server) handleGetSchedule(w http.ResponseWriter, r *http.Request, sess *SessionInfo) {
	q := r.URL.Query()
	start := q.Get("start")
	if start == "" {
		start = s.store.TodayStr()
	}
	days, _ := strconv.Atoi(q.Get("days"))
	if days <= 0 {
		days = 7
	}
	withDraft := q.Get("draft") == "1"

	dates, entries, err := s.store.GetScheduleRange(r.Context(), start, days)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	var draft []ScheduleEntry
	if withDraft {
		draft, err = s.store.GenerateFairRotaDraft(r.Context(), dates[0], days)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"dates":   dates,
		"entries": entries,
		"draft":   draft,
	})
}

func (s *Server) handleSaveSchedule(w http.ResponseWriter, r *http.Request, sess *SessionInfo) {
	var req struct {
		Entries []ScheduleEntry `json:"entries"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "参数错误"})
		return
	}
	saved, err := s.store.SaveScheduleEntries(r.Context(), req.Entries)
	if err != nil {
		writeJSON(w, mapErrStatus(err), map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"saved": saved})
}

func (s *Server) handleImportSchedule(w http.ResponseWriter, r *http.Request, sess *SessionInfo) {
	var req struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "参数错误"})
		return
	}
	saved, err := s.store.ImportScheduleText(r.Context(), req.Text)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"saved": saved})
}

func (s *Server) handleHistoryBook(w http.ResponseWriter, r *http.Request, sess *SessionInfo) {
	q := r.URL.Query()
	date := q.Get("date")
	skipEmpty := q.Get("skip_empty") != "0"
	page, err := s.store.GetHistoryBookPage(r.Context(), date, skipEmpty)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (s *Server) handleSavePerson(w http.ResponseWriter, r *http.Request, sess *SessionInfo) {
	var req PersonSaveInput
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "参数错误"})
		return
	}
	if err := s.store.SavePerson(r.Context(), req); err != nil {
		writeJSON(w, mapErrStatus(err), map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleSavePost(w http.ResponseWriter, r *http.Request, sess *SessionInfo) {
	var req struct {
		ID        int64  `json:"id"`
		Name      string `json:"name"`
		Color     string `json:"color"`
		SortOrder int    `json:"sort_order"`
		IsActive  bool   `json:"is_active"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "参数错误"})
		return
	}
	if err := s.store.SavePost(r.Context(), req.ID, req.Name, req.Color, req.SortOrder, req.IsActive); err != nil {
		writeJSON(w, mapErrStatus(err), map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleListTemplates(w http.ResponseWriter, r *http.Request, sess *SessionInfo) {
	list, err := s.store.ListTemplates(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"templates": list})
}

func (s *Server) handleSaveTemplate(w http.ResponseWriter, r *http.Request, sess *SessionInfo) {
	var req TaskTemplate
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "参数错误"})
		return
	}
	id, err := s.store.CreateOrUpdateTemplate(r.Context(), req, s.store.Now())
	if err != nil {
		writeJSON(w, mapErrStatus(err), map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id})
}

func (s *Server) handleManualBackup(w http.ResponseWriter, r *http.Request, sess *SessionInfo) {
	fileName, err := s.store.PerformBackup(r.Context(), s.store.Now())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	backups, _ := s.store.ListBackups()
	writeJSON(w, http.StatusOK, map[string]any{"file": fileName, "backups": backups})
}

func (s *Server) handleChangePort(w http.ResponseWriter, r *http.Request, sess *SessionInfo) {
	var req struct {
		Port string `json:"port"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "参数错误"})
		return
	}
	newPort := strings.TrimSpace(req.Port)
	pNum, err := strconv.Atoi(newPort)
	if err != nil || pNum < 1024 || pNum > 65535 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "建议使用 1024 ~ 65535 之间的端口号"})
		return
	}
	s.mu.RLock()
	changer := s.portChanger
	s.mu.RUnlock()
	if changer == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "当前模式不支持在线热切换端口"})
		return
	}
	if err := changer(newPort); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	lanURL := fmt.Sprintf("http://%s:%s", detectLANIP(), newPort)
	writeJSON(w, http.StatusOK, map[string]any{
		"port":    newPort,
		"lan_url": lanURL,
	})
}

// ==================== 纯 Go 零依赖 QR Code SVG 生成器 ====================

func (s *Server) handleQR(w http.ResponseWriter, r *http.Request) {
	port := s.GetPort()
	lanBase := fmt.Sprintf("http://%s:%s", detectLANIP(), port)
	text := strings.TrimSpace(r.URL.Query().Get("text"))
	if u := r.URL.Query().Get("user"); u != "" {
		text = fmt.Sprintf("%s/quick-login?u=%s", lanBase, url.QueryEscape(u))
	}
	if text == "" {
		text = lanBase
	}
	svg, err := encodeQRSVG([]byte(text))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "image/svg+xml; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(svg)
}

type qrVerSpec struct {
	ver       int
	size      int
	dataBytes int
	ecBytes   int
	align     []int
}

var qrSpecs = []qrVerSpec{
	{1, 21, 19, 7, nil},
	{2, 25, 34, 10, []int{6, 18}},
	{3, 29, 55, 15, []int{6, 22}},
	{4, 33, 80, 20, []int{6, 26}},
	{5, 37, 108, 26, []int{6, 30}},
}

func encodeQRSVG(payload []byte) ([]byte, error) {
	if len(payload) > 106 {
		return nil, errors.New("二维码内容过长")
	}
	var spec qrVerSpec
	for _, sp := range qrSpecs {
		if len(payload)+2 <= sp.dataBytes {
			spec = sp
			break
		}
	}

	bits := make([]byte, 0, spec.dataBytes*8)
	appendBits := func(val, length int) {
		for i := length - 1; i >= 0; i-- {
			bits = append(bits, byte((val>>i)&1))
		}
	}
	appendBits(0x4, 4)
	appendBits(len(payload), 8)
	for _, b := range payload {
		appendBits(int(b), 8)
	}
	for i := 0; i < 4 && len(bits) < spec.dataBytes*8; i++ {
		bits = append(bits, 0)
	}
	for len(bits)%8 != 0 {
		bits = append(bits, 0)
	}
	data := make([]byte, len(bits)/8)
	for i := range data {
		var v byte
		for b := 0; b < 8; b++ {
			v = (v << 1) | bits[i*8+b]
		}
		data[i] = v
	}
	pads := []byte{0xEC, 0x11}
	for p := 0; len(data) < spec.dataBytes; p++ {
		data = append(data, pads[p%2])
	}

	ec := qrReedSolomon(data, spec.ecBytes)
	fullCode := append(append([]byte{}, data...), ec...)

	n := spec.size
	mod := make([][]bool, n)
	isFunc := make([][]bool, n)
	for i := range mod {
		mod[i] = make([]bool, n)
		isFunc[i] = make([]bool, n)
	}
	setFunc := func(x, y int, dark bool) {
		if x >= 0 && x < n && y >= 0 && y < n {
			mod[y][x] = dark
			isFunc[y][x] = true
		}
	}
	drawFinder := func(cx, cy int) {
		for dy := -4; dy <= 4; dy++ {
			for dx := -4; dx <= 4; dx++ {
				dist := max(absInt(dx), absInt(dy))
				setFunc(cx+dx, cy+dy, dist != 2 && dist != 4)
			}
		}
	}
	drawFinder(3, 3)
	drawFinder(n-4, 3)
	drawFinder(3, n-4)

	for i := 0; i < n; i++ {
		if !isFunc[6][i] {
			setFunc(i, 6, i%2 == 0)
		}
		if !isFunc[i][6] {
			setFunc(6, i, i%2 == 0)
		}
	}
	if len(spec.align) > 0 {
		cx, cy := spec.align[1], spec.align[1]
		for dy := -2; dy <= 2; dy++ {
			for dx := -2; dx <= 2; dx++ {
				dist := max(absInt(dx), absInt(dy))
				setFunc(cx+dx, cy+dy, dist != 1)
			}
		}
	}
	for i := 0; i <= 8; i++ {
		if i != 6 {
			isFunc[8][i] = true
			isFunc[i][8] = true
		}
	}
	for i := 0; i < 8; i++ {
		isFunc[8][n-1-i] = true
		isFunc[n-1-i][8] = true
	}
	setFunc(8, n-8, true)

	bitIdx := 0
	totalBits := len(fullCode) * 8
	up := true
	for right := n - 1; right >= 1; right -= 2 {
		if right == 6 {
			right = 5
		}
		for vert := 0; vert < n; vert++ {
			y := vert
			if up {
				y = n - 1 - vert
			}
			for j := 0; j < 2; j++ {
				x := right - j
				if !isFunc[y][x] {
					dark := false
					if bitIdx < totalBits {
						dark = ((fullCode[bitIdx/8] >> (7 - (bitIdx % 8))) & 1) == 1
						bitIdx++
					}
					if (x+y)%2 == 0 {
						dark = !dark
					}
					mod[y][x] = dark
				}
			}
		}
		up = !up
	}

	const fmtBits = 0x77C4
	getFmt := func(i int) bool { return ((fmtBits >> i) & 1) == 1 }
	for i := 0; i <= 5; i++ {
		mod[8][i] = getFmt(i)
	}
	mod[8][7] = getFmt(6)
	mod[8][8] = getFmt(7)
	mod[7][8] = getFmt(8)
	for i := 9; i < 15; i++ {
		mod[14-i][8] = getFmt(i)
	}
	for i := 0; i < 8; i++ {
		mod[8][n-1-i] = getFmt(i)
	}
	for i := 8; i < 15; i++ {
		mod[n-15+i][8] = getFmt(i)
	}

	margin := 3
	dim := n + margin*2
	var buf bytes.Buffer
	fmt.Fprintf(&buf, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" shape-rendering="crispEdges">`, dim, dim)
	fmt.Fprintf(&buf, `<rect width="%d" height="%d" fill="#FFFFFF" rx="2"/>`, dim, dim)
	buf.WriteString(`<path fill="#1D1D1F" d="`)
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			if mod[y][x] {
				fmt.Fprintf(&buf, "M%d,%dh1v1h-1z", x+margin, y+margin)
			}
		}
	}
	buf.WriteString(`"/></svg>`)
	return buf.Bytes(), nil
}

func qrReedSolomon(data []byte, degree int) []byte {
	gfMul := func(x, y byte) byte {
		var z byte
		for i := 7; i >= 0; i-- {
			z = (z << 1) ^ ((z >> 7) * 0x1D)
			z ^= ((y >> i) & 1) * x
		}
		return z
	}
	gen := make([]byte, degree)
	gen[degree-1] = 1
	root := byte(1)
	for i := 0; i < degree; i++ {
		for j := 0; j < degree; j++ {
			gen[j] = gfMul(gen[j], root)
			if j+1 < degree {
				gen[j] ^= gen[j+1]
			}
		}
		root = gfMul(root, 0x02)
	}
	rem := make([]byte, degree)
	for _, b := range data {
		factor := b ^ rem[0]
		copy(rem, rem[1:])
		rem[degree-1] = 0
		for j := 0; j < degree; j++ {
			rem[j] ^= gfMul(gen[j], factor)
		}
	}
	return rem
}

func absInt(x int) int {
	if x < 0 {
		return -x
	}
	return x
}
