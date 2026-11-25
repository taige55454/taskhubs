package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

const (
	DefaultAdminInitialPassword = "admin123"
	FixedReviewColor            = "#D97706" // 次日接班复核任务全局固定琥珀金标签色
)

var (
	ErrForbidden     = errors.New("无权执行此操作")
	ErrInvalidStatus = errors.New("当前任务状态不允许此操作")
	ErrBadInput      = errors.New("请求参数不合法")
	ErrNotFound      = errors.New("目标记录不存在")
	ErrAuthFailed    = errors.New("密码错误或需要输入密码")
)

type Store struct {
	writeDB   *sql.DB
	readDB    *sql.DB
	dbPath    string
	backupDir string
	loc       *time.Location
	logger    *slog.Logger

	clockMu sync.RWMutex
	nowFunc func() time.Time
}

type Person struct {
	ID            int64  `json:"id"`
	Nickname      string `json:"nickname"`
	IsAdmin       bool   `json:"is_admin"`
	InRota        bool   `json:"in_rota"`
	HasPassword   bool   `json:"has_password"`
	RestWeekdays  string `json:"rest_weekdays"`   // 逗号分隔 0-6, 如 "0,6" 代表周末休息
	RestMonthDays string `json:"rest_month_days"` // 逗号分隔, 支持 "5,18" (结合 rest_month) 或 "2026-11-28"
	RestMonth     string `json:"rest_month"`      // 记录设置当月如 "2026-11", 防止纯日号跨月误判
	IsActive      bool   `json:"is_active"`
	CreatedAt     string `json:"created_at"`
}

type Post struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Color     string `json:"color"`
	SortOrder int    `json:"sort_order"`
	IsActive  bool   `json:"is_active"`
	CreatedAt string `json:"created_at"`
}

type TaskTemplate struct {
	ID          int64  `json:"id"`
	PostID      int64  `json:"post_id"`
	PostName    string `json:"post_name"`
	PostColor   string `json:"post_color"`
	PostSort    int    `json:"post_sort"`
	Title       string `json:"title"`
	Description string `json:"description"`
	CycleType   string `json:"cycle_type"` // daily | weekly | monthly
	Weekdays    string `json:"weekdays"`
	MonthDays   string `json:"month_days"`
	NeedReview  bool   `json:"need_review"` // 是否需次日接班伙伴复核
	IsActive    bool   `json:"is_active"`
	CreatedAt   string `json:"created_at"`
}

type TaskInstance struct {
	ID                 int64   `json:"id"`
	Source             string  `json:"source"` // template | adhoc | review
	TemplateID         *int64  `json:"template_id"`
	PostID             *int64  `json:"post_id"`
	PostName           string  `json:"post_name"`
	PostColor          string  `json:"post_color"`
	BadgeColor         string  `json:"badge_color"` // 普通任务等于岗位色，复核任务固定为 #D97706
	Title              string  `json:"title"`
	Description        string  `json:"description"`
	PeriodKey          *string `json:"period_key"`
	BusinessDate       string  `json:"business_date"`
	NeedReview         bool    `json:"need_review"`
	ReviewOfTaskID     *int64  `json:"review_of_task_id"`
	ParentDate         string  `json:"parent_date,omitempty"`
	ParentStatus       string  `json:"parent_status,omitempty"`
	ParentAssigneeName string  `json:"parent_assignee_name,omitempty"`
	ParentCompletedBy  string  `json:"parent_completed_by,omitempty"`
	ParentCompletedAt  string  `json:"parent_completed_at,omitempty"`
	OriginalAssigneeID *int64  `json:"original_assignee_id"`
	OriginalName       string  `json:"original_assignee_name"`
	CurrentAssigneeID  *int64  `json:"current_assignee_id"`
	CurrentName        string  `json:"current_assignee_name"`
	Status             string  `json:"status"` // pending | completed | not_applicable | cancelled
	CompletedBy        *int64  `json:"completed_by"`
	CompletedByName    string  `json:"completed_by_name"`
	CompletedAt        *string `json:"completed_at"`
	NotApplicableBy    *int64  `json:"not_applicable_by"`
	NotApplicableName  string  `json:"not_applicable_by_name"`
	NotApplicableAt    *string `json:"not_applicable_at"`
	NotApplicableNote  *string `json:"not_applicable_note"`
	CancelledBy        *int64  `json:"cancelled_by"`
	CancelledAt        *string `json:"cancelled_at"`
	CreatedAt          string  `json:"created_at"`
}

type HandoverRecord struct {
	ID             int64  `json:"id"`
	TaskInstanceID int64  `json:"task_instance_id"`
	TaskTitle      string `json:"task_title"`
	PostName       string `json:"post_name"`
	PostColor      string `json:"post_color"`
	FromPersonID   *int64 `json:"from_person_id"`
	FromName       string `json:"from_name"`
	ToPersonID     int64  `json:"to_person_id"`
	ToName         string `json:"to_name"`
	OperatedBy     int64  `json:"operated_by"`
	OperatedByName string `json:"operated_by_name"`
	Note           string `json:"note"`
	CreatedAt      string `json:"created_at"`
}

type ScheduleEntry struct {
	ScheduleDate string `json:"schedule_date"`
	PostID       int64  `json:"post_id"`
	PostName     string `json:"post_name,omitempty"`
	PersonID     int64  `json:"person_id"`
	PersonName   string `json:"person_name,omitempty"`
}

type DailyStats struct {
	BusinessDate  string  `json:"business_date"`
	Completed     int     `json:"completed"`
	Pending       int     `json:"pending"`
	NotApp        int     `json:"not_applicable"`
	Cancelled     int     `json:"cancelled"`
	RateStrict    float64 `json:"rate_strict"`
	RateInclusive float64 `json:"rate_inclusive"`
}

type HistoryChapterDot struct {
	Date      string `json:"date"`
	Completed int    `json:"completed"`
	Pending   int    `json:"pending"`
	NotApp    int    `json:"not_applicable"`
}

type HistoryBookPage struct {
	Date       string              `json:"date"`
	PrevDate   string              `json:"prev_date"`
	NextDate   string              `json:"next_date"`
	Stats      DailyStats          `json:"stats"`
	Tasks      []TaskInstance      `json:"tasks"`
	Handovers  []HandoverRecord    `json:"handovers"`
	Schedules  []ScheduleEntry     `json:"schedules"`
	ChapterBar []HistoryChapterDot `json:"chapter_bar"`
}

type SessionInfo struct {
	Token     string
	CSRFToken string
	Person    Person
}

func buildDSN(dbPath string, readOnly bool) string {
	pragmas := []string{
		"_pragma=journal_mode(WAL)",
		"_pragma=synchronous(NORMAL)",
		"_pragma=busy_timeout(5000)",
		"_pragma=foreign_keys(ON)",
		"_pragma=temp_store(MEMORY)",
		"_pragma=cache_size(-32000)", // 32MB 页缓存，兼顾性能与低内存占用
	}
	if readOnly {
		pragmas = append(pragmas, "_pragma=query_only(ON)")
	}
	return fmt.Sprintf("file:%s?%s", filepath.ToSlash(dbPath), strings.Join(pragmas, "&"))
}

func NewStore(dbPath, backupDir string, loc *time.Location, logger *slog.Logger) (*Store, error) {
	f, err := os.OpenFile(dbPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("数据库文件不可写: %w", err)
	}
	_ = f.Close()
	_ = os.Chmod(dbPath, 0600)

	writeDB, err := sql.Open("sqlite", buildDSN(dbPath, false))
	if err != nil {
		return nil, err
	}
	writeDB.SetMaxOpenConns(1)
	writeDB.SetMaxIdleConns(1)
	writeDB.SetConnMaxLifetime(0)

	if err := initAndMigrateSchema(writeDB); err != nil {
		_ = writeDB.Close()
		return nil, fmt.Errorf("初始化数据库表结构失败: %w", err)
	}

	readDB, err := sql.Open("sqlite", buildDSN(dbPath, true))
	if err != nil {
		_ = writeDB.Close()
		return nil, err
	}
	readDB.SetMaxOpenConns(6)
	readDB.SetMaxIdleConns(3)
	readDB.SetConnMaxIdleTime(10 * time.Minute)

	s := &Store{
		writeDB:   writeDB,
		readDB:    readDB,
		dbPath:    dbPath,
		backupDir: backupDir,
		loc:       loc,
		logger:    logger,
		nowFunc:   func() time.Time { return time.Now().In(loc) },
	}

	if err := s.SelfCheck(context.Background()); err != nil {
		_ = s.Close()
		return nil, err
	}
	if err := s.ensureBootstrapAdmin(context.Background()); err != nil {
		_ = s.Close()
		return nil, err
	}
	return s, nil
}

// SetClock 供单元测试安全注入模拟时钟，避免测试受物理机器当前日期干扰
func (s *Store) SetClock(fn func() time.Time) {
	s.clockMu.Lock()
	defer s.clockMu.Unlock()
	if fn == nil {
		s.nowFunc = func() time.Time { return time.Now().In(s.loc) }
	} else {
		s.nowFunc = fn
	}
}

func (s *Store) Now() time.Time {
	s.clockMu.RLock()
	fn := s.nowFunc
	s.clockMu.RUnlock()
	if fn != nil {
		return fn().In(s.loc)
	}
	return time.Now().In(s.loc)
}

func (s *Store) NowISO() string   { return s.Now().Format(time.RFC3339) }
func (s *Store) TodayStr() string { return s.Now().Format("2006-01-02") }

func (s *Store) SelfCheck(ctx context.Context) error {
	info, err := os.Stat(s.backupDir)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("自检失败: 备份目录不存在 (%s)", s.backupDir)
	}
	var journalMode string
	if err := s.writeDB.QueryRowContext(ctx, "PRAGMA journal_mode;").Scan(&journalMode); err != nil {
		return fmt.Errorf("自检失败: 无法读取 journal_mode: %w", err)
	}
	if !strings.EqualFold(journalMode, "wal") {
		return fmt.Errorf("自检失败: WAL 未生效 (当前=%s)，请确认数据库位于本地磁盘", journalMode)
	}
	var fk int
	if err := s.writeDB.QueryRowContext(ctx, "PRAGMA foreign_keys;").Scan(&fk); err != nil || fk != 1 {
		return fmt.Errorf("自检失败: writeDB 外键未开启")
	}
	if err := s.readDB.QueryRowContext(ctx, "PRAGMA foreign_keys;").Scan(&fk); err != nil || fk != 1 {
		return fmt.Errorf("自检失败: readDB 外键未开启")
	}
	return s.withWriteTx(ctx, func(conn *sql.Conn) error {
		_, err := conn.ExecContext(ctx,
			`INSERT INTO app_kv(k, v) VALUES('last_startup_check', ?)
			 ON CONFLICT(k) DO UPDATE SET v = excluded.v`, s.NowISO())
		return err
	})
}

func (s *Store) Close() error {
	_, _ = s.writeDB.Exec("PRAGMA wal_checkpoint(TRUNCATE);")
	_, _ = s.writeDB.Exec("PRAGMA optimize;")
	err1 := s.readDB.Close()
	err2 := s.writeDB.Close()
	if err1 != nil {
		return err1
	}
	return err2
}

func initAndMigrateSchema(db *sql.DB) error {
	ddl := `
CREATE TABLE IF NOT EXISTS app_kv (
	k TEXT PRIMARY KEY,
	v TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS person (
	id              INTEGER PRIMARY KEY AUTOINCREMENT,
	nickname        TEXT    NOT NULL,
	password_hash   TEXT    NOT NULL DEFAULT '',
	is_admin        INTEGER NOT NULL DEFAULT 0 CHECK (is_admin IN (0, 1)),
	in_rota         INTEGER NOT NULL DEFAULT 1 CHECK (in_rota IN (0, 1)),
	rest_weekdays   TEXT    NOT NULL DEFAULT '',
	rest_month_days TEXT    NOT NULL DEFAULT '',
	rest_month      TEXT    NOT NULL DEFAULT '',
	is_active       INTEGER NOT NULL DEFAULT 1 CHECK (is_active IN (0, 1)),
	created_at      TEXT    NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS uk_person_nickname ON person(nickname);

CREATE TABLE IF NOT EXISTS post (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	name        TEXT    NOT NULL,
	color       TEXT    NOT NULL DEFAULT '#007AFF',
	sort_order  INTEGER NOT NULL DEFAULT 0,
	is_active   INTEGER NOT NULL DEFAULT 1 CHECK (is_active IN (0, 1)),
	created_at  TEXT    NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_post_active_sort ON post(is_active, sort_order);

CREATE TABLE IF NOT EXISTS task_template (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	post_id     INTEGER NOT NULL,
	title       TEXT    NOT NULL,
	description TEXT    NOT NULL DEFAULT '',
	cycle_type  TEXT    NOT NULL CHECK (cycle_type IN ('daily', 'weekly', 'monthly')),
	weekdays    TEXT    NOT NULL DEFAULT '',
	month_days  TEXT    NOT NULL DEFAULT '',
	need_review INTEGER NOT NULL DEFAULT 0 CHECK (need_review IN (0, 1)),
	is_active   INTEGER NOT NULL DEFAULT 1 CHECK (is_active IN (0, 1)),
	created_at  TEXT    NOT NULL,
	FOREIGN KEY (post_id) REFERENCES post(id) ON DELETE RESTRICT
);
CREATE INDEX IF NOT EXISTS idx_template_post_active ON task_template(post_id, is_active);

CREATE TABLE IF NOT EXISTS post_schedule (
	id            INTEGER PRIMARY KEY AUTOINCREMENT,
	post_id       INTEGER NOT NULL,
	schedule_date TEXT    NOT NULL,
	person_id     INTEGER NOT NULL,
	updated_at    TEXT    NOT NULL,
	FOREIGN KEY (post_id)   REFERENCES post(id)   ON DELETE RESTRICT,
	FOREIGN KEY (person_id) REFERENCES person(id) ON DELETE RESTRICT
);
CREATE UNIQUE INDEX IF NOT EXISTS uk_post_schedule ON post_schedule(post_id, schedule_date);
CREATE INDEX IF NOT EXISTS idx_post_schedule_date ON post_schedule(schedule_date);
CREATE INDEX IF NOT EXISTS idx_post_schedule_stats ON post_schedule(post_id, person_id, schedule_date);

CREATE TABLE IF NOT EXISTS task_instance (
	id                   INTEGER PRIMARY KEY AUTOINCREMENT,
	source               TEXT    NOT NULL DEFAULT 'template' CHECK (source IN ('template', 'adhoc', 'review')),
	template_id          INTEGER NULL,
	post_id              INTEGER NULL,
	title                TEXT    NOT NULL,
	description          TEXT    NOT NULL DEFAULT '',
	period_key           TEXT    NULL,
	business_date        TEXT    NOT NULL,
	need_review          INTEGER NOT NULL DEFAULT 0 CHECK (need_review IN (0, 1)),
	review_of_task_id    INTEGER NULL,
	original_assignee_id INTEGER NULL,
	current_assignee_id  INTEGER NULL,
	status               TEXT    NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'completed', 'not_applicable', 'cancelled')),
	completed_by         INTEGER NULL,
	completed_at         TEXT    NULL,
	not_applicable_by    INTEGER NULL,
	not_applicable_at    TEXT    NULL,
	not_applicable_note  TEXT    NULL,
	cancelled_by         INTEGER NULL,
	cancelled_at         TEXT    NULL,
	created_at           TEXT    NOT NULL,
	FOREIGN KEY (template_id)          REFERENCES task_template(id) ON DELETE RESTRICT,
	FOREIGN KEY (post_id)              REFERENCES post(id)          ON DELETE RESTRICT,
	FOREIGN KEY (review_of_task_id)    REFERENCES task_instance(id) ON DELETE RESTRICT,
	FOREIGN KEY (original_assignee_id) REFERENCES person(id)        ON DELETE RESTRICT,
	FOREIGN KEY (current_assignee_id)  REFERENCES person(id)        ON DELETE RESTRICT,
	FOREIGN KEY (completed_by)         REFERENCES person(id)        ON DELETE RESTRICT,
	FOREIGN KEY (not_applicable_by)    REFERENCES person(id)        ON DELETE RESTRICT,
	FOREIGN KEY (cancelled_by)         REFERENCES person(id)        ON DELETE RESTRICT
);

CREATE TABLE IF NOT EXISTS handover_record (
	id               INTEGER PRIMARY KEY AUTOINCREMENT,
	task_instance_id INTEGER NOT NULL,
	from_person_id   INTEGER NULL,
	to_person_id     INTEGER NOT NULL,
	operated_by      INTEGER NOT NULL,
	note             TEXT    NOT NULL DEFAULT '',
	created_at       TEXT    NOT NULL,
	FOREIGN KEY (task_instance_id) REFERENCES task_instance(id) ON DELETE RESTRICT,
	FOREIGN KEY (from_person_id)   REFERENCES person(id)        ON DELETE RESTRICT,
	FOREIGN KEY (to_person_id)     REFERENCES person(id)        ON DELETE RESTRICT,
	FOREIGN KEY (operated_by)      REFERENCES person(id)        ON DELETE RESTRICT
);
CREATE INDEX IF NOT EXISTS idx_handover_instance ON handover_record(task_instance_id, created_at);

CREATE TABLE IF NOT EXISTS session (
	token       TEXT    PRIMARY KEY,
	person_id   INTEGER NOT NULL,
	csrf_token  TEXT    NOT NULL,
	expires_at  TEXT    NOT NULL,
	created_at  TEXT    NOT NULL,
	FOREIGN KEY (person_id) REFERENCES person(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_session_expires ON session(expires_at);
`
	if _, err := db.Exec(ddl); err != nil {
		return err
	}

	alterStmts := []string{
		"ALTER TABLE person ADD COLUMN password_hash TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE person ADD COLUMN in_rota INTEGER NOT NULL DEFAULT 1",
		"ALTER TABLE person ADD COLUMN rest_weekdays TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE person ADD COLUMN rest_month_days TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE person ADD COLUMN rest_month TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE post ADD COLUMN color TEXT NOT NULL DEFAULT '#007AFF'",
		"ALTER TABLE task_template ADD COLUMN weekdays TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE task_template ADD COLUMN month_days TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE task_template ADD COLUMN need_review INTEGER NOT NULL DEFAULT 0",
		"ALTER TABLE task_instance ADD COLUMN need_review INTEGER NOT NULL DEFAULT 0",
		"ALTER TABLE task_instance ADD COLUMN review_of_task_id INTEGER NULL",
	}
	for _, st := range alterStmts {
		_, _ = db.Exec(st)
	}

	var tblSQL string
	_ = db.QueryRow("SELECT sql FROM sqlite_master WHERE type='table' AND name='task_instance'").Scan(&tblSQL)
	if tblSQL != "" && !strings.Contains(tblSQL, "'review'") {
		if err := rebuildTaskInstanceForReview(db); err != nil {
			return err
		}
	}

	idxStmts := []string{
		"DROP INDEX IF EXISTS uk_task_instance_template_period",
		"CREATE UNIQUE INDEX IF NOT EXISTS uk_task_instance_tmpl_bizdate ON task_instance(template_id, business_date) WHERE template_id IS NOT NULL",
		"CREATE UNIQUE INDEX IF NOT EXISTS uk_task_instance_review_of ON task_instance(review_of_task_id) WHERE review_of_task_id IS NOT NULL",
		"CREATE INDEX IF NOT EXISTS idx_task_assignee_date_status ON task_instance(current_assignee_id, business_date, status)",
		"CREATE INDEX IF NOT EXISTS idx_task_business_date ON task_instance(business_date, post_id, status)",
	}
	for _, st := range idxStmts {
		if _, err := db.Exec(st); err != nil {
			return err
		}
	}
	return nil
}

func rebuildTaskInstanceForReview(db *sql.DB) error {
	_, _ = db.Exec("PRAGMA foreign_keys = OFF;")
	defer func() { _, _ = db.Exec("PRAGMA foreign_keys = ON;") }()

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(`
CREATE TABLE task_instance_v2 (
	id                   INTEGER PRIMARY KEY AUTOINCREMENT,
	source               TEXT    NOT NULL DEFAULT 'template' CHECK (source IN ('template', 'adhoc', 'review')),
	template_id          INTEGER NULL,
	post_id              INTEGER NULL,
	title                TEXT    NOT NULL,
	description          TEXT    NOT NULL DEFAULT '',
	period_key           TEXT    NULL,
	business_date        TEXT    NOT NULL,
	need_review          INTEGER NOT NULL DEFAULT 0 CHECK (need_review IN (0, 1)),
	review_of_task_id    INTEGER NULL,
	original_assignee_id INTEGER NULL,
	current_assignee_id  INTEGER NULL,
	status               TEXT    NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'completed', 'not_applicable', 'cancelled')),
	completed_by         INTEGER NULL,
	completed_at         TEXT    NULL,
	not_applicable_by    INTEGER NULL,
	not_applicable_at    TEXT    NULL,
	not_applicable_note  TEXT    NULL,
	cancelled_by         INTEGER NULL,
	cancelled_at         TEXT    NULL,
	created_at           TEXT    NOT NULL,
	FOREIGN KEY (template_id)          REFERENCES task_template(id) ON DELETE RESTRICT,
	FOREIGN KEY (post_id)              REFERENCES post(id)          ON DELETE RESTRICT,
	FOREIGN KEY (review_of_task_id)    REFERENCES task_instance(id) ON DELETE RESTRICT,
	FOREIGN KEY (original_assignee_id) REFERENCES person(id)        ON DELETE RESTRICT,
	FOREIGN KEY (current_assignee_id)  REFERENCES person(id)        ON DELETE RESTRICT,
	FOREIGN KEY (completed_by)         REFERENCES person(id)        ON DELETE RESTRICT,
	FOREIGN KEY (not_applicable_by)    REFERENCES person(id)        ON DELETE RESTRICT,
	FOREIGN KEY (cancelled_by)         REFERENCES person(id)        ON DELETE RESTRICT
);`); err != nil {
		return err
	}

	if _, err := tx.Exec(`
INSERT INTO task_instance_v2 (
	id, source, template_id, post_id, title, description, period_key, business_date,
	need_review, review_of_task_id, original_assignee_id, current_assignee_id, status,
	completed_by, completed_at, not_applicable_by, not_applicable_at, not_applicable_note,
	cancelled_by, cancelled_at, created_at
)
SELECT
	id, source, template_id, post_id, title, description, period_key, business_date,
	COALESCE(need_review, 0), review_of_task_id, original_assignee_id, current_assignee_id, status,
	completed_by, completed_at, not_applicable_by, not_applicable_at, not_applicable_note,
	cancelled_by, cancelled_at, created_at
FROM task_instance;`); err != nil {
		return err
	}

	if _, err := tx.Exec("DROP TABLE task_instance;"); err != nil {
		return err
	}
	if _, err := tx.Exec("ALTER TABLE task_instance_v2 RENAME TO task_instance;"); err != nil {
		return err
	}
	return tx.Commit()
}

// ==================== 密码哈希与主机物理重置管理员密码 ====================

func hashPassword(plain string) string {
	salt := make([]byte, 16)
	_, _ = rand.Read(salt)
	sum := deriveKey(plain, salt)
	return hex.EncodeToString(salt) + "$" + hex.EncodeToString(sum)
}

func verifyPassword(plain, encoded string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 2 {
		return false
	}
	salt, err1 := hex.DecodeString(parts[0])
	want, err2 := hex.DecodeString(parts[1])
	if err1 != nil || err2 != nil || len(salt) == 0 || len(want) == 0 {
		return false
	}
	got := deriveKey(plain, salt)
	return subtle.ConstantTimeCompare(got, want) == 1
}

func deriveKey(plain string, salt []byte) []byte {
	pBytes := []byte(plain)
	h := sha256.New()
	h.Write(salt)
	h.Write(pBytes)
	digest := h.Sum(nil)
	for i := 0; i < 10000; i++ {
		h.Reset()
		h.Write(digest)
		h.Write(salt)
		h.Write(pBytes)
		digest = h.Sum(digest[:0])
	}
	return digest
}

func (s *Store) ensureBootstrapAdmin(ctx context.Context) error {
	return s.withWriteTx(ctx, func(conn *sql.Conn) error {
		var count int
		if err := conn.QueryRowContext(ctx, "SELECT COUNT(1) FROM person").Scan(&count); err != nil {
			return err
		}
		if count == 0 {
			pwdHash := hashPassword(DefaultAdminInitialPassword)
			_, err := conn.ExecContext(ctx,
				"INSERT INTO person(nickname, password_hash, is_admin, in_rota, is_active, created_at) VALUES(?, ?, 1, 0, 1, ?)",
				"主机管理员", pwdHash, s.NowISO())
			if err != nil {
				return err
			}
			_, _ = conn.ExecContext(ctx, "INSERT OR IGNORE INTO app_kv(k, v) VALUES('admin_using_default_pwd', '1')")
			return nil
		}
		rows, err := conn.QueryContext(ctx, "SELECT id FROM person WHERE is_admin = 1 AND password_hash = ''")
		if err != nil {
			return err
		}
		var ids []int64
		for rows.Next() {
			var id int64
			_ = rows.Scan(&id)
			ids = append(ids, id)
		}
		rows.Close()
		for _, id := range ids {
			_, _ = conn.ExecContext(ctx, "UPDATE person SET password_hash = ? WHERE id = ?", hashPassword(DefaultAdminInitialPassword), id)
			_, _ = conn.ExecContext(ctx, "INSERT OR REPLACE INTO app_kv(k, v) VALUES('admin_using_default_pwd', '1')")
		}
		return nil
	})
}

func (s *Store) ResetAdminPasswords(ctx context.Context) error {
	return s.withWriteTx(ctx, func(conn *sql.Conn) error {
		rows, err := conn.QueryContext(ctx, "SELECT id FROM person WHERE is_admin = 1")
		if err != nil {
			return err
		}
		var ids []int64
		for rows.Next() {
			var id int64
			_ = rows.Scan(&id)
			ids = append(ids, id)
		}
		rows.Close()

		if len(ids) == 0 {
			_, err := conn.ExecContext(ctx,
				"INSERT INTO person(nickname, password_hash, is_admin, in_rota, is_active, created_at) VALUES(?, ?, 1, 0, 1, ?)",
				"主机管理员", hashPassword(DefaultAdminInitialPassword), s.NowISO())
			if err != nil {
				return err
			}
		} else {
			for _, id := range ids {
				if _, err := conn.ExecContext(ctx,
					"UPDATE person SET password_hash = ?, is_active = 1 WHERE id = ?",
					hashPassword(DefaultAdminInitialPassword), id); err != nil {
					return err
				}
			}
		}
		_, _ = conn.ExecContext(ctx, "INSERT OR REPLACE INTO app_kv(k, v) VALUES('admin_using_default_pwd', '1')")
		return nil
	})
}

func (s *Store) IsUsingDefaultAdminPwd(ctx context.Context) bool {
	var v string
	err := s.readDB.QueryRowContext(ctx, "SELECT v FROM app_kv WHERE k = 'admin_using_default_pwd'").Scan(&v)
	return err == nil && v == "1"
}

func (s *Store) GetSavedPort(ctx context.Context) string {
	var v string
	err := s.readDB.QueryRowContext(ctx, "SELECT v FROM app_kv WHERE k = 'listen_port'").Scan(&v)
	if err != nil || strings.TrimSpace(v) == "" {
		return "8080"
	}
	return strings.TrimSpace(v)
}

func (s *Store) SetSavedPort(ctx context.Context, port string) error {
	return s.withWriteTx(ctx, func(conn *sql.Conn) error {
		_, err := conn.ExecContext(ctx,
			"INSERT INTO app_kv(k, v) VALUES('listen_port', ?) ON CONFLICT(k) DO UPDATE SET v = excluded.v",
			strings.TrimSpace(port))
		return err
	})
}

func isBusyErr(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "sqlite_busy") || strings.Contains(msg, "database is locked")
}

func (s *Store) withWriteTx(ctx context.Context, fn func(conn *sql.Conn) error) error {
	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		if attempt > 0 {
			jitter, _ := rand.Int(rand.Reader, big.NewInt(40))
			backoff := time.Duration(25*(1<<attempt)+int(jitter.Int64())) * time.Millisecond
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
		}
		conn, err := s.writeDB.Conn(ctx)
		if err != nil {
			return err
		}
		if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
			_ = conn.Close()
			if isBusyErr(err) {
				lastErr = err
				continue
			}
			return err
		}
		if fnErr := fn(conn); fnErr != nil {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
			_ = conn.Close()
			if isBusyErr(fnErr) {
				lastErr = fnErr
				continue
			}
			return fnErr
		}
		_, commitErr := conn.ExecContext(ctx, "COMMIT")
		_ = conn.Close()
		if commitErr != nil {
			if isBusyErr(commitErr) {
				lastErr = commitErr
				continue
			}
			return commitErr
		}
		return nil
	}
	return fmt.Errorf("写事务繁忙重试超限: %w", lastErr)
}

func randomHex(bytesLen int) string {
	b := make([]byte, bytesLen)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// ==================== 认证与会话管理 ====================

func (s *Store) Login(ctx context.Context, nickname, password string) (*SessionInfo, error) {
	nickname = strings.TrimSpace(nickname)
	if nickname == "" {
		return nil, ErrBadInput
	}
	var p Person
	var pwdHash string
	var isAdmin, inRota, isActive int
	err := s.readDB.QueryRowContext(ctx, `
		SELECT id, nickname, password_hash, is_admin, in_rota, rest_weekdays, rest_month_days, rest_month, is_active, created_at
		FROM person WHERE nickname = ?`, nickname).Scan(
		&p.ID, &p.Nickname, &pwdHash, &isAdmin, &inRota, &p.RestWeekdays, &p.RestMonthDays, &p.RestMonth, &isActive, &p.CreatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if isActive == 0 {
		return nil, errors.New("该账号已停用，请联系管理员")
	}

	if isAdmin == 1 || pwdHash != "" {
		if password == "" || !verifyPassword(password, pwdHash) {
			return nil, ErrAuthFailed
		}
	}

	p.IsAdmin = isAdmin == 1
	p.InRota = inRota == 1
	p.HasPassword = pwdHash != ""
	p.IsActive = true

	token := randomHex(32)
	csrf := randomHex(16)
	now := s.Now()
	expires := now.AddDate(1, 0, 0).Format(time.RFC3339)

	err = s.withWriteTx(ctx, func(conn *sql.Conn) error {
		_, err := conn.ExecContext(ctx,
			"INSERT INTO session(token, person_id, csrf_token, expires_at, created_at) VALUES(?, ?, ?, ?, ?)",
			token, p.ID, csrf, expires, now.Format(time.RFC3339))
		return err
	})
	if err != nil {
		return nil, err
	}
	return &SessionInfo{Token: token, CSRFToken: csrf, Person: p}, nil
}

func (s *Store) CanPasswordlessQuickLogin(ctx context.Context, nickname string) bool {
	var pwdHash string
	var isAdmin, isActive int
	err := s.readDB.QueryRowContext(ctx,
		"SELECT password_hash, is_admin, is_active FROM person WHERE nickname = ?",
		strings.TrimSpace(nickname)).Scan(&pwdHash, &isAdmin, &isActive)
	if err != nil || isActive == 0 {
		return false
	}
	return isAdmin == 0 && pwdHash == ""
}

func (s *Store) ChangeOwnPassword(ctx context.Context, personID int64, oldPwd, newPwd string, clearPwd bool) error {
	return s.withWriteTx(ctx, func(conn *sql.Conn) error {
		var currHash string
		var isAdmin int
		err := conn.QueryRowContext(ctx,
			"SELECT password_hash, is_admin FROM person WHERE id = ?", personID).Scan(&currHash, &isAdmin)
		if err != nil {
			return err
		}
		if currHash != "" && !verifyPassword(oldPwd, currHash) {
			return errors.New("原密码验证不正确")
		}
		if clearPwd {
			if isAdmin == 1 {
				return errors.New("主机管理员账号必须保留登录密码，不可设为空")
			}
			_, err = conn.ExecContext(ctx, "UPDATE person SET password_hash = '' WHERE id = ?", personID)
			return err
		}
		if len(newPwd) < 4 {
			return errors.New("新密码长度请至少输入 4 位")
		}
		_, err = conn.ExecContext(ctx, "UPDATE person SET password_hash = ? WHERE id = ?", hashPassword(newPwd), personID)
		if err == nil && isAdmin == 1 {
			_, _ = conn.ExecContext(ctx, "DELETE FROM app_kv WHERE k = 'admin_using_default_pwd'")
		}
		return err
	})
}

func (s *Store) GetSession(ctx context.Context, token string) (*SessionInfo, error) {
	if token == "" {
		return nil, ErrNotFound
	}
	var info SessionInfo
	var pwdHash, expiresAt string
	var isAdmin, inRota, isActive int
	err := s.readDB.QueryRowContext(ctx, `
		SELECT s.token, s.csrf_token, s.expires_at,
		       p.id, p.nickname, p.password_hash, p.is_admin, p.in_rota,
		       p.rest_weekdays, p.rest_month_days, p.rest_month, p.is_active, p.created_at
		FROM session s
		JOIN person p ON p.id = s.person_id
		WHERE s.token = ?`, token).Scan(
		&info.Token, &info.CSRFToken, &expiresAt,
		&info.Person.ID, &info.Person.Nickname, &pwdHash, &isAdmin, &inRota,
		&info.Person.RestWeekdays, &info.Person.RestMonthDays, &info.Person.RestMonth,
		&isActive, &info.Person.CreatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if isActive == 0 {
		return nil, ErrForbidden
	}
	expTime, err := time.Parse(time.RFC3339, expiresAt)
	if err != nil || s.Now().After(expTime) {
		return nil, ErrNotFound
	}
	info.Person.IsAdmin = isAdmin == 1
	info.Person.InRota = inRota == 1
	info.Person.HasPassword = pwdHash != ""
	info.Person.IsActive = true
	return &info, nil
}

func (s *Store) DeleteSession(ctx context.Context, token string) error {
	return s.withWriteTx(ctx, func(conn *sql.Conn) error {
		_, err := conn.ExecContext(ctx, "DELETE FROM session WHERE token = ?", token)
		return err
	})
}

func (s *Store) RunDailyMaintenance(ctx context.Context, now time.Time) error {
	_ = s.withWriteTx(ctx, func(conn *sql.Conn) error {
		_, _ = conn.ExecContext(ctx, "DELETE FROM session WHERE expires_at < ?", now.Format(time.RFC3339))
		return nil
	})
	_, _ = s.writeDB.ExecContext(ctx, "PRAGMA wal_checkpoint(PASSIVE);")
	_, _ = s.writeDB.ExecContext(ctx, "PRAGMA optimize;")
	return nil
}

// ==================== 人员（含周休与当月/指定日调休配置） ====================

func normalizeRestMonthDays(raw string, loc *time.Location) (string, error) {
	raw = strings.ReplaceAll(raw, "，", ",")
	raw = strings.ReplaceAll(raw, "、", ",")
	raw = strings.ReplaceAll(raw, " ", ",")
	if strings.TrimSpace(raw) == "" {
		return "", nil
	}
	seen := make(map[string]bool)
	var items []string
	for _, tok := range strings.Split(raw, ",") {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		normDate := strings.ReplaceAll(strings.ReplaceAll(tok, "/", "-"), ".", "-")
		if strings.Count(normDate, "-") == 2 {
			t, err := time.ParseInLocation("2006-1-2", normDate, loc)
			if err != nil {
				t, err = time.ParseInLocation("2006-01-02", normDate, loc)
			}
			if err != nil {
				return "", fmt.Errorf("无效的调休日期格式 %q", tok)
			}
			dStr := t.Format("2006-01-02")
			if !seen[dStr] {
				seen[dStr] = true
				items = append(items, dStr)
			}
			continue
		}
		dayNum, err := strconv.Atoi(strings.TrimSuffix(tok, "日"))
		if err != nil {
			dayNum, err = strconv.Atoi(strings.TrimSuffix(tok, "号"))
		}
		if err != nil || dayNum < 1 || dayNum > 31 {
			return "", fmt.Errorf("无效的休息日号 %q（请输入 1~31 或 YYYY-MM-DD）", tok)
		}
		k := strconv.Itoa(dayNum)
		if !seen[k] {
			seen[k] = true
			items = append(items, k)
		}
	}
	return strings.Join(items, ","), nil
}

func isPersonRestingOn(p Person, t time.Time) bool {
	wd := int(t.Weekday())
	for _, d := range parseSavedInts(p.RestWeekdays) {
		if d == wd {
			return true
		}
	}
	if strings.TrimSpace(p.RestMonthDays) == "" {
		return false
	}
	fullDate := t.Format("2006-01-02")
	curMonth := t.Format("2006-01")
	dayNum := t.Day()
	for _, tok := range strings.Split(p.RestMonthDays, ",") {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		if strings.Contains(tok, "-") {
			if tok == fullDate {
				return true
			}
		} else if v, err := strconv.Atoi(tok); err == nil {
			if (p.RestMonth == "" || p.RestMonth == curMonth) && v == dayNum {
				return true
			}
		}
	}
	return false
}

func (s *Store) ListPeople(ctx context.Context, activeOnly bool) ([]Person, error) {
	q := `SELECT id, nickname, password_hash, is_admin, in_rota,
	             rest_weekdays, rest_month_days, rest_month, is_active, created_at
	      FROM person`
	if activeOnly {
		q += " WHERE is_active = 1"
	}
	q += " ORDER BY is_active DESC, is_admin DESC, id ASC"
	rows, err := s.readDB.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []Person
	for rows.Next() {
		var p Person
		var pwdHash string
		var adm, rota, act int
		if err := rows.Scan(&p.ID, &p.Nickname, &pwdHash, &adm, &rota,
			&p.RestWeekdays, &p.RestMonthDays, &p.RestMonth, &act, &p.CreatedAt); err != nil {
			return nil, err
		}
		p.IsAdmin = adm == 1
		p.InRota = rota == 1
		p.HasPassword = pwdHash != ""
		p.IsActive = act == 1
		list = append(list, p)
	}
	return list, rows.Err()
}

type PersonSaveInput struct {
	ID            int64  `json:"id"`
	Nickname      string `json:"nickname"`
	Password      string `json:"password"`
	ClearPassword bool   `json:"clear_password"`
	IsAdmin       bool   `json:"is_admin"`
	InRota        bool   `json:"in_rota"`
	RestWeekdays  string `json:"rest_weekdays"`
	RestMonthDays string `json:"rest_month_days"`
	RestMonth     string `json:"rest_month"`
	IsActive      bool   `json:"is_active"`
}

func (s *Store) SavePerson(ctx context.Context, in PersonSaveInput) error {
	in.Nickname = strings.TrimSpace(in.Nickname)
	if in.Nickname == "" {
		return ErrBadInput
	}

	normWeek := ""
	if strings.TrimSpace(in.RestWeekdays) != "" {
		_, nw, err := normalizeIntCSV(in.RestWeekdays, 0, 6)
		if err != nil {
			return errors.New("每周休息日格式不正确")
		}
		normWeek = nw
	}
	normMonthDays, err := normalizeRestMonthDays(in.RestMonthDays, s.loc)
	if err != nil {
		return err
	}
	restMonth := strings.TrimSpace(in.RestMonth)
	if normMonthDays != "" && restMonth == "" {
		restMonth = s.Now().Format("2006-01")
	}
	if normMonthDays == "" {
		restMonth = ""
	}

	adm, rota, act := 0, 0, 0
	if in.IsAdmin {
		adm = 1
	}
	if in.InRota {
		rota = 1
	}
	if in.IsActive {
		act = 1
	}

	return s.withWriteTx(ctx, func(conn *sql.Conn) error {
		if in.ID == 0 {
			if in.IsAdmin && len(in.Password) < 4 {
				return errors.New("新增管理员必须设置至少 4 位登录密码")
			}
			pwdHash := ""
			if in.Password != "" {
				if len(in.Password) < 4 {
					return errors.New("密码长度请至少设置 4 位")
				}
				pwdHash = hashPassword(in.Password)
			}
			_, err := conn.ExecContext(ctx, `
				INSERT INTO person(nickname, password_hash, is_admin, in_rota, rest_weekdays, rest_month_days, rest_month, is_active, created_at)
				VALUES(?, ?, ?, ?, ?, ?, ?, 1, ?)`,
				in.Nickname, pwdHash, adm, rota, normWeek, normMonthDays, restMonth, s.NowISO())
			return err
		}

		var currHash string
		if err := conn.QueryRowContext(ctx, "SELECT password_hash FROM person WHERE id = ?", in.ID).Scan(&currHash); err != nil {
			return err
		}

		if !in.IsAdmin || !in.IsActive {
			var otherAdmins int
			if err := conn.QueryRowContext(ctx,
				"SELECT COUNT(1) FROM person WHERE is_admin = 1 AND is_active = 1 AND id != ?", in.ID).Scan(&otherAdmins); err != nil {
				return err
			}
			if otherAdmins == 0 {
				return errors.New("系统至少需保留一名启用状态的管理员")
			}
		}

		newHash := currHash
		if in.ClearPassword {
			if in.IsAdmin {
				return errors.New("管理员账号必须保留密码，不可清空")
			}
			newHash = ""
		} else if in.Password != "" {
			if len(in.Password) < 4 {
				return errors.New("密码长度请至少设置 4 位")
			}
			newHash = hashPassword(in.Password)
			if in.IsAdmin {
				_, _ = conn.ExecContext(ctx, "DELETE FROM app_kv WHERE k = 'admin_using_default_pwd'")
			}
		} else if in.IsAdmin && currHash == "" {
			return errors.New("升级为管理员时必须同时设置登录密码")
		}

		_, err := conn.ExecContext(ctx, `
			UPDATE person
			SET nickname = ?, password_hash = ?, is_admin = ?, in_rota = ?,
			    rest_weekdays = ?, rest_month_days = ?, rest_month = ?, is_active = ?
			WHERE id = ?`,
			in.Nickname, newHash, adm, rota, normWeek, normMonthDays, restMonth, act, in.ID)
		return err
	})
}

// ==================== 岗位与周期任务模板 ====================

func sanitizeColor(c string) string {
	c = strings.TrimSpace(c)
	if len(c) == 7 && c[0] == '#' {
		for _, ch := range c[1:] {
			if !((ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'f') || (ch >= 'A' && ch <= 'F')) {
				return "#007AFF"
			}
		}
		return strings.ToUpper(c)
	}
	return "#007AFF"
}

func (s *Store) ListPosts(ctx context.Context, activeOnly bool) ([]Post, error) {
	q := "SELECT id, name, color, sort_order, is_active, created_at FROM post"
	if activeOnly {
		q += " WHERE is_active = 1"
	}
	q += " ORDER BY is_active DESC, sort_order ASC, id ASC"
	rows, err := s.readDB.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []Post
	for rows.Next() {
		var p Post
		var act int
		if err := rows.Scan(&p.ID, &p.Name, &p.Color, &p.SortOrder, &act, &p.CreatedAt); err != nil {
			return nil, err
		}
		p.Color = sanitizeColor(p.Color)
		p.IsActive = act == 1
		list = append(list, p)
	}
	return list, rows.Err()
}

func (s *Store) SavePost(ctx context.Context, id int64, name, color string, sortOrder int, isActive bool) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return ErrBadInput
	}
	color = sanitizeColor(color)
	act := 0
	if isActive {
		act = 1
	}
	return s.withWriteTx(ctx, func(conn *sql.Conn) error {
		if id == 0 {
			_, err := conn.ExecContext(ctx,
				"INSERT INTO post(name, color, sort_order, is_active, created_at) VALUES(?, ?, ?, 1, ?)",
				name, color, sortOrder, s.NowISO())
			return err
		}
		_, err := conn.ExecContext(ctx,
			"UPDATE post SET name = ?, color = ?, sort_order = ?, is_active = ? WHERE id = ?",
			name, color, sortOrder, act, id)
		return err
	})
}

func normalizeIntCSV(raw string, minVal, maxVal int) ([]int, string, error) {
	raw = strings.ReplaceAll(raw, "，", ",")
	raw = strings.ReplaceAll(raw, "、", ",")
	raw = strings.ReplaceAll(raw, " ", ",")
	seen := make(map[int]bool)
	var nums []int
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		v, err := strconv.Atoi(part)
		if err != nil || v < minVal || v > maxVal {
			return nil, "", fmt.Errorf("数值 %q 超出有效范围 (%d~%d)", part, minVal, maxVal)
		}
		if !seen[v] {
			seen[v] = true
			nums = append(nums, v)
		}
	}
	if len(nums) == 0 {
		return nil, "", fmt.Errorf("请至少选择或输入一个有效数字 (%d~%d)", minVal, maxVal)
	}
	sort.Ints(nums)
	strs := make([]string, len(nums))
	for i, n := range nums {
		strs[i] = strconv.Itoa(n)
	}
	return nums, strings.Join(strs, ","), nil
}

func parseSavedInts(csvStr string) []int {
	var out []int
	for _, p := range strings.Split(csvStr, ",") {
		p = strings.TrimSpace(p)
		if v, err := strconv.Atoi(p); err == nil {
			out = append(out, v)
		}
	}
	return out
}

func (s *Store) ListTemplates(ctx context.Context) ([]TaskTemplate, error) {
	rows, err := s.readDB.QueryContext(ctx, `
		SELECT t.id, t.post_id, p.name, p.color, p.sort_order,
		       t.title, t.description, t.cycle_type, t.weekdays, t.month_days, t.need_review, t.is_active, t.created_at
		FROM task_template t
		JOIN post p ON p.id = t.post_id
		ORDER BY
			CASE t.cycle_type WHEN 'daily' THEN 1 WHEN 'weekly' THEN 2 ELSE 3 END ASC,
			p.sort_order ASC, p.id ASC, t.is_active DESC, t.id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []TaskTemplate
	for rows.Next() {
		var t TaskTemplate
		var nr, act int
		if err := rows.Scan(&t.ID, &t.PostID, &t.PostName, &t.PostColor, &t.PostSort,
			&t.Title, &t.Description, &t.CycleType, &t.Weekdays, &t.MonthDays, &nr, &act, &t.CreatedAt); err != nil {
			return nil, err
		}
		t.PostColor = sanitizeColor(t.PostColor)
		t.NeedReview = nr == 1
		t.IsActive = act == 1
		list = append(list, t)
	}
	return list, rows.Err()
}

func (s *Store) CreateOrUpdateTemplate(ctx context.Context, tmpl TaskTemplate, now time.Time) (int64, error) {
	tmpl.Title = strings.TrimSpace(tmpl.Title)
	tmpl.Description = strings.TrimSpace(tmpl.Description)
	if tmpl.PostID <= 0 || tmpl.Title == "" {
		return 0, ErrBadInput
	}
	switch tmpl.CycleType {
	case "daily":
		tmpl.Weekdays = ""
		tmpl.MonthDays = ""
	case "weekly":
		_, norm, err := normalizeIntCSV(tmpl.Weekdays, 0, 6)
		if err != nil {
			return 0, errors.New("每周任务请至少勾选一天 (周日~周六)")
		}
		tmpl.Weekdays = norm
		tmpl.MonthDays = ""
	case "monthly":
		_, norm, err := normalizeIntCSV(tmpl.MonthDays, 1, 31)
		if err != nil {
			return 0, errors.New("每月任务请输入有效日期 (1~31，可逗号分隔多天)")
		}
		tmpl.MonthDays = norm
		tmpl.Weekdays = ""
	default:
		return 0, ErrBadInput
	}

	nrInt := 0
	if tmpl.NeedReview {
		nrInt = 1
	}

	now = now.In(s.loc)
	var outID int64
	err := s.withWriteTx(ctx, func(conn *sql.Conn) error {
		if tmpl.ID == 0 {
			res, err := conn.ExecContext(ctx, `
				INSERT INTO task_template(post_id, title, description, cycle_type, weekdays, month_days, need_review, is_active, created_at)
				VALUES(?, ?, ?, ?, ?, ?, ?, 1, ?)`,
				tmpl.PostID, tmpl.Title, tmpl.Description, tmpl.CycleType, tmpl.Weekdays, tmpl.MonthDays, nrInt, now.Format(time.RFC3339))
			if err != nil {
				return err
			}
			outID, _ = res.LastInsertId()
			tmpl.ID = outID
			tmpl.IsActive = true
		} else {
			act := 0
			if tmpl.IsActive {
				act = 1
			}
			outID = tmpl.ID
			_, err := conn.ExecContext(ctx, `
				UPDATE task_template
				SET post_id = ?, title = ?, description = ?, cycle_type = ?, weekdays = ?, month_days = ?, need_review = ?, is_active = ?
				WHERE id = ?`,
				tmpl.PostID, tmpl.Title, tmpl.Description, tmpl.CycleType, tmpl.Weekdays, tmpl.MonthDays, nrInt, act, tmpl.ID)
			if err != nil {
				return err
			}
		}

		if tmpl.IsActive {
			var postActive int
			if err := conn.QueryRowContext(ctx, "SELECT is_active FROM post WHERE id = ?", tmpl.PostID).Scan(&postActive); err != nil {
				return err
			}
			if postActive == 1 && shouldTriggerTemplateOnDate(tmpl, now) {
				_, err := insertTemplateInstanceTx(ctx, conn, tmpl, now)
				return err
			}
		}
		return nil
	})
	return outID, err
}

// ==================== 周期任务生成 + 次日接班复核任务自动生成 ====================

func daysInMonthOf(y int, m time.Month, loc *time.Location) int {
	return time.Date(y, m+1, 0, 0, 0, 0, 0, loc).Day()
}

func shouldTriggerTemplateOnDate(t TaskTemplate, date time.Time) bool {
	switch t.CycleType {
	case "daily":
		return true
	case "weekly":
		wd := int(date.Weekday())
		for _, d := range parseSavedInts(t.Weekdays) {
			if d == wd {
				return true
			}
		}
		return false
	case "monthly":
		lastDay := daysInMonthOf(date.Year(), date.Month(), date.Location())
		todayDay := date.Day()
		for _, d := range parseSavedInts(t.MonthDays) {
			effectiveDay := d
			if effectiveDay > lastDay {
				effectiveDay = lastDay
			}
			if todayDay == effectiveDay {
				return true
			}
		}
		return false
	default:
		return false
	}
}

func resolveScheduledPersonTx(ctx context.Context, conn *sql.Conn, postID int64, bizDate string) sql.NullInt64 {
	var assigneeID sql.NullInt64
	if postID > 0 {
		_ = conn.QueryRowContext(ctx, `
			SELECT ps.person_id
			FROM post_schedule ps
			JOIN person p ON p.id = ps.person_id AND p.is_active = 1
			WHERE ps.post_id = ? AND ps.schedule_date = ?
			LIMIT 1`, postID, bizDate).Scan(&assigneeID)
	}
	return assigneeID
}

func insertTemplateInstanceTx(ctx context.Context, conn *sql.Conn, t TaskTemplate, now time.Time) (int64, error) {
	bizDate := now.Format("2006-01-02")
	assigneeID := resolveScheduledPersonTx(ctx, conn, t.PostID, bizDate)

	var origPtr, currPtr any
	if assigneeID.Valid {
		origPtr = assigneeID.Int64
		currPtr = assigneeID.Int64
	}
	nrInt := 0
	if t.NeedReview {
		nrInt = 1
	}

	res, err := conn.ExecContext(ctx, `
		INSERT INTO task_instance(
			source, template_id, post_id, title, description,
			period_key, business_date, need_review, original_assignee_id, current_assignee_id,
			status, created_at
		) VALUES('template', ?, ?, ?, ?, ?, ?, ?, ?, ?, 'pending', ?)
		ON CONFLICT(template_id, business_date) WHERE template_id IS NOT NULL DO NOTHING`,
		t.ID, t.PostID, t.Title, t.Description,
		bizDate, bizDate, nrInt, origPtr, currPtr, now.Format(time.RFC3339))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func insertReviewTaskForParentTx(ctx context.Context, conn *sql.Conn, parentID int64, postID sql.NullInt64, parentTitle, parentDesc, reviewBizDate, nowISO string) (int64, error) {
	var postPtr, origPtr, currPtr any
	if postID.Valid && postID.Int64 > 0 {
		postPtr = postID.Int64
		sched := resolveScheduledPersonTx(ctx, conn, postID.Int64, reviewBizDate)
		if sched.Valid {
			origPtr = sched.Int64
			currPtr = sched.Int64
		}
	}

	reviewDesc := "请接班伙伴现场核查昨日该项任务是否真实、规范落实到位。"
	if parentDesc != "" {
		reviewDesc += "\n原任务标准：" + parentDesc
	}

	res, err := conn.ExecContext(ctx, `
		INSERT INTO task_instance(
			source, template_id, post_id, title, description,
			period_key, business_date, need_review, review_of_task_id,
			original_assignee_id, current_assignee_id, status, created_at
		) VALUES('review', NULL, ?, ?, ?, NULL, ?, 0, ?, ?, ?, 'pending', ?)
		ON CONFLICT(review_of_task_id) WHERE review_of_task_id IS NOT NULL DO NOTHING`,
		postPtr, parentTitle, reviewDesc, reviewBizDate, parentID, origPtr, currPtr, nowISO)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (s *Store) GeneratePeriodicTasks(ctx context.Context, now time.Time) (int, error) {
	now = now.In(s.loc)
	todayStr := now.Format("2006-01-02")
	yesterdayStr := now.AddDate(0, 0, -1).Format("2006-01-02")
	nowISO := now.Format(time.RFC3339)

	var generated int
	err := s.withWriteTx(ctx, func(conn *sql.Conn) error {
		rows, err := conn.QueryContext(ctx, `
			SELECT t.id, t.post_id, t.title, t.description, t.cycle_type, t.weekdays, t.month_days, t.need_review
			FROM task_template t
			JOIN post p ON p.id = t.post_id
			WHERE t.is_active = 1 AND p.is_active = 1`)
		if err != nil {
			return err
		}
		var tmpls []TaskTemplate
		for rows.Next() {
			var t TaskTemplate
			var nr int
			if err := rows.Scan(&t.ID, &t.PostID, &t.Title, &t.Description, &t.CycleType, &t.Weekdays, &t.MonthDays, &nr); err != nil {
				rows.Close()
				return err
			}
			t.NeedReview = nr == 1
			tmpls = append(tmpls, t)
		}
		rows.Close()

		for _, t := range tmpls {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if !shouldTriggerTemplateOnDate(t, now) {
				continue
			}
			n, err := insertTemplateInstanceTx(ctx, conn, t, now)
			if err != nil {
				return err
			}
			generated += int(n)
		}

		rRows, err := conn.QueryContext(ctx, `
			SELECT id, post_id, title, description
			FROM task_instance
			WHERE business_date = ?
			  AND need_review = 1
			  AND status IN ('completed', 'pending')`, yesterdayStr)
		if err != nil {
			return err
		}
		type reviewSrc struct {
			id     int64
			postID sql.NullInt64
			title  string
			desc   string
		}
		var srcs []reviewSrc
		for rRows.Next() {
			var rs reviewSrc
			if err := rRows.Scan(&rs.id, &rs.postID, &rs.title, &rs.desc); err != nil {
				rRows.Close()
				return err
			}
			srcs = append(srcs, rs)
		}
		rRows.Close()

		for _, src := range srcs {
			n, err := insertReviewTaskForParentTx(ctx, conn, src.id, src.postID, src.title, src.desc, todayStr, nowISO)
			if err != nil {
				return err
			}
			generated += int(n)
		}
		return nil
	})
	return generated, err
}

// ==================== 预排班表与公平轮询 ====================

func (s *Store) GetScheduleRange(ctx context.Context, startDate string, days int) ([]string, []ScheduleEntry, error) {
	if days < 1 {
		days = 1
	}
	if days > 62 {
		days = 62
	}
	start, err := time.ParseInLocation("2006-01-02", startDate, s.loc)
	if err != nil {
		start = s.Now()
	}
	dates := make([]string, days)
	for i := 0; i < days; i++ {
		dates[i] = start.AddDate(0, 0, i).Format("2006-01-02")
	}
	endDate := dates[len(dates)-1]

	rows, err := s.readDB.QueryContext(ctx, `
		SELECT ps.schedule_date, ps.post_id, po.name, ps.person_id, p.nickname
		FROM post_schedule ps
		JOIN post po ON po.id = ps.post_id
		JOIN person p ON p.id = ps.person_id
		WHERE ps.schedule_date >= ? AND ps.schedule_date <= ?
		ORDER BY ps.schedule_date ASC, po.sort_order ASC`, dates[0], endDate)
	if err != nil {
		return dates, nil, err
	}
	defer rows.Close()
	var list []ScheduleEntry
	for rows.Next() {
		var e ScheduleEntry
		if err := rows.Scan(&e.ScheduleDate, &e.PostID, &e.PostName, &e.PersonID, &e.PersonName); err != nil {
			return dates, nil, err
		}
		list = append(list, e)
	}
	return dates, list, rows.Err()
}

func (s *Store) GenerateFairRotaDraft(ctx context.Context, startDate string, days int) ([]ScheduleEntry, error) {
	if days < 1 {
		days = 1
	}
	if days > 31 {
		days = 31
	}
	start, err := time.ParseInLocation("2006-01-02", startDate, s.loc)
	if err != nil {
		start = s.Now()
	}
	startStr := start.Format("2006-01-02")

	posts, err := s.ListPosts(ctx, true)
	if err != nil || len(posts) == 0 {
		return nil, err
	}
	allPeople, err := s.ListPeople(ctx, true)
	if err != nil || len(allPeople) == 0 {
		return nil, err
	}

	var baseCandidates []Person
	for _, p := range allPeople {
		if p.InRota {
			baseCandidates = append(baseCandidates, p)
		}
	}
	if len(baseCandidates) == 0 {
		baseCandidates = allPeople
	}

	postCnt := make(map[int64]map[int64]int)
	lastDate := make(map[int64]map[int64]string)
	totalCnt := make(map[int64]int)
	for _, po := range posts {
		postCnt[po.ID] = make(map[int64]int)
		lastDate[po.ID] = make(map[int64]string)
	}

	rows, err := s.readDB.QueryContext(ctx, `
		SELECT post_id, person_id, COUNT(1), MAX(schedule_date)
		FROM post_schedule
		WHERE schedule_date < ?
		GROUP BY post_id, person_id`, startStr)
	if err == nil {
		for rows.Next() {
			var poID, peID int64
			var cnt int
			var maxD sql.NullString
			if rows.Scan(&poID, &peID, &cnt, &maxD) == nil {
				if postCnt[poID] == nil {
					postCnt[poID] = make(map[int64]int)
					lastDate[poID] = make(map[int64]string)
				}
				postCnt[poID][peID] = cnt
				lastDate[poID][peID] = maxD.String
				totalCnt[peID] += cnt
			}
		}
		rows.Close()
	}

	var draft []ScheduleEntry
	for d := 0; d < days; d++ {
		dayTime := start.AddDate(0, 0, d)
		dStr := dayTime.Format("2006-01-02")

		var availableToday []Person
		for _, p := range baseCandidates {
			if !isPersonRestingOn(p, dayTime) {
				availableToday = append(availableToday, p)
			}
		}
		if len(availableToday) == 0 {
			continue
		}

		dayLoad := make(map[int64]int)
		for _, po := range posts {
			sorted := make([]Person, len(availableToday))
			copy(sorted, availableToday)
			sort.Slice(sorted, func(i, j int) bool {
				pi, pj := sorted[i], sorted[j]
				if dayLoad[pi.ID] != dayLoad[pj.ID] {
					return dayLoad[pi.ID] < dayLoad[pj.ID]
				}
				ci, cj := postCnt[po.ID][pi.ID], postCnt[po.ID][pj.ID]
				if ci != cj {
					return ci < cj
				}
				if totalCnt[pi.ID] != totalCnt[pj.ID] {
					return totalCnt[pi.ID] < totalCnt[pj.ID]
				}
				li, lj := lastDate[po.ID][pi.ID], lastDate[po.ID][pj.ID]
				if li != lj {
					return li < lj
				}
				return pi.ID < pj.ID
			})

			chosen := sorted[0]
			dayLoad[chosen.ID]++
			postCnt[po.ID][chosen.ID]++
			totalCnt[chosen.ID]++
			lastDate[po.ID][chosen.ID] = dStr

			draft = append(draft, ScheduleEntry{
				ScheduleDate: dStr,
				PostID:       po.ID,
				PostName:     po.Name,
				PersonID:     chosen.ID,
				PersonName:   chosen.Nickname,
			})
		}
	}
	return draft, nil
}

func (s *Store) SaveScheduleEntries(ctx context.Context, entries []ScheduleEntry) (int, error) {
	if len(entries) == 0 {
		return 0, nil
	}
	nowISO := s.NowISO()
	var saved int
	err := s.withWriteTx(ctx, func(conn *sql.Conn) error {
		for _, e := range entries {
			if e.PostID <= 0 || e.ScheduleDate == "" {
				continue
			}
			if _, err := time.Parse("2006-01-02", e.ScheduleDate); err != nil {
				return fmt.Errorf("日期格式错误: %s", e.ScheduleDate)
			}
			if e.PersonID <= 0 {
				_, err := conn.ExecContext(ctx,
					"DELETE FROM post_schedule WHERE post_id = ? AND schedule_date = ?",
					e.PostID, e.ScheduleDate)
				if err != nil {
					return err
				}
				_, _ = conn.ExecContext(ctx, `
					UPDATE task_instance
					SET original_assignee_id = NULL, current_assignee_id = NULL
					WHERE post_id = ? AND business_date = ? AND status = 'pending'
					  AND (original_assignee_id IS NULL OR original_assignee_id = current_assignee_id)`,
					e.PostID, e.ScheduleDate)
				continue
			}

			var active int
			if err := conn.QueryRowContext(ctx, "SELECT is_active FROM person WHERE id = ?", e.PersonID).Scan(&active); err != nil || active != 1 {
				return fmt.Errorf("人员(ID=%d)不存在或已停用", e.PersonID)
			}

			_, err := conn.ExecContext(ctx, `
				INSERT INTO post_schedule(post_id, schedule_date, person_id, updated_at)
				VALUES(?, ?, ?, ?)
				ON CONFLICT(post_id, schedule_date) DO UPDATE SET
					person_id = excluded.person_id,
					updated_at = excluded.updated_at`,
				e.PostID, e.ScheduleDate, e.PersonID, nowISO)
			if err != nil {
				return err
			}

			_, err = conn.ExecContext(ctx, `
				UPDATE task_instance
				SET original_assignee_id = COALESCE(original_assignee_id, ?),
				    current_assignee_id  = ?
				WHERE post_id = ?
				  AND business_date = ?
				  AND status = 'pending'
				  AND (current_assignee_id IS NULL OR original_assignee_id = current_assignee_id)`,
				e.PersonID, e.PersonID, e.PostID, e.ScheduleDate)
			if err != nil {
				return err
			}
			saved++
		}
		s.logger.Info("写入岗位当值排班表", "entries", saved)
		return nil
	})
	return saved, err
}

func (s *Store) ImportScheduleText(ctx context.Context, rawText string) (int, error) {
	posts, err := s.ListPosts(ctx, true)
	if err != nil {
		return 0, err
	}
	people, err := s.ListPeople(ctx, true)
	if err != nil {
		return 0, err
	}
	postByName := make(map[string]int64)
	for _, p := range posts {
		postByName[strings.TrimSpace(p.Name)] = p.ID
	}
	personByName := make(map[string]int64)
	for _, p := range people {
		personByName[strings.TrimSpace(p.Nickname)] = p.ID
	}

	rawText = strings.TrimPrefix(rawText, "\uFEFF")
	rawLines := strings.Split(strings.ReplaceAll(rawText, "\r\n", "\n"), "\n")
	var rows [][]string
	for _, line := range rawLines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		sep := ","
		if strings.Contains(line, "\t") {
			sep = "\t"
		} else if strings.Contains(line, "，") && !strings.Contains(line, ",") {
			sep = "，"
		}
		cols := strings.Split(line, sep)
		for i := range cols {
			cols[i] = strings.Trim(strings.TrimSpace(cols[i]), `"`)
		}
		rows = append(rows, cols)
	}
	if len(rows) == 0 {
		return 0, errors.New("未识别到有效排班内容")
	}

	normalizeDate := func(d string) (string, bool) {
		d = strings.ReplaceAll(strings.ReplaceAll(strings.TrimSpace(d), "/", "-"), ".", "-")
		for _, layout := range []string{"2006-01-02", "2006-1-2"} {
			if t, err := time.ParseInLocation(layout, d, s.loc); err == nil {
				return t.Format("2006-01-02"), true
			}
		}
		return "", false
	}

	var entries []ScheduleEntry
	firstRow := rows[0]
	_, firstCellIsDate := normalizeDate(firstRow[0])

	isMatrix := false
	if !firstCellIsDate && len(firstRow) >= 2 {
		matchedPosts := 0
		for _, colName := range firstRow[1:] {
			if _, ok := postByName[colName]; ok {
				matchedPosts++
			}
		}
		if matchedPosts > 0 {
			isMatrix = true
		}
	}

	if isMatrix {
		colPostID := make(map[int]int64)
		for idx := 1; idx < len(firstRow); idx++ {
			if firstRow[idx] == "" {
				continue
			}
			pid, ok := postByName[firstRow[idx]]
			if !ok {
				return 0, fmt.Errorf("表头中的岗位「%s」不存在或未启用", firstRow[idx])
			}
			colPostID[idx] = pid
		}
		for rIdx := 1; rIdx < len(rows); rIdx++ {
			r := rows[rIdx]
			if len(r) == 0 || r[0] == "" {
				continue
			}
			dStr, ok := normalizeDate(r[0])
			if !ok {
				return 0, fmt.Errorf("第 %d 行日期「%s」格式不正确，请使用 YYYY-MM-DD", rIdx+1, r[0])
			}
			for cIdx := 1; cIdx < len(r); cIdx++ {
				postID, hasCol := colPostID[cIdx]
				if !hasCol || r[cIdx] == "" || r[cIdx] == "-" {
					continue
				}
				peID, ok := personByName[r[cIdx]]
				if !ok {
					return 0, fmt.Errorf("第 %d 行人员「%s」不存在或已停用", rIdx+1, r[cIdx])
				}
				entries = append(entries, ScheduleEntry{ScheduleDate: dStr, PostID: postID, PersonID: peID})
			}
		}
	} else {
		startIdx := 0
		if !firstCellIsDate {
			startIdx = 1
		}
		for rIdx := startIdx; rIdx < len(rows); rIdx++ {
			r := rows[rIdx]
			if len(r) < 3 {
				return 0, fmt.Errorf("第 %d 行列数不足（需包含：日期,岗位名称,当值人昵称）", rIdx+1)
			}
			dStr, ok := normalizeDate(r[0])
			if !ok {
				return 0, fmt.Errorf("第 %d 行日期「%s」格式不正确", rIdx+1, r[0])
			}
			poID, ok := postByName[r[1]]
			if !ok {
				return 0, fmt.Errorf("第 %d 行岗位「%s」不存在或未启用", rIdx+1, r[1])
			}
			peID, ok := personByName[r[2]]
			if !ok {
				return 0, fmt.Errorf("第 %d 行人员「%s」不存在或已停用", rIdx+1, r[2])
			}
			entries = append(entries, ScheduleEntry{ScheduleDate: dStr, PostID: poID, PersonID: peID})
		}
	}

	if len(entries) == 0 {
		return 0, errors.New("未解析出任何有效的排班记录")
	}
	return s.SaveScheduleEntries(ctx, entries)
}

// ==================== 临时任务 ====================

type AdhocInput struct {
	Title        string `json:"title"`
	Description  string `json:"description"`
	BusinessDate string `json:"business_date"`
	PostID       *int64 `json:"post_id"`
	AssigneeID   *int64 `json:"assignee_id"`
	NeedReview   bool   `json:"need_review"`
}

func (s *Store) CreateAdhocTask(ctx context.Context, in AdhocInput) (int64, error) {
	in.Title = strings.TrimSpace(in.Title)
	in.Description = strings.TrimSpace(in.Description)
	if in.Title == "" {
		return 0, errors.New("任务标题不能为空")
	}
	if in.BusinessDate == "" {
		in.BusinessDate = s.TodayStr()
	}
	if _, err := time.Parse("2006-01-02", in.BusinessDate); err != nil {
		return 0, errors.New("业务日期格式必须为 YYYY-MM-DD")
	}
	if (in.PostID == nil || *in.PostID <= 0) && (in.AssigneeID == nil || *in.AssigneeID <= 0) {
		return 0, errors.New("请至少指定当值伙伴或关联岗位")
	}

	nrInt := 0
	if in.NeedReview {
		nrInt = 1
	}

	var id int64
	err := s.withWriteTx(ctx, func(conn *sql.Conn) error {
		var finalAssignee any
		if in.AssigneeID != nil && *in.AssigneeID > 0 {
			var act int
			if err := conn.QueryRowContext(ctx, "SELECT is_active FROM person WHERE id = ?", *in.AssigneeID).Scan(&act); err != nil || act != 1 {
				return errors.New("指定的当值伙伴不存在或已停用")
			}
			finalAssignee = *in.AssigneeID
		} else if in.PostID != nil && *in.PostID > 0 {
			schedPerson := resolveScheduledPersonTx(ctx, conn, *in.PostID, in.BusinessDate)
			if schedPerson.Valid {
				finalAssignee = schedPerson.Int64
			}
		}

		var postPtr any
		if in.PostID != nil && *in.PostID > 0 {
			postPtr = *in.PostID
		}

		res, err := conn.ExecContext(ctx, `
			INSERT INTO task_instance(
				source, template_id, post_id, title, description,
				period_key, business_date, need_review, original_assignee_id, current_assignee_id,
				status, created_at
			) VALUES('adhoc', NULL, ?, ?, ?, NULL, ?, ?, ?, ?, 'pending', ?)`,
			postPtr, in.Title, in.Description, in.BusinessDate, nrInt, finalAssignee, finalAssignee, s.NowISO())
		if err != nil {
			return err
		}
		id, _ = res.LastInsertId()
		s.logger.Info("添加临时任务", "task_id", id, "date", in.BusinessDate, "need_review", in.NeedReview)
		return nil
	})
	return id, err
}

// ==================== 任务查询与状态机流转 ====================

const baseTaskSelectSQL = `
SELECT
	ti.id, ti.source, ti.template_id, ti.post_id,
	COALESCE(po.name, '个人直派'), COALESCE(po.color, '#8E8E93'),
	ti.title, ti.description, ti.period_key, ti.business_date,
	ti.need_review, ti.review_of_task_id,
	COALESCE(tp.business_date, ''), COALESCE(tp.status, ''),
	COALESCE(p_pcurr.nickname, '待安排'), COALESCE(p_pcomp.nickname, ''), COALESCE(tp.completed_at, ''),
	ti.original_assignee_id, COALESCE(p_orig.nickname, '待安排'),
	ti.current_assignee_id, COALESCE(p_curr.nickname, '待安排'),
	ti.status,
	ti.completed_by, COALESCE(p_comp.nickname, ''), ti.completed_at,
	ti.not_applicable_by, COALESCE(p_na.nickname, ''), ti.not_applicable_at, ti.not_applicable_note,
	ti.cancelled_by, ti.cancelled_at, ti.created_at
FROM task_instance ti
LEFT JOIN post po          ON po.id = ti.post_id
LEFT JOIN task_instance tp ON tp.id = ti.review_of_task_id
LEFT JOIN person p_pcurr   ON p_pcurr.id = tp.current_assignee_id
LEFT JOIN person p_pcomp   ON p_pcomp.id = tp.completed_by
LEFT JOIN person p_orig    ON p_orig.id = ti.original_assignee_id
LEFT JOIN person p_curr    ON p_curr.id = ti.current_assignee_id
LEFT JOIN person p_comp    ON p_comp.id = ti.completed_by
LEFT JOIN person p_na      ON p_na.id = ti.not_applicable_by
`

func scanTaskRow(scanner interface{ Scan(dest ...any) error }) (TaskInstance, error) {
	var t TaskInstance
	var needRev int
	var tmplID, postID, revOfID, origID, currID, compBy, naBy, cancelBy sql.NullInt64
	var pKey, compAt, naAt, naNote, cancelAt sql.NullString

	err := scanner.Scan(
		&t.ID, &t.Source, &tmplID, &postID, &t.PostName, &t.PostColor,
		&t.Title, &t.Description, &pKey, &t.BusinessDate,
		&needRev, &revOfID,
		&t.ParentDate, &t.ParentStatus, &t.ParentAssigneeName, &t.ParentCompletedBy, &t.ParentCompletedAt,
		&origID, &t.OriginalName,
		&currID, &t.CurrentName,
		&t.Status,
		&compBy, &t.CompletedByName, &compAt,
		&naBy, &t.NotApplicableName, &naAt, &naNote,
		&cancelBy, &t.CancelledAt, &t.CreatedAt,
	)
	if err != nil {
		return t, err
	}
	t.NeedReview = needRev == 1
	if t.Source == "review" {
		t.BadgeColor = FixedReviewColor
	} else {
		t.BadgeColor = t.PostColor
	}
	if tmplID.Valid {
		v := tmplID.Int64
		t.TemplateID = &v
	}
	if postID.Valid {
		v := postID.Int64
		t.PostID = &v
	}
	if revOfID.Valid {
		v := revOfID.Int64
		t.ReviewOfTaskID = &v
	}
	if pKey.Valid {
		v := pKey.String
		t.PeriodKey = &v
	}
	if origID.Valid {
		v := origID.Int64
		t.OriginalAssigneeID = &v
	}
	if currID.Valid {
		v := currID.Int64
		t.CurrentAssigneeID = &v
	}
	if compBy.Valid {
		v := compBy.Int64
		t.CompletedBy = &v
	}
	if compAt.Valid {
		v := compAt.String
		t.CompletedAt = &v
	}
	if naBy.Valid {
		v := naBy.Int64
		t.NotApplicableBy = &v
	}
	if naAt.Valid {
		v := naAt.String
		t.NotApplicableAt = &v
	}
	if naNote.Valid {
		v := naNote.String
		t.NotApplicableNote = &v
	}
	if cancelBy.Valid {
		v := cancelBy.Int64
		t.CancelledBy = &v
	}
	if cancelAt.Valid {
		v := cancelAt.String
		t.CancelledAt = &v
	}
	return t, nil
}

func (s *Store) ListTasks(ctx context.Context, businessDate, scope string, userID, postID int64) ([]TaskInstance, error) {
	if businessDate == "" {
		businessDate = s.TodayStr()
	}
	conds := []string{"ti.business_date = ?"}
	args := []any{businessDate}

	switch scope {
	case "mine":
		conds = append(conds, "ti.current_assignee_id = ? AND ti.status = 'pending'")
		args = append(args, userID)
	case "pending":
		conds = append(conds, "ti.status = 'pending'")
	}

	if postID > 0 {
		conds = append(conds, "ti.post_id = ?")
		args = append(args, postID)
	}

	query := baseTaskSelectSQL +
		" WHERE " + strings.Join(conds, " AND ") +
		` ORDER BY
			COALESCE(po.sort_order, 9999) ASC,
			COALESCE(ti.post_id, 999999) ASC,
			CASE ti.status WHEN 'pending' THEN 0 WHEN 'completed' THEN 1 WHEN 'not_applicable' THEN 2 ELSE 3 END,
			CASE WHEN ti.source = 'review' THEN 0 ELSE 1 END,
			ti.id ASC`

	rows, err := s.readDB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var list []TaskInstance
	for rows.Next() {
		item, err := scanTaskRow(rows)
		if err != nil {
			return nil, err
		}
		list = append(list, item)
	}
	return list, rows.Err()
}

func getTaskByIDConn(ctx context.Context, conn *sql.Conn, id int64) (TaskInstance, error) {
	row := conn.QueryRowContext(ctx, baseTaskSelectSQL+" WHERE ti.id = ?", id)
	t, err := scanTaskRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return t, ErrNotFound
	}
	return t, err
}

func (s *Store) CompleteTask(ctx context.Context, taskID, completedByID int64, operator Person) (TaskInstance, error) {
	if completedByID <= 0 {
		completedByID = operator.ID
	}
	now := s.Now()
	nowISO := now.Format(time.RFC3339)
	todayStr := now.Format("2006-01-02")

	var out TaskInstance
	err := s.withWriteTx(ctx, func(conn *sql.Conn) error {
		var active int
		if err := conn.QueryRowContext(ctx, "SELECT is_active FROM person WHERE id = ?", completedByID).Scan(&active); err != nil || active != 1 {
			return errors.New("指定的完成人不存在或已停用")
		}
		res, err := conn.ExecContext(ctx, `
			UPDATE task_instance
			SET status = 'completed',
			    completed_by = ?,
			    completed_at = ?,
			    original_assignee_id = COALESCE(original_assignee_id, ?),
			    current_assignee_id  = COALESCE(current_assignee_id, ?)
			WHERE id = ? AND status = 'pending'`,
			completedByID, nowISO, completedByID, completedByID, taskID)
		if err != nil {
			return err
		}
		affected, _ := res.RowsAffected()
		curr, err := getTaskByIDConn(ctx, conn, taskID)
		if err != nil {
			return err
		}
		if affected == 0 {
			if curr.Status == "completed" {
				out = curr
				return nil
			}
			return ErrInvalidStatus
		}

		// 精准补偿：仅当今天补打勾“昨天”的需复核任务（例如凌晨 00:00~00:04 之间补完）时，才为今天实时确保复核单存在；
		// 绝不对更早的历史日期塞入幽灵待办，也不提前生成明天的复核单。
		if curr.NeedReview {
			if bizT, pErr := time.ParseInLocation("2006-01-02", curr.BusinessDate, s.loc); pErr == nil {
				nextDayStr := bizT.AddDate(0, 0, 1).Format("2006-01-02")
				if nextDayStr == todayStr {
					var pID sql.NullInt64
					if curr.PostID != nil {
						pID = sql.NullInt64{Int64: *curr.PostID, Valid: true}
					}
					_, _ = insertReviewTaskForParentTx(ctx, conn, curr.ID, pID, curr.Title, curr.Description, nextDayStr, nowISO)
				}
			}
		}

		out = curr
		s.logger.Info("任务已完成", "task_id", taskID, "source", out.Source, "completed_by", out.CompletedByName)
		return nil
	})
	return out, err
}

func (s *Store) MarkNotApplicable(ctx context.Context, taskID int64, note string, operator Person) (TaskInstance, error) {
	note = strings.TrimSpace(note)
	var out TaskInstance
	err := s.withWriteTx(ctx, func(conn *sql.Conn) error {
		curr, err := getTaskByIDConn(ctx, conn, taskID)
		if err != nil {
			return err
		}
		if curr.Status == "not_applicable" {
			out = curr
			return nil
		}
		if curr.Status != "pending" {
			return ErrInvalidStatus
		}
		isAssignee := curr.CurrentAssigneeID != nil && *curr.CurrentAssigneeID == operator.ID
		if !operator.IsAdmin && !isAssignee {
			return ErrForbidden
		}
		res, err := conn.ExecContext(ctx, `
			UPDATE task_instance
			SET status = 'not_applicable',
			    not_applicable_by = ?,
			    not_applicable_at = ?,
			    not_applicable_note = ?
			WHERE id = ? AND status = 'pending'`,
			operator.ID, s.NowISO(), note, taskID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			curr, err = getTaskByIDConn(ctx, conn, taskID)
			if err == nil && curr.Status == "not_applicable" {
				out = curr
				return nil
			}
			return ErrInvalidStatus
		}
		out, err = getTaskByIDConn(ctx, conn, taskID)
		return err
	})
	return out, err
}

func (s *Store) RevertNotApplicable(ctx context.Context, taskID int64, operator Person, now time.Time) (TaskInstance, error) {
	today := now.In(s.loc).Format("2006-01-02")
	var out TaskInstance
	err := s.withWriteTx(ctx, func(conn *sql.Conn) error {
		curr, err := getTaskByIDConn(ctx, conn, taskID)
		if err != nil {
			return err
		}
		if curr.Status == "pending" {
			out = curr
			return nil
		}
		if curr.Status != "not_applicable" {
			return ErrInvalidStatus
		}
		if curr.BusinessDate != today {
			return errors.New("仅允许在业务当天内撤销“无此项”标记，跨天不可撤销")
		}
		isMarker := curr.NotApplicableBy != nil && *curr.NotApplicableBy == operator.ID
		if !operator.IsAdmin && !isMarker {
			return ErrForbidden
		}
		_, err = conn.ExecContext(ctx, `
			UPDATE task_instance
			SET status = 'pending', not_applicable_by = NULL, not_applicable_at = NULL, not_applicable_note = NULL
			WHERE id = ? AND status = 'not_applicable'`, taskID)
		if err != nil {
			return err
		}
		out, err = getTaskByIDConn(ctx, conn, taskID)
		return err
	})
	return out, err
}

func (s *Store) RevertCompleted(ctx context.Context, taskID int64, operator Person) (TaskInstance, error) {
	if !operator.IsAdmin {
		return TaskInstance{}, ErrForbidden
	}
	var out TaskInstance
	err := s.withWriteTx(ctx, func(conn *sql.Conn) error {
		curr, err := getTaskByIDConn(ctx, conn, taskID)
		if err != nil {
			return err
		}
		if curr.Status == "pending" {
			out = curr
			return nil
		}
		if curr.Status != "completed" {
			return ErrInvalidStatus
		}
		_, err = conn.ExecContext(ctx, `
			UPDATE task_instance
			SET status = 'pending', completed_by = NULL, completed_at = NULL
			WHERE id = ? AND status = 'completed'`, taskID)
		if err != nil {
			return err
		}
		out, err = getTaskByIDConn(ctx, conn, taskID)
		return err
	})
	return out, err
}

func (s *Store) CancelAdhocTask(ctx context.Context, taskID int64, operator Person) (TaskInstance, error) {
	if !operator.IsAdmin {
		return TaskInstance{}, ErrForbidden
	}
	var out TaskInstance
	err := s.withWriteTx(ctx, func(conn *sql.Conn) error {
		curr, err := getTaskByIDConn(ctx, conn, taskID)
		if err != nil {
			return err
		}
		if curr.Source != "adhoc" {
			return errors.New("仅临时任务支持取消操作")
		}
		if curr.Status == "cancelled" {
			out = curr
			return nil
		}
		if curr.Status != "pending" {
			return ErrInvalidStatus
		}
		_, err = conn.ExecContext(ctx, `
			UPDATE task_instance
			SET status = 'cancelled', cancelled_by = ?, cancelled_at = ?
			WHERE id = ? AND source = 'adhoc' AND status = 'pending'`,
			operator.ID, s.NowISO(), taskID)
		if err != nil {
			return err
		}
		out, err = getTaskByIDConn(ctx, conn, taskID)
		return err
	})
	return out, err
}

func (s *Store) HandoverTasks(ctx context.Context, taskIDs []int64, toPersonID int64, note string, operator Person) (int, error) {
	if len(taskIDs) == 0 || toPersonID <= 0 {
		return 0, ErrBadInput
	}
	note = strings.TrimSpace(note)
	nowISO := s.NowISO()
	var count int
	err := s.withWriteTx(ctx, func(conn *sql.Conn) error {
		var toActive int
		if err := conn.QueryRowContext(ctx, "SELECT is_active FROM person WHERE id = ?", toPersonID).Scan(&toActive); err != nil || toActive != 1 {
			return errors.New("交接接收伙伴不存在或已停用")
		}
		for _, taskID := range taskIDs {
			curr, err := getTaskByIDConn(ctx, conn, taskID)
			if err != nil {
				return err
			}
			if curr.Status != "pending" {
				return fmt.Errorf("任务「%s」状态已不是待完成，无法交接", curr.Title)
			}
			if curr.CurrentAssigneeID != nil && *curr.CurrentAssigneeID == toPersonID {
				continue
			}
			var fromPtr any
			if curr.CurrentAssigneeID != nil {
				fromPtr = *curr.CurrentAssigneeID
			}
			res, err := conn.ExecContext(ctx, `
				UPDATE task_instance
				SET original_assignee_id = COALESCE(original_assignee_id, ?),
				    current_assignee_id  = ?
				WHERE id = ? AND status = 'pending'`,
				toPersonID, toPersonID, taskID)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n == 0 {
				return fmt.Errorf("任务「%s」发生并发变更，交接已回滚", curr.Title)
			}
			_, err = conn.ExecContext(ctx, `
				INSERT INTO handover_record(task_instance_id, from_person_id, to_person_id, operated_by, note, created_at)
				VALUES(?, ?, ?, ?, ?, ?)`,
				taskID, fromPtr, toPersonID, operator.ID, note, nowISO)
			if err != nil {
				return err
			}
			count++
		}
		return nil
	})
	return count, err
}

func (s *Store) ListHandoverRecords(ctx context.Context, businessDate string) ([]HandoverRecord, error) {
	if businessDate == "" {
		businessDate = s.TodayStr()
	}
	rows, err := s.readDB.QueryContext(ctx, `
		SELECT h.id, h.task_instance_id, ti.title, COALESCE(po.name, '个人直派'), COALESCE(po.color, '#8E8E93'),
		       h.from_person_id, COALESCE(pf.nickname, '待安排'),
		       h.to_person_id, pt.nickname,
		       h.operated_by, pop.nickname, h.note, h.created_at
		FROM handover_record h
		JOIN task_instance ti ON ti.id = h.task_instance_id
		LEFT JOIN post po ON po.id = ti.post_id
		LEFT JOIN person pf ON pf.id = h.from_person_id
		JOIN person pt ON pt.id = h.to_person_id
		JOIN person pop ON pop.id = h.operated_by
		WHERE ti.business_date = ?
		ORDER BY h.id DESC`, businessDate)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []HandoverRecord
	for rows.Next() {
		var r HandoverRecord
		var fromID sql.NullInt64
		if err := rows.Scan(&r.ID, &r.TaskInstanceID, &r.TaskTitle, &r.PostName, &r.PostColor,
			&fromID, &r.FromName, &r.ToPersonID, &r.ToName,
			&r.OperatedBy, &r.OperatedByName, &r.Note, &r.CreatedAt); err != nil {
			return nil, err
		}
		if fromID.Valid {
			v := fromID.Int64
			r.FromPersonID = &v
		}
		list = append(list, r)
	}
	return list, rows.Err()
}

// ==================== 统计与历史手账 ====================

func (s *Store) GetDailyStats(ctx context.Context, businessDate string) (DailyStats, error) {
	if businessDate == "" {
		businessDate = s.TodayStr()
	}
	st := DailyStats{BusinessDate: businessDate}
	rows, err := s.readDB.QueryContext(ctx, `
		SELECT status, COUNT(1)
		FROM task_instance
		WHERE business_date = ?
		GROUP BY status`, businessDate)
	if err != nil {
		return st, err
	}
	defer rows.Close()
	for rows.Next() {
		var status string
		var cnt int
		if err := rows.Scan(&status, &cnt); err != nil {
			return st, err
		}
		switch status {
		case "completed":
			st.Completed = cnt
		case "pending":
			st.Pending = cnt
		case "not_applicable":
			st.NotApp = cnt
		case "cancelled":
			st.Cancelled = cnt
		}
	}
	denomStrict := st.Completed + st.Pending
	if denomStrict > 0 {
		st.RateStrict = float64(st.Completed) * 100.0 / float64(denomStrict)
	}
	denomInc := st.Completed + st.Pending + st.NotApp
	if denomInc > 0 {
		st.RateInclusive = float64(st.Completed) * 100.0 / float64(denomInc)
	}
	return st, nil
}

func (s *Store) GetHistoryBookPage(ctx context.Context, targetDate string, skipEmpty bool) (HistoryBookPage, error) {
	today := s.TodayStr()
	if targetDate == "" {
		targetDate = today
	}

	var page HistoryBookPage
	page.Date = targetDate

	cRows, err := s.readDB.QueryContext(ctx, `
		SELECT business_date,
		       SUM(CASE WHEN status='completed' THEN 1 ELSE 0 END),
		       SUM(CASE WHEN status='pending' THEN 1 ELSE 0 END),
		       SUM(CASE WHEN status='not_applicable' THEN 1 ELSE 0 END)
		FROM task_instance
		WHERE business_date <= ? AND status != 'cancelled'
		GROUP BY business_date
		ORDER BY business_date DESC
		LIMIT 60`, today)
	if err == nil {
		var rev []HistoryChapterDot
		for cRows.Next() {
			var d HistoryChapterDot
			if cRows.Scan(&d.Date, &d.Completed, &d.Pending, &d.NotApp) == nil {
				rev = append(rev, d)
			}
		}
		cRows.Close()
		for i := len(rev) - 1; i >= 0; i-- {
			page.ChapterBar = append(page.ChapterBar, rev[i])
		}
	}

	if skipEmpty {
		_ = s.readDB.QueryRowContext(ctx,
			"SELECT COALESCE(MAX(business_date), '') FROM task_instance WHERE business_date < ? AND status != 'cancelled'",
			targetDate).Scan(&page.PrevDate)
		_ = s.readDB.QueryRowContext(ctx,
			"SELECT COALESCE(MIN(business_date), '') FROM task_instance WHERE business_date > ? AND business_date <= ? AND status != 'cancelled'",
			targetDate, today).Scan(&page.NextDate)
	} else {
		if t, err := time.ParseInLocation("2006-01-02", targetDate, s.loc); err == nil {
			page.PrevDate = t.AddDate(0, 0, -1).Format("2006-01-02")
			next := t.AddDate(0, 0, 1).Format("2006-01-02")
			if next <= today {
				page.NextDate = next
			}
		}
	}

	page.Stats, _ = s.GetDailyStats(ctx, targetDate)
	page.Tasks, _ = s.ListTasks(ctx, targetDate, "all", 0, 0)
	page.Handovers, _ = s.ListHandoverRecords(ctx, targetDate)
	_, page.Schedules, _ = s.GetScheduleRange(ctx, targetDate, 1)
	return page, nil
}

// ==================== 在线安全备份 ====================

func (s *Store) PerformBackup(ctx context.Context, now time.Time) (string, error) {
	now = now.In(s.loc)
	fileName := fmt.Sprintf("taskhub_backup_%s.db", now.Format("20060102_150405"))
	destPath := filepath.Join(s.backupDir, fileName)
	escapedDest := strings.ReplaceAll(filepath.ToSlash(destPath), "'", "''")

	var vacErr error
	for attempt := 0; attempt < 4; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(100*attempt) * time.Millisecond)
			_ = os.Remove(destPath)
		}
		conn, cErr := s.writeDB.Conn(ctx)
		if cErr != nil {
			vacErr = cErr
			continue
		}
		_, vacErr = conn.ExecContext(ctx, fmt.Sprintf("VACUUM INTO '%s';", escapedDest))
		_ = conn.Close()
		if vacErr == nil || !isBusyErr(vacErr) {
			break
		}
	}
	if vacErr != nil {
		return "", fmt.Errorf("VACUUM INTO 失败: %w", vacErr)
	}
	_ = os.Chmod(destPath, 0600)

	chkDB, err := sql.Open("sqlite", fmt.Sprintf("file:%s?mode=ro", filepath.ToSlash(destPath)))
	if err != nil {
		return "", fmt.Errorf("打开备份验证失败: %w", err)
	}
	var integrity string
	err = chkDB.QueryRowContext(ctx, "PRAGMA integrity_check;").Scan(&integrity)
	_ = chkDB.Close()
	if err != nil || !strings.EqualFold(integrity, "ok") {
		_ = os.Remove(destPath)
		return "", fmt.Errorf("备份完整性校验未通过: %s (%v)", integrity, err)
	}

	cutoff := now.AddDate(0, 0, -30)
	if entries, err := os.ReadDir(s.backupDir); err == nil {
		for _, e := range entries {
			if e.IsDir() || !strings.HasPrefix(e.Name(), "taskhub_backup_") || !strings.HasSuffix(e.Name(), ".db") {
				continue
			}
			if info, iErr := e.Info(); iErr == nil && info.ModTime().Before(cutoff) {
				_ = os.Remove(filepath.Join(s.backupDir, e.Name()))
			}
		}
	}
	return fileName, nil
}

func (s *Store) ListBackups() ([]string, error) {
	entries, err := os.ReadDir(s.backupDir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasPrefix(e.Name(), "taskhub_backup_") && strings.HasSuffix(e.Name(), ".db") {
			names = append(names, e.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	return names, nil
}
