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

// 测试 1：密码校验、修改密码与忘记管理员密码物理重置
func TestPasswordAndAdminReset(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	if _, err := s.Login(ctx, "主机管理员", ""); err == nil {
		t.Fatal("管理员空密码登录应当被拒绝")
	}
	sess, err := s.Login(ctx, "主机管理员", DefaultAdminInitialPassword)
	if err != nil {
		t.Fatalf("管理员初始密码登录失败: %v", err)
	}

	if err := s.ChangeOwnPassword(ctx, sess.Person.ID, DefaultAdminInitialPassword, "MySecret999", false); err != nil {
		t.Fatal(err)
	}
	if s.IsUsingDefaultAdminPwd(ctx) {
		t.Fatal("修改密码后不应仍标记为默认密码")
	}
	if err := s.ResetAdminPasswords(ctx); err != nil {
		t.Fatalf("重置管理员密码失败: %v", err)
	}
	if !s.IsUsingDefaultAdminPwd(ctx) {
		t.Fatal("重置后应重新恢复默认密码提示标记")
	}
	if _, err := s.Login(ctx, "主机管理员", DefaultAdminInitialPassword); err != nil {
		t.Fatalf("重置后使用 admin123 登录失败: %v", err)
	}
}

// 测试 2：每周固定休息日 + 当月临时调休在自动化排班中的精准避让
func TestRotaSkipsRestingPeople(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	_ = s.SavePost(ctx, 0, "早班岗", "#007AFF", 1, true)
	_ = s.SavePost(ctx, 0, "晚班岗", "#34C759", 2, true)

	// 2026-12-01 是周二(weekday=2)，2026-12-02 是周三(weekday=3)
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

// 测试 3：重要任务「次日接班复核」自动流转给次日接班人 + 固定颜色 #D97706 + 幂等防重 + 防历史污染
func TestNextDayHandoverReviewWorkflow(t *testing.T) {
	s, loc := newTestStore(t)
	ctx := context.Background()

	// 锁定测试初始模拟时钟为 2026-11-23 09:00
	day1 := time.Date(2026, 11, 23, 9, 0, 0, 0, loc)
	s.SetClock(func() time.Time { return day1 })

	_ = s.SavePost(ctx, 0, "机房安全岗", "#2563EB", 1, true)
	_ = s.SavePerson(ctx, PersonSaveInput{Nickname: "周一当值小王", InRota: true, IsActive: true})
	_ = s.SavePerson(ctx, PersonSaveInput{Nickname: "周二接班小赵", InRota: true, IsActive: true})

	posts, _ := s.ListPosts(ctx, true)
	people, _ := s.ListPeople(ctx, true)
	postID := posts[0].ID
	var wang, zhao Person
	for _, p := range people {
		if p.Nickname == "周一当值小王" {
			wang = p
		}
		if p.Nickname == "周二接班小赵" {
			zhao = p
		}
	}

	_, _ = s.SaveScheduleEntries(ctx, []ScheduleEntry{
		{ScheduleDate: "2026-11-23", PostID: postID, PersonID: wang.ID},
		{ScheduleDate: "2026-11-24", PostID: postID, PersonID: zhao.ID},
	})

	_, err := s.CreateOrUpdateTemplate(ctx, TaskTemplate{
		PostID:     postID,
		Title:      "核心冷库温度阀门锁定",
		CycleType:  "daily",
		NeedReview: true,
	}, day1)
	if err != nil {
		t.Fatal(err)
	}
	day1Tasks, _ := s.ListTasks(ctx, "2026-11-23", "mine", wang.ID, 0)
	if len(day1Tasks) != 1 || !day1Tasks[0].NeedReview {
		t.Fatal("11-23 应为小王生成 1 条带 NeedReview 标记的任务")
	}
	_, _ = s.CompleteTask(ctx, day1Tasks[0].ID, wang.ID, wang)

	// 推进模拟时钟至次日 2026-11-24 00:05 触发每日调度
	day2 := time.Date(2026, 11, 24, 0, 5, 0, 0, loc)
	s.SetClock(func() time.Time { return day2 })

	gen, err := s.GeneratePeriodicTasks(ctx, day2)
	if err != nil || gen != 2 {
		t.Fatalf("11-24 应生成 2 条任务(1日常+1次日复核)，实际 gen=%d, err=%v", gen, err)
	}

	// 重复执行调度必须幂等为 0
	if gen2, _ := s.GeneratePeriodicTasks(ctx, day2); gen2 != 0 {
		t.Fatalf("重复生成次日复核任务必须幂等返回 0，实际 %d", gen2)
	}

	zhaoMine, _ := s.ListTasks(ctx, "2026-11-24", "mine", zhao.ID, 0)
	if len(zhaoMine) != 2 {
		t.Fatalf("接班人小赵应看到 2 条待办，实际 %d", len(zhaoMine))
	}

	revTask := zhaoMine[0]
	if revTask.Source != "review" {
		t.Fatalf("复核任务应在同岗位待办中优先置顶，实际首条 source=%s", revTask.Source)
	}
	if revTask.BadgeColor != FixedReviewColor {
		t.Fatalf("复核任务标签颜色应固定为 %s，实际 %s", FixedReviewColor, revTask.BadgeColor)
	}
	if revTask.ParentCompletedBy != "周一当值小王" || revTask.ParentDate != "2026-11-23" {
		t.Fatalf("复核卡片未正确回溯昨日完成人信息: %+v", revTask)
	}

	// 补充边界验证：在 11-24 补完成一条几天前 (11-20) 的遗留任务，不应往过去日期 (11-21) 塞幽灵复核单
	oldID, err := s.CreateAdhocTask(ctx, AdhocInput{
		Title:        "历史补录任务",
		BusinessDate: "2026-11-20",
		PostID:       &postID,
		AssigneeID:   &wang.ID,
		NeedReview:   true,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = s.CompleteTask(ctx, oldID, wang.ID, wang)
	ghostTasks, _ := s.ListTasks(ctx, "2026-11-21", "all", 0, 0)
	if len(ghostTasks) != 0 {
		t.Fatalf("补完远期历史任务不应污染历史日期 11-21，实际产生 %d 条", len(ghostTasks))
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

// 测试 5：日常垃圾回收维护与在线 VACUUM INTO 备份校验
func TestDailyMaintenanceAndBackup(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	if err := s.RunDailyMaintenance(ctx, s.Now()); err != nil {
		t.Fatalf("每日垃圾清理维护失败: %v", err)
	}
	fName, err := s.PerformBackup(ctx, s.Now())
	if err != nil || fName == "" {
		t.Fatalf("在线安全备份失败: %v", err)
	}
}
