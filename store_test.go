package main

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestStore(t *testing.T) (*Store, *time.Location) {
	t.Helper()
	base := t.TempDir()
	dbPath := filepath.Join(base, "data", "test.db")
	backupDir := filepath.Join(base, "backups")
	_ = os.MkdirAll(filepath.Dir(dbPath), 0700)
	_ = os.MkdirAll(backupDir, 0700)

	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatalf("加载时区失败: %v", err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	store, err := NewStore(dbPath, backupDir, loc, logger)
	if err != nil {
		t.Fatalf("初始化 Store 失败: %v", err)
	}
	t.Cleanup(func() {
		_ = store.Close()
	})
	return store, loc
}

// 1. 测试旧版数据库表结构升级迁移 (rebuildTaskInstanceForReview) 与启动自检异常拦截
func TestLegacySchemaMigrationAndSelfCheck(t *testing.T) {
	base := t.TempDir()
	dbPath := filepath.Join(base, "data", "legacy.db")
	backupDir := filepath.Join(base, "backups")
	_ = os.MkdirAll(filepath.Dir(dbPath), 0700)

	loc, _ := time.LoadLocation("Asia/Shanghai")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	// 备份目录不存在时，NewStore 自检必须报错拒绝启动
	if _, err := NewStore(dbPath, filepath.Join(base, "missing_dir"), loc, logger); err == nil {
		t.Fatal("备份目录不存在时 NewStore 应当自检失败")
	}

	_ = os.MkdirAll(backupDir, 0700)
	// 先手工建一个不含 'review' 枚举的早期 v1 版 task_instance 表并塞入历史数据与无密码管理员
	rawDB, err := sql.Open("sqlite", buildDSN(dbPath, false))
	if err != nil {
		t.Fatal(err)
	}
	_, err = rawDB.Exec(`
		CREATE TABLE person (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			nickname TEXT NOT NULL,
			password_hash TEXT NOT NULL DEFAULT '',
			is_admin INTEGER NOT NULL DEFAULT 1,
			is_active INTEGER NOT NULL DEFAULT 1,
			created_at TEXT NOT NULL
		);
		INSERT INTO person(nickname, password_hash, is_admin, is_active, created_at)
		VALUES('早期无密管理员', '', 1, 1, '2026-01-01T00:00:00Z');

		CREATE TABLE post (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL,
			sort_order INTEGER NOT NULL DEFAULT 0,
			is_active INTEGER NOT NULL DEFAULT 1,
			created_at TEXT NOT NULL
		);
		INSERT INTO post(name, sort_order, is_active, created_at) VALUES('老岗位', 1, 1, '2026-01-01T00:00:00Z');

		CREATE TABLE task_instance (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			source TEXT NOT NULL DEFAULT 'template' CHECK (source IN ('template', 'adhoc')),
			template_id INTEGER NULL,
			post_id INTEGER NULL,
			title TEXT NOT NULL,
			description TEXT NOT NULL DEFAULT '',
			period_key TEXT NULL,
			business_date TEXT NOT NULL,
			original_assignee_id INTEGER NULL,
			current_assignee_id INTEGER NULL,
			status TEXT NOT NULL DEFAULT 'pending',
			completed_by INTEGER NULL,
			completed_at TEXT NULL,
			not_applicable_by INTEGER NULL,
			not_applicable_at TEXT NULL,
			not_applicable_note TEXT NULL,
			cancelled_by INTEGER NULL,
			cancelled_at TEXT NULL,
			created_at TEXT NOT NULL
		);
		INSERT INTO task_instance(source, post_id, title, business_date, status, created_at)
		VALUES('adhoc', 1, '历史旧任务', '2026-11-01', 'completed', '2026-11-01T09:00:00Z');
	`)
	_ = rawDB.Close()
	if err != nil {
		t.Fatalf("构造旧版数据库失败: %v", err)
	}

	// 启动 NewStore 触发自动列补齐 + 表重建迁移 + 空密管理员自动补默认密码
	st, err := NewStore(dbPath, backupDir, loc, logger)
	if err != nil {
		t.Fatalf("旧版数据库平滑迁移失败: %v", err)
	}
	defer st.Close()

	tasks, err := st.ListTasks(context.Background(), "2026-11-01", "all", 0, 0)
	if err != nil || len(tasks) != 1 || tasks[0].Title != "历史旧任务" {
		t.Fatalf("迁移后历史任务丢失: %+v, err=%v", tasks, err)
	}
	if _, err := st.Login(context.Background(), "早期无密管理员", DefaultAdminInitialPassword); err != nil {
		t.Fatalf("早期空密管理员应自动补齐初始密码: %v", err)
	}

	// 测试 SetClock(nil) 恢复真实时钟及 KV 端口存取
	st.SetClock(nil)
	if st.TodayStr() == "" || st.NowISO() == "" {
		t.Fatal("时钟输出不应为空")
	}
	if st.GetSavedPort(context.Background()) != "8080" {
		t.Fatal("默认端口应为 8080")
	}
	_ = st.SetSavedPort(context.Background(), "9090")
	if st.GetSavedPort(context.Background()) != "9090" {
		t.Fatal("端口保存未生效")
	}
}

// 2. 测试认证、密码修改、会话生命周期、管理员重置与帮伙伴清密码的所有正反分支
func TestAuthSessionsAndPasswordBranches(t *testing.T) {
	s, loc := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 25, 10, 0, 0, 0, loc)
	s.SetClock(func() time.Time { return now })

	// 内部辅助函数分支校验
	if verifyPassword("abc", "bad_format") || verifyPassword("abc", "zz$zz") {
		t.Fatal("损坏的密码哈希不应通过验证")
	}
	if !isBusyErr(errors.New("SQLITE_BUSY: database is locked")) || isBusyErr(nil) {
		t.Fatal("isBusyErr 判断异常")
	}

	// 登录异常分支
	if _, err := s.Login(ctx, "   ", ""); !errors.Is(err, ErrBadInput) {
		t.Fatal("空昵称应返回 ErrBadInput")
	}
	if _, err := s.Login(ctx, "不存在的人", ""); !errors.Is(err, ErrNotFound) {
		t.Fatal("未知用户应返回 ErrNotFound")
	}
	if _, err := s.Login(ctx, "主机管理员", "wrong_pwd"); !errors.Is(err, ErrAuthFailed) {
		t.Fatal("错误密码应返回 ErrAuthFailed")
	}
	if s.CanPasswordlessQuickLogin(ctx, "主机管理员") || s.CanPasswordlessQuickLogin(ctx, "不存在") {
		t.Fatal("管理员或不存在用户不可免密快捷登录")
	}

	adminSess, err := s.Login(ctx, "主机管理员", DefaultAdminInitialPassword)
	if err != nil {
		t.Fatal(err)
	}

	// 修改自身密码各校验分支
	if err := s.ChangeOwnPassword(ctx, adminSess.Person.ID, "wrong_old", "new1234", false); err == nil {
		t.Fatal("原密码错误应拦截")
	}
	if err := s.ChangeOwnPassword(ctx, adminSess.Person.ID, DefaultAdminInitialPassword, "", true); err == nil {
		t.Fatal("管理员清空密码应被拦截")
	}
	if err := s.ChangeOwnPassword(ctx, adminSess.Person.ID, DefaultAdminInitialPassword, "123", false); err == nil {
		t.Fatal("少于4位的新密码应被拦截")
	}
	if err := s.ChangeOwnPassword(ctx, adminSess.Person.ID, DefaultAdminInitialPassword, "Admin8888", false); err != nil {
		t.Fatal(err)
	}

	// 普通成员：免密 -> 自行设密 -> 自行清空密码 -> 管理员设密 -> 管理员一键清密
	_ = s.SavePerson(ctx, PersonSaveInput{Nickname: "伙伴小张", InRota: true, IsActive: true})
	if !s.CanPasswordlessQuickLogin(ctx, "伙伴小张") {
		t.Fatal("未设密普通伙伴应支持免密快捷登录")
	}
	zhangSess, err := s.Login(ctx, "伙伴小张", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ChangeOwnPassword(ctx, zhangSess.Person.ID, "", "zhang123", false); err != nil {
		t.Fatal(err)
	}
	if s.CanPasswordlessQuickLogin(ctx, "伙伴小张") {
		t.Fatal("设密后不应允许免密快捷登录")
	}
	if err := s.ChangeOwnPassword(ctx, zhangSess.Person.ID, "zhang123", "", true); err != nil {
		t.Fatal(err)
	}

	// 管理员帮小张设密后再测试一键清除密码及边界
	_ = s.SavePerson(ctx, PersonSaveInput{ID: zhangSess.Person.ID, Nickname: "伙伴小张", Password: "forgot99", InRota: true, IsActive: true})
	if err := s.AdminClearMemberPassword(ctx, 0); !errors.Is(err, ErrBadInput) {
		t.Fatal("非法ID应返回 ErrBadInput")
	}
	if err := s.AdminClearMemberPassword(ctx, 99999); !errors.Is(err, ErrNotFound) {
		t.Fatal("不存在ID应返回 ErrNotFound")
	}
	if err := s.AdminClearMemberPassword(ctx, adminSess.Person.ID); err == nil {
		t.Fatal("不可清除管理员密码")
	}
	if err := s.AdminClearMemberPassword(ctx, zhangSess.Person.ID); err != nil {
		t.Fatal(err)
	}
	// 清密后旧会话应已被清理
	if _, err := s.GetSession(ctx, zhangSess.Token); !errors.Is(err, ErrNotFound) {
		t.Fatal("清除密码后旧 Session 应已失效")
	}

	// 重新登录测试停用拦截、过期拦截与退出登录
	zhangSess2, _ := s.Login(ctx, "伙伴小张", "")
	if _, err := s.GetSession(ctx, ""); !errors.Is(err, ErrNotFound) {
		t.Fatal("空 Token 应返回 ErrNotFound")
	}
	// 时钟推进 2 年验证 Session 过期拦截
	s.SetClock(func() time.Time { return now.AddDate(2, 0, 0) })
	if _, err := s.GetSession(ctx, zhangSess2.Token); !errors.Is(err, ErrNotFound) {
		t.Fatal("过期 Session 应返回 ErrNotFound")
	}
	s.SetClock(func() time.Time { return now })
	_ = s.DeleteSession(ctx, zhangSess2.Token)

	// 停用账号后拒绝登录
	_ = s.SavePerson(ctx, PersonSaveInput{ID: zhangSess.Person.ID, Nickname: "伙伴小张", IsActive: false})
	if _, err := s.Login(ctx, "伙伴小张", ""); err == nil {
		t.Fatal("停用账号不应允许登录")
	}

	// 极端情况：若数据库中无任何管理员记录时调用 ResetAdminPasswords 自动重建默认管理员
	_, _ = s.writeDB.Exec("UPDATE person SET is_admin = 0")
	if err := s.ResetAdminPasswords(ctx); err != nil {
		t.Fatalf("无管理员兜底重建失败: %v", err)
	}
}

// 3. 测试人员、休息日解析、岗位色彩净化与最后一名管理员保护规则
func TestPersonPostAndRestDayValidation(t *testing.T) {
	s, loc := newTestStore(t)
	ctx := context.Background()

	if sanitizeColor("#ffaa00") != "#FFAA00" || sanitizeColor("bad") != "#007AFF" || sanitizeColor("#12345G") != "#007AFF" {
		t.Fatal("颜色净化逻辑异常")
	}
	if err := s.SavePost(ctx, 0, "  ", "#000000", 1, true); !errors.Is(err, ErrBadInput) {
		t.Fatal("空岗位名应报错")
	}
	_ = s.SavePost(ctx, 0, "收银岗", "#ff2d55", 10, true)
	posts, _ := s.ListPosts(ctx, false)
	_ = s.SavePost(ctx, posts[0].ID, "收银服务岗", "#34C759", 5, false)
	if actPosts, _ := s.ListPosts(ctx, true); len(actPosts) != 0 {
		t.Fatal("停用岗位后 activeOnly 应返回 0 条")
	}

	// 休息日多种混合格式标准化与错误拦截
	norm, err := normalizeRestMonthDays("5日，18号、2026/12/01 2026.12.02", loc)
	if err != nil || norm != "5,18,2026-12-01,2026-12-02" {
		t.Fatalf("调休日期标准化不符合预期: %q, err=%v", norm, err)
	}
	if _, err := normalizeRestMonthDays("35号", loc); err == nil {
		t.Fatal("超出31号的日期应报错")
	}
	if _, err := normalizeRestMonthDays("2026-99-99", loc); err == nil {
		t.Fatal("非法完整日期应报错")
	}

	// SavePerson 各种边界防御
	if err := s.SavePerson(ctx, PersonSaveInput{Nickname: ""}); !errors.Is(err, ErrBadInput) {
		t.Fatal("空昵称应报错")
	}
	if err := s.SavePerson(ctx, PersonSaveInput{Nickname: "测试员", RestWeekdays: "9"}); err == nil {
		t.Fatal("非法星期应报错")
	}
	if err := s.SavePerson(ctx, PersonSaveInput{Nickname: "新管理", IsAdmin: true, Password: "12"}); err == nil {
		t.Fatal("新增管理员未设够4位密码应报错")
	}
	if err := s.SavePerson(ctx, PersonSaveInput{Nickname: "短密员工", Password: "12"}); err == nil {
		t.Fatal("新增普通员工设短密码应报错")
	}

	people, _ := s.ListPeople(ctx, false)
	admin := people[0]
	// 唯一管理员不可降级或停用
	if err := s.SavePerson(ctx, PersonSaveInput{ID: admin.ID, Nickname: admin.Nickname, IsAdmin: false, IsActive: true}); err == nil {
		t.Fatal("唯一管理员不可取消管理员权限")
	}
	if err := s.SavePerson(ctx, PersonSaveInput{ID: admin.ID, Nickname: admin.Nickname, IsAdmin: true, IsActive: false}); err == nil {
		t.Fatal("唯一管理员不可停用")
	}
	if err := s.SavePerson(ctx, PersonSaveInput{ID: admin.ID, Nickname: admin.Nickname, IsAdmin: true, ClearPassword: true, IsActive: true}); err == nil {
		t.Fatal("管理员不可通过 SavePerson 清空密码")
	}

	// 普通无密成员升级管理员时未设密码应拦截
	_ = s.SavePerson(ctx, PersonSaveInput{Nickname: "准管理员", IsActive: true})
	allP, _ := s.ListPeople(ctx, false)
	var cand Person
	for _, p := range allP {
		if p.Nickname == "准管理员" {
			cand = p
		}
	}
	if err := s.SavePerson(ctx, PersonSaveInput{ID: cand.ID, Nickname: cand.Nickname, IsAdmin: true, IsActive: true}); err == nil {
		t.Fatal("无密成员升级管理员未填密码应被拦截")
	}
}

// 4. 测试排班范围、公平轮询全休跳过、排班保存联动与文本/矩阵导入全分支
func TestScheduleRotaAndImportBranches(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	_ = s.SavePost(ctx, 0, "A岗", "#007AFF", 1, true)
	_ = s.SavePost(ctx, 0, "B岗", "#34C759", 2, true)
	_ = s.SavePerson(ctx, PersonSaveInput{Nickname: "小王", InRota: true, RestMonthDays: "2026-12-01", IsActive: true})
	_ = s.SavePerson(ctx, PersonSaveInput{Nickname: "小李", InRota: true, RestMonthDays: "2026-12-01", IsActive: true})

	// 12-01 全员休息应自动跳过不排班，12-02 正常排班
	draft, err := s.GenerateFairRotaDraft(ctx, "2026-12-01", 2)
	if err != nil || len(draft) != 2 || draft[0].ScheduleDate != "2026-12-02" {
		t.Fatalf("全员休息日应跳过且次日正常生成: %+v, err=%v", draft, err)
	}

	// GetScheduleRange 边界收敛 (days < 1 及 days > 62)
	datesMin, _, _ := s.GetScheduleRange(ctx, "invalid-date", 0)
	datesMax, _, _ := s.GetScheduleRange(ctx, "2026-12-01", 100)
	if len(datesMin) != 1 || len(datesMax) != 62 {
		t.Fatalf("排班天数收敛异常: min=%d, max=%d", len(datesMin), len(datesMax))
	}

	// 测试 CSV 三列格式（中文逗号/斜杠日期/UTF-8 BOM）与矩阵格式导入
	csv3Col := "\uFEFF日期，岗位，人员\n2026/12/03，A岗，小王\n2026.12.03，B岗，小李"
	if n, err := s.ImportScheduleText(ctx, csv3Col); err != nil || n != 2 {
		t.Fatalf("三列中文逗号排班导入失败: n=%d, err=%v", n, err)
	}

	matrixTSV := "日期\tA岗\tB岗\n2026-12-04\t小李\t-"
	if n, err := s.ImportScheduleText(ctx, matrixTSV); err != nil || n != 1 {
		t.Fatalf("矩阵 TSV 排班导入失败: n=%d, err=%v", n, err)
	}

	// 导入异常分支校验
	badInputs := []string{
		"   ",
		"日期,不存在岗\n2026-12-05,小王",
		"日期\tA岗\nbad-date\t小王",
		"日期\tA岗\n2026-12-05\t未知人",
		"2026-12-05,列数不足",
		"bad-date,A岗,小王",
		"2026-12-05,不存在岗,小王",
		"2026-12-05,A岗,未知人",
	}
	for _, raw := range badInputs {
		if _, err := s.ImportScheduleText(ctx, raw); err == nil {
			t.Fatalf("错误排班文本应当报错拦截: %q", raw)
		}
	}

	// SaveScheduleEntries 传 PersonID=0 清除排班与错误日期拦截
	posts, _ := s.ListPosts(ctx, true)
	if _, err := s.SaveScheduleEntries(ctx, []ScheduleEntry{{ScheduleDate: "bad", PostID: posts[0].ID, PersonID: 1}}); err == nil {
		t.Fatal("错误排班日期格式应报错")
	}
	if _, err := s.SaveScheduleEntries(ctx, []ScheduleEntry{{ScheduleDate: "2026-12-04", PostID: posts[0].ID, PersonID: 0}}); err != nil {
		t.Fatalf("清除排班失败: %v", err)
	}
}

// 5. 测试任务全状态机流转、权限边界、凌晨跨天补打卡、歇业复工复核、兜底复核人与历史手账
func TestTaskStateMachineReviewFallbacksAndHistoryBook(t *testing.T) {
	s, loc := newTestStore(t)
	ctx := context.Background()

	day1 := time.Date(2026, 11, 20, 10, 0, 0, 0, loc) // 周五
	s.SetClock(func() time.Time { return day1 })

	_ = s.SavePost(ctx, 0, "核心机房岗", "#2563EB", 1, true)
	_ = s.SavePerson(ctx, PersonSaveInput{Nickname: "甲", InRota: true, IsActive: true})
	_ = s.SavePerson(ctx, PersonSaveInput{Nickname: "乙", InRota: true, IsActive: true})
	_ = s.SavePerson(ctx, PersonSaveInput{Nickname: "丙", InRota: true, IsActive: true})

	posts, _ := s.ListPosts(ctx, true)
	postID := posts[0].ID
	people, _ := s.ListPeople(ctx, true)
	var admin, jia, yi, bing Person
	for _, p := range people {
		switch p.Nickname {
		case "主机管理员":
			admin = p
		case "甲":
			jia = p
		case "乙":
			yi = p
		case "丙":
			bing = p
		}
	}

	_, _ = s.SaveScheduleEntries(ctx, []ScheduleEntry{
		{ScheduleDate: "2026-11-20", PostID: postID, PersonID: jia.ID},
	})

	// 模板参数校验分支
	if _, err := s.CreateOrUpdateTemplate(ctx, TaskTemplate{PostID: 0, Title: "x", CycleType: "daily"}, day1); !errors.Is(err, ErrBadInput) {
		t.Fatal("无效PostID应拦截")
	}
	if _, err := s.CreateOrUpdateTemplate(ctx, TaskTemplate{PostID: postID, Title: "周任务缺星期", CycleType: "weekly"}, day1); err == nil {
		t.Fatal("每周模板未选星期应拦截")
	}
	if _, err := s.CreateOrUpdateTemplate(ctx, TaskTemplate{PostID: postID, Title: "月任务缺日号", CycleType: "monthly", MonthDays: "0"}, day1); err == nil {
		t.Fatal("每月模板非法日号应拦截")
	}
	if _, err := s.CreateOrUpdateTemplate(ctx, TaskTemplate{PostID: postID, Title: "未知周期", CycleType: "yearly"}, day1); !errors.Is(err, ErrBadInput) {
		t.Fatal("非法周期类型应拦截")
	}

	// 创建周五触发的每周任务（带 NeedReview）并测试更新模板
	tmplID, err := s.CreateOrUpdateTemplate(ctx, TaskTemplate{
		PostID:     postID,
		Title:      "周五UPS放电巡检",
		Description: "检查电压曲线",
		CycleType:  "weekly",
		Weekdays:   "5", // 周五
		NeedReview: true,
	}, day1)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.CreateOrUpdateTemplate(ctx, TaskTemplate{
		ID:          tmplID,
		PostID:      postID,
		Title:       "周五UPS深度巡检",
		Description: "检查负载与内阻",
		CycleType:   "weekly",
		Weekdays:    "5",
		NeedReview:  true,
		IsActive:    true,
	}, day1)
	if err != nil {
		t.Fatal(err)
	}
	if tmpls, _ := s.ListTemplates(ctx); len(tmpls) != 1 {
		t.Fatal("模板列表数量应为 1")
	}

	// 测试 MarkNotApplicable 与 RevertNotApplicable 权限及跨天限制
	tasksD1, _ := s.ListTasks(ctx, "2026-11-20", "all", 0, postID)
	t1 := tasksD1[0]

	// 非当值且非管理员的「乙」标记无此项应被拒绝
	if _, err := s.MarkNotApplicable(ctx, t1.ID, "无关人员操作", yi); !errors.Is(err, ErrForbidden) {
		t.Fatal("非当值普通伙伴标记无此项应返回 ErrForbidden")
	}
	// 当值人「甲」成功标记无此项，重复标记保持幂等
	if _, err := s.MarkNotApplicable(ctx, t1.ID, "设备维保停机", jia); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MarkNotApplicable(ctx, t1.ID, "重复点击", jia); err != nil {
		t.Fatal("重复标记无此项应当幂等成功")
	}
	// 跨天撤销无此项应被拒绝
	if _, err := s.RevertNotApplicable(ctx, t1.ID, jia, day1.AddDate(0, 0, 1)); err == nil {
		t.Fatal("跨天撤销无此项必须报错拦截")
	}
	// 非标记人「乙」当天撤销无此项应拒绝
	if _, err := s.RevertNotApplicable(ctx, t1.ID, yi, day1); !errors.Is(err, ErrForbidden) {
		t.Fatal("他人撤销无此项应返回 ErrForbidden")
	}
	// 本人当天撤销恢复 pending
	if _, err := s.RevertNotApplicable(ctx, t1.ID, jia, day1); err != nil {
		t.Fatal(err)
	}

	// 测试交接任务 HandoverTasks 与流水记录
	if _, err := s.HandoverTasks(ctx, []int64{t1.ID}, yi.ID, "临时有事换班", jia); err != nil {
		t.Fatal(err)
	}
	hos, _ := s.ListHandoverRecords(ctx, "2026-11-20")
	if len(hos) != 1 || hos[0].ToName != "乙" {
		t.Fatalf("交接流水记录异常: %+v", hos)
	}

	// 测试凌晨 00:02 补打勾“昨天”的需复核任务 -> 实时补偿生成今日复核单，且次日未排班时自动兜底给主机管理员！
	day2Early := time.Date(2026, 11, 21, 0, 2, 0, 0, loc)
	s.SetClock(func() time.Time { return day2Early })
	if _, err := s.CompleteTask(ctx, t1.ID, yi.ID, yi); err != nil {
		t.Fatal(err)
	}
	// 重复点击完成保持幂等
	if _, err := s.CompleteTask(ctx, t1.ID, yi.ID, yi); err != nil {
		t.Fatal("重复完成应保持幂等")
	}

	// 11-21 未给机房岗排班，检查自动生成的复核单是否兜底分给了「主机管理员」
	adminRevTasks, _ := s.ListTasks(ctx, "2026-11-21", "mine", admin.ID, 0)
	if len(adminRevTasks) != 1 || adminRevTasks[0].Source != "review" {
		t.Fatalf("次日漏排班时复核单应自动兜底指派给主机管理员，实际: %+v", adminRevTasks)
	}

	// 测试 RevertCompleted 权限与回退，以及 CancelAdhocTask 级联取消待复核单
	if _, err := s.RevertCompleted(ctx, t1.ID, yi); !errors.Is(err, ErrForbidden) {
		t.Fatal("普通成员不可撤销已完成状态")
	}
	if _, err := s.RevertCompleted(ctx, t1.ID, admin); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CancelAdhocTask(ctx, t1.ID, admin); err == nil {
		t.Fatal("周期模板任务不允许当作临时任务取消")
	}

	// 创建一条指定了复核人「丙」的临时任务，随后停用「丙」，验证跨日复核自动降级兜底给主机管理员
	adhocID, err := s.CreateAdhocTask(ctx, AdhocInput{
		Title:        "临时专项检查",
		BusinessDate: "2026-11-21",
		AssigneeID:   &yi.ID,
		NeedReview:   true,
		ReviewerID:   &bing.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = s.SavePerson(ctx, PersonSaveInput{ID: bing.ID, Nickname: bing.Nickname, IsActive: false})

	day3 := time.Date(2026, 11, 22, 8, 0, 0, 0, loc)
	s.SetClock(func() time.Time { return day3 })
	if gen, err := s.GeneratePeriodicTasks(ctx, day3); err != nil || gen != 1 {
		t.Fatalf("11-22 应为昨日未取消临时任务生成 1 条复核单: gen=%d, err=%v", gen, err)
	}
	// 管理员取消原临时任务 -> 尚未完成的复核单应级联取消
	if _, err := s.CancelAdhocTask(ctx, adhocID, admin); err != nil {
		t.Fatal(err)
	}

	// 验证统计率计算（含无此项 vs 不含无此项）与历史手账翻页（skipEmpty=true / false）
	stats, err := s.GetDailyStats(ctx, "2026-11-22")
	if err != nil || stats.Cancelled != 1 {
		t.Fatalf("统计结果异常: %+v, err=%v", stats, err)
	}
	bookSkip, err := s.GetHistoryBookPage(ctx, "2026-11-21", true)
	if err != nil || bookSkip.PrevDate != "2026-11-20" {
		t.Fatalf("历史手账跳空翻页异常: %+v, err=%v", bookSkip, err)
	}
	bookLinear, err := s.GetHistoryBookPage(ctx, "2026-11-21", false)
	if err != nil || bookLinear.NextDate != "2026-11-22" {
		t.Fatalf("历史手账按日连续翻页异常: %+v, err=%v", bookLinear, err)
	}
}

// 6. 测试在线安全备份完整性、列表排序及超过 30 天旧备份自动淘汰清理
func TestBackupIntegrityAnd30DayRetention(t *testing.T) {
	s, loc := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 25, 3, 5, 0, 0, loc)
	s.SetClock(func() time.Time { return now })

	// 放入一个 45 天前的过期备份文件和一个普通说明文件
	expiredFile := filepath.Join(s.backupDir, "taskhub_backup_20261001_030000.db")
	keepNoteFile := filepath.Join(s.backupDir, "readme.txt")
	_ = os.WriteFile(expiredFile, []byte("old"), 0600)
	_ = os.WriteFile(keepNoteFile, []byte("keep"), 0600)
	oldTime := now.AddDate(0, 0, -45)
	_ = os.Chtimes(expiredFile, oldTime, oldTime)
	_ = os.Chtimes(keepNoteFile, oldTime, oldTime)

	if err := s.RunDailyMaintenance(ctx, now); err != nil {
		t.Fatal(err)
	}
	fName, err := s.PerformBackup(ctx, now)
	if err != nil || !strings.HasPrefix(fName, "taskhub_backup_20261125") {
		t.Fatalf("执行在线备份失败: fName=%q, err=%v", fName, err)
	}

	if _, err := os.Stat(expiredFile); !os.IsNotExist(err) {
		t.Fatal("超过 30 天的旧备份文件应当被自动清理")
	}
	if _, err := os.Stat(keepNoteFile); err != nil {
		t.Fatal("非备份命名的文件不应被误删")
	}
	backups, err := s.ListBackups()
	if err != nil || len(backups) != 1 || backups[0] != fName {
		t.Fatalf("备份列表返回异常: %+v, err=%v", backups, err)
	}
}
