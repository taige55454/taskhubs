package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
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

// 测试 1：管理员密码重置 + 管理员一键帮忘记密码的普通伙伴清除密码
func TestPasswordAdminResetAndClearMemberPwd(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	sess, err := s.Login(ctx, "主机管理员", DefaultAdminInitialPassword)
	if err != nil {
		t.Fatalf("管理员初始密码登录失败: %v", err)
	}

	// 管理员不允许被清除为空密码
	if err := s.AdminClearMemberPassword(ctx, sess.Person.ID); err == nil {
		t.Fatal("管理员账号不应允许清除为空密码")
	}

	// 新增设了密码的普通伙伴“小陈”
	if err := s.SavePerson(ctx, PersonSaveInput{
		Nickname: "小陈",
		Password: "chen888",
		InRota:   true,
		IsActive: true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Login(ctx, "小陈", ""); err == nil {
		t.Fatal("已设密码的小陈空密码登录应被拒绝")
	}

	// 小陈忘记密码，管理员一键帮其清除密码 -> 立即恢复免密登录
	people, _ := s.ListPeople(ctx, true)
	var chenID int64
	for _, p := range people {
		if p.Nickname == "小陈" {
			chenID = p.ID
		}
	}
	if err := s.AdminClearMemberPassword(ctx, chenID); err != nil {
		t.Fatalf("管理员清除伙伴密码失败: %v", err)
	}
	if _, err := s.Login(ctx, "小陈", ""); err != nil {
		t.Fatalf("清除密码后小陈免密登录应当成功: %v", err)
	}

	// 验证管理员改密与物理重置
	_ = s.ChangeOwnPassword(ctx, sess.Person.ID, DefaultAdminInitialPassword, "Admin999", false)
	if err := s.ResetAdminPasswords(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Login(ctx, "主机管理员", DefaultAdminInitialPassword); err != nil {
		t.Fatal("物理重置后使用 admin123 登录应成功")
	}
}

// 测试 2：每周固定休息日 + 当月临时调休在自动化排班中的精准避让
func TestRotaSkipsRestingPeople(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	_ = s.SavePost(ctx, 0, "早班岗", "#007AFF", 1, true)
	_ = s.SavePost(ctx, 0, "晚班岗", "#34C759", 2, true)

	_ = s.SavePerson(ctx, PersonSaveInput{Nickname: "甲", InRota: true, RestWeekdays: "2", IsActive: true})
	_ = s.SavePerson(ctx, PersonSaveInput{Nickname: "乙", InRota: true, RestMonthDays: "2", RestMonth: "2026-12", IsActive: true})
	_ = s.SavePerson(ctx, PersonSaveInput{Nickname: "丙", InRota: true, IsActive: true})
	_ = s.SavePerson(ctx, PersonSaveInput{Nickname: "丁", InRota: true, IsActive: true})

	draft, err := s.GenerateFairRotaDraft(ctx, "2026-12-01", 2)
	if err != nil || len(draft) != 4 {
		t.Fatalf("生成排班草稿失败: len=%d, err=%v", len(draft), err)
	}

	for _, e := range draft {
		if e.ScheduleDate == "2026-12-01" && e.PersonName == "甲" {
			t.Fatal("周二固定休息的「甲」不应在 2026-12-01(周二) 被排班")
		}
		if e.ScheduleDate == "2026-12-02" && e.PersonName == "乙" {
			t.Fatal("12月2日调休的「乙」不应在 2026-12-02 被排班")
		}
	}

	people, _ := s.ListPeople(ctx, true)
	for _, p := range people {
		if p.Nickname == "乙" {
			jan2 := time.Date(2027, 1, 2, 0, 0, 0, 0, s.loc)
			if isPersonRestingOn(p, jan2) {
				t.Fatal("2026-12 的当月调休号不应跨月影响 2027-01")
			}
		}
	}
}

// 测试 3：特殊歇业停机数日后复工复核衔接 + 纯个人直派任务必选指定复核人
func TestClosureMultiDayReviewAndDirectAdhocReviewer(t *testing.T) {
	s, loc := newTestStore(t)
	ctx := context.Background()

	// 营业日 1：2026-11-23 正常开门运行
	dayOpen := time.Date(2026, 11, 23, 9, 0, 0, 0, loc)
	s.SetClock(func() time.Time { return dayOpen })

	_ = s.SavePost(ctx, 0, "机房安全岗", "#2563EB", 1, true)
	_ = s.SavePerson(ctx, PersonSaveInput{Nickname: "小王", InRota: true, IsActive: true})
	_ = s.SavePerson(ctx, PersonSaveInput{Nickname: "小赵", InRota: true, IsActive: true})
	_ = s.SavePerson(ctx, PersonSaveInput{Nickname: "店长小李", InRota: false, IsActive: true})

	posts, _ := s.ListPosts(ctx, true)
	people, _ := s.ListPeople(ctx, true)
	postID := posts[0].ID
	var wang, zhao, li Person
	for _, p := range people {
		switch p.Nickname {
		case "小王":
			wang = p
		case "小赵":
			zhao = p
		case "店长小李":
			li = p
		}
	}

	// 排班：11-23 小王当值，11-24/25 临时歇业关机未启动程序，11-26 恢复营业由小赵接班
	_, _ = s.SaveScheduleEntries(ctx, []ScheduleEntry{
		{ScheduleDate: "2026-11-23", PostID: postID, PersonID: wang.ID},
		{ScheduleDate: "2026-11-26", PostID: postID, PersonID: zhao.ID},
	})

	_, err := s.CreateOrUpdateTemplate(ctx, TaskTemplate{
		PostID:     postID,
		Title:      "闭店冷库总阀检查",
		CycleType:  "daily",
		NeedReview: true,
	}, dayOpen)
	if err != nil {
		t.Fatal(err)
	}

	// 验证纯个人直派任务（PostID == nil）：开启复核但未传 ReviewerID 必须拦截报错
	if _, err := s.CreateAdhocTask(ctx, AdhocInput{
		Title:        "盘点贵重备件柜",
		BusinessDate: "2026-11-23",
		AssigneeID:   &wang.ID,
		NeedReview:   true,
	}); err == nil {
		t.Fatal("纯个人直派任务开启复核但未指定 ReviewerID 应当被拒绝")
	}

	// 正确指定 ReviewerID = 店长小李
	adhocID, err := s.CreateAdhocTask(ctx, AdhocInput{
		Title:        "盘点贵重备件柜",
		BusinessDate: "2026-11-23",
		AssigneeID:   &wang.ID,
		NeedReview:   true,
		ReviewerID:   &li.ID,
	})
	if err != nil {
		t.Fatalf("创建带专属复核人的个人直派任务失败: %v", err)
	}

	// 小王在 11-23 完成岗位任务与个人直派任务
	d1Tasks, _ := s.ListTasks(ctx, "2026-11-23", "mine", wang.ID, 0)
	for _, tk := range d1Tasks {
		_, _ = s.CompleteTask(ctx, tk.ID, wang.ID, wang)
	}
	_ = adhocID

	// 模拟 11-24、11-25 歇业未打开程序，直接跳到 11-26 08:30 开机启动
	dayReopen := time.Date(2026, 11, 26, 8, 30, 0, 0, loc)
	s.SetClock(func() time.Time { return dayReopen })

	gen, err := s.GeneratePeriodicTasks(ctx, dayReopen)
	if err != nil || gen != 3 {
		t.Fatalf("复工日 11-26 应生成 3 条任务(1日常 + 1岗位跨歇业复核 + 1个人直派复核)，实际 gen=%d, err=%v", gen, err)
	}

	// 验证歇业日 11-24、11-25 保持 0 任务干净状态，绝不补灌死账
	closedTasks, _ := s.ListTasks(ctx, "2026-11-24", "all", 0, 0)
	if len(closedTasks) != 0 {
		t.Fatalf("歇业日 11-24 应保持 0 任务，实际 %d", len(closedTasks))
	}

	// 验证接班人小赵收到 11-23 的岗位复核 + 11-26 今日日常
	zhaoMine, _ := s.ListTasks(ctx, "2026-11-26", "mine", zhao.ID, 0)
	if len(zhaoMine) != 2 || zhaoMine[0].Source != "review" || zhaoMine[0].ParentDate != "2026-11-23" {
		t.Fatalf("小赵应收到溯源至 11-23 的置顶复核任务: %+v", zhaoMine)
	}

	// 验证店长小李精准收到个人直派任务的专属复核单
	liMine, _ := s.ListTasks(ctx, "2026-11-26", "mine", li.ID, 0)
	if len(liMine) != 1 || liMine[0].Source != "review" || liMine[0].Title != "盘点贵重备件柜" {
		t.Fatalf("指定复核人店长小李应收到个人直派复核单，实际: %+v", liMine)
	}
}

// 测试 4：多天周期与平年/闰年 2月及小月 29/30/31 号月末收敛
func TestEndOfMonthClamping(t *testing.T) {
	s, loc := newTestStore(t)
	ctx := context.Background()

	ref := time.Date(2026, 11, 10, 9, 0, 0, 0, loc)
	s.SetClock(func() time.Time { return ref })

	_ = s.SavePost(ctx, 0, "财务岗", "#5856D6", 1, true)
	posts, _ := s.ListPosts(ctx, true)

	_, err := s.CreateOrUpdateTemplate(ctx, TaskTemplate{
		PostID:    posts[0].ID,
		Title:     "月末库存总账核对",
		CycleType: "monthly",
		MonthDays: "15, 31",
	}, ref)
	if err != nil {
		t.Fatal(err)
	}

	if n, _ := s.GeneratePeriodicTasks(ctx, time.Date(2026, 2, 28, 0, 5, 0, 0, loc)); n != 1 {
		t.Fatal("平年2月28日应触发31号月末任务")
	}
	if n, _ := s.GeneratePeriodicTasks(ctx, time.Date(2028, 2, 28, 0, 5, 0, 0, loc)); n != 0 {
		t.Fatal("闰年2月28日不应提前触发31号月末任务")
	}
	if n, _ := s.GeneratePeriodicTasks(ctx, time.Date(2028, 2, 29, 0, 5, 0, 0, loc)); n != 1 {
		t.Fatal("闰年2月29日应精准触发31号月末任务")
	}
}

// 测试 5：日常垃圾回收维护与在线 VACUUM INTO 备份及日期持久化
func TestDailyMaintenanceAndBackup(t *testing.T) {
	s, loc := newTestStore(t)
	ctx := context.Background()

	now := time.Date(2026, 11, 25, 10, 0, 0, 0, loc)
	s.SetClock(func() time.Time { return now })

	if err := s.RunDailyMaintenance(ctx, now); err != nil {
		t.Fatalf("每日垃圾清理维护失败: %v", err)
	}
	fName, err := s.PerformBackup(ctx, now)
	if err != nil || fName == "" {
		t.Fatalf("在线安全备份失败: %v", err)
	}
	if got := s.GetLastBackupDate(ctx); got != "2026-11-25" {
		t.Fatalf("备份完成后应持久化记录日期 2026-11-25，实际 %q", got)
	}
}
