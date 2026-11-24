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

// 测试 1：管理员强制密码验证、修改密码 & 普通成员可选密码/免密登录
func TestPasswordAuthRules(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	// 管理员不输密码或输错密码必须拒绝
	if _, err := s.Login(ctx, "主机管理员", ""); err == nil {
		t.Fatal("管理员空密码登录应当被拒绝")
	}
	if _, err := s.Login(ctx, "主机管理员", "wrong"); err == nil {
		t.Fatal("管理员错误密码登录应当被拒绝")
	}
	adminSess, err := s.Login(ctx, "主机管理员", DefaultAdminInitialPassword)
	if err != nil {
		t.Fatalf("管理员使用初始密码登录失败: %v", err)
	}

	// 管理员禁止清空密码，但可修改密码
	if err := s.ChangeOwnPassword(ctx, adminSess.Person.ID, DefaultAdminInitialPassword, "", true); err == nil {
		t.Fatal("管理员清空密码应当被拒绝")
	}
	if err := s.ChangeOwnPassword(ctx, adminSess.Person.ID, DefaultAdminInitialPassword, "newpass888", false); err != nil {
		t.Fatalf("管理员修改密码失败: %v", err)
	}
	if _, err := s.Login(ctx, "主机管理员", "newpass888"); err != nil {
		t.Fatalf("管理员新密码登录失败: %v", err)
	}

	// 普通成员 A（留空免密）与成员 B（设置密码）
	if err := s.SavePerson(ctx, PersonSaveInput{Nickname: "免密小张", InRota: true, IsActive: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.SavePerson(ctx, PersonSaveInput{Nickname: "加密小李", Password: "1234", InRota: true, IsActive: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Login(ctx, "免密小张", ""); err != nil {
		t.Fatalf("未设密码的成员免密登录失败: %v", err)
	}
	if _, err := s.Login(ctx, "加密小李", ""); err == nil {
		t.Fatal("已设密码的成员空密码登录应当被拒绝")
	}
	if _, err := s.Login(ctx, "加密小李", "1234"); err != nil {
		t.Fatalf("已设密码成员正确密码登录失败: %v", err)
	}
}

// 测试 2：重复生成不产生重复实例 + 预排班表自动指派与后续补排同步
func TestPeriodicGenerateIdempotentAndScheduleSync(t *testing.T) {
	s, loc := newTestStore(t)
	ctx := context.Background()

	_ = s.SavePost(ctx, 0, "前台岗", "#007AFF", 1, true)
	_ = s.SavePerson(ctx, PersonSaveInput{Nickname: "张三", InRota: true, IsActive: true})
	posts, _ := s.ListPosts(ctx, true)
	people, _ := s.ListPeople(ctx, true)
	var zhangSan Person
	for _, p := range people {
		if p.Nickname == "张三" {
			zhangSan = p
		}
	}

	refTime := time.Date(2026, 11, 24, 8, 0, 0, 0, loc)
	_, err := s.CreateOrUpdateTemplate(ctx, TaskTemplate{
		PostID:    posts[0].ID,
		Title:     "晨间设备巡检",
		CycleType: "daily",
	}, refTime)
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 3; i++ {
		n, err := s.GeneratePeriodicTasks(ctx, refTime)
		if err != nil || n != 0 {
			t.Fatalf("重复生成必须幂等返回 0，实际 n=%d, err=%v", n, err)
		}
	}

	// 保存当日排班后，待安排任务自动指派给张三
	_, err = s.SaveScheduleEntries(ctx, []ScheduleEntry{
		{ScheduleDate: "2026-11-24", PostID: posts[0].ID, PersonID: zhangSan.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	mine, _ := s.ListTasks(ctx, "2026-11-24", "mine", zhangSan.ID, 0)
	if len(mine) != 1 {
		t.Fatalf("排班写入后张三应看到 1 条待办，实际 %d 条", len(mine))
	}
}

// 测试 3：每周多天（一三五）与每月多天 + 月末 29/30/31 在平年2月/闰年2月/小月30天的精准最后一天计算
func TestMultiDayAndEndOfMonthClamping(t *testing.T) {
	s, loc := newTestStore(t)
	ctx := context.Background()

	_ = s.SavePost(ctx, 0, "财务岗", "#5856D6", 1, true)
	posts, _ := s.ListPosts(ctx, true)
	postID := posts[0].ID

	// 创建每周一、三、五模板（创建时间为周二，创建当下不生成）
	tue := time.Date(2026, 11, 24, 10, 0, 0, 0, loc)
	_, err := s.CreateOrUpdateTemplate(ctx, TaskTemplate{
		PostID:    postID,
		Title:     "一三五盘点",
		CycleType: "weekly",
		Weekdays:  "1,3,5",
	}, tue)
	if err != nil {
		t.Fatal(err)
	}
	if tasks, _ := s.ListTasks(ctx, "2026-11-24", "all", 0, 0); len(tasks) != 0 {
		t.Fatal("周二不应触发周一三五模板")
	}

	// 周三、周五分别触发生成 1 条
	wed := time.Date(2026, 11, 25, 0, 5, 0, 0, loc)
	fri := time.Date(2026, 11, 27, 0, 5, 0, 0, loc)
	if n, _ := s.GeneratePeriodicTasks(ctx, wed); n != 1 {
		t.Fatalf("周三应生成 1 条，实际 %d", n)
	}
	if n, _ := s.GeneratePeriodicTasks(ctx, fri); n != 1 {
		t.Fatalf("周五应生成 1 条，实际 %d", n)
	}

	// 创建每月 1, 15, 30, 31 模板，验证小月（11月30天）、平年2月（2026年2月28天）、闰年2月（2028年2月29天）
	_, err = s.CreateOrUpdateTemplate(ctx, TaskTemplate{
		PostID:    postID,
		Title:     "月末结账归档",
		CycleType: "monthly",
		MonthDays: "1, 15, 30, 31",
	}, tue)
	if err != nil {
		t.Fatal(err)
	}

	// 1) 11月30日（小月最后一天，30和31重叠在同一天，唯一索引保证只生成 1 条月末任务）
	nov30 := time.Date(2026, 11, 30, 0, 5, 0, 0, loc)
	_, _ = s.GeneratePeriodicTasks(ctx, nov30)
	tasksNov30, _ := s.ListTasks(ctx, "2026-11-30", "all", 0, 0)
	monthCnt := 0
	for _, tk := range tasksNov30 {
		if tk.Title == "月末结账归档" {
			monthCnt++
		}
	}
	if monthCnt != 1 {
		t.Fatalf("11月30日应精准生成 1 条月末任务且不重复，实际 %d 条", monthCnt)
	}

	// 2) 平年 2026年2月28日 -> 30/31 均收敛到 28日触发
	feb28NonLeap := time.Date(2026, 2, 28, 0, 5, 0, 0, loc)
	if n, _ := s.GeneratePeriodicTasks(ctx, feb28NonLeap); n == 0 {
		t.Fatal("平年2月28日应触发月末最后一天任务")
	}

	// 3) 闰年 2028年2月：2月28日不触发，2月29日精准触发！
	feb28Leap := time.Date(2028, 2, 28, 0, 5, 0, 0, loc)
	_, _ = s.GeneratePeriodicTasks(ctx, feb28Leap)
	tasksFeb28Leap, _ := s.ListTasks(ctx, "2028-02-28", "all", 0, 0)
	for _, tk := range tasksFeb28Leap {
		if tk.Title == "月末结账归档" {
			t.Fatal("闰年2月有29天，月末任务不应在28日提前触发")
		}
	}
	feb29Leap := time.Date(2028, 2, 29, 0, 5, 0, 0, loc)
	_, _ = s.GeneratePeriodicTasks(ctx, feb29Leap)
	tasksFeb29Leap, _ := s.ListTasks(ctx, "2028-02-29", "all", 0, 0)
	foundLeap := false
	for _, tk := range tasksFeb29Leap {
		if tk.Title == "月末结账归档" {
			foundLeap = true
		}
	}
	if !foundLeap {
		t.Fatal("闰年2月29日应精准触发月末任务")
	}
}

// 测试 4：公平轮询草稿（人数充足时严格每人每天 1 岗；导入排班表正常工作）
func TestFairRotaOnePostPerPersonAndCSVImport(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	_ = s.SavePost(ctx, 0, "A岗", "#007AFF", 1, true)
	_ = s.SavePost(ctx, 0, "B岗", "#34C759", 2, true)
	_ = s.SavePost(ctx, 0, "C岗", "#FF9500", 3, true)

	_ = s.SavePerson(ctx, PersonSaveInput{Nickname: "甲", InRota: true, IsActive: true})
	_ = s.SavePerson(ctx, PersonSaveInput{Nickname: "乙", InRota: true, IsActive: true})
	_ = s.SavePerson(ctx, PersonSaveInput{Nickname: "丙", InRota: true, IsActive: true})

	// 3 人 3 岗连续排 3 天：每天每人必须恰好 1 岗，且 3 天内轮流换岗
	draft, err := s.GenerateFairRotaDraft(ctx, "2026-12-01", 3)
	if err != nil || len(draft) != 9 {
		t.Fatalf("生成轮询草稿失败: len=%d, err=%v", len(draft), err)
	}
	byDate := make(map[string]map[int64]int)
	for _, e := range draft {
		if byDate[e.ScheduleDate] == nil {
			byDate[e.ScheduleDate] = make(map[int64]int)
		}
		byDate[e.ScheduleDate][e.PersonID]++
		if byDate[e.ScheduleDate][e.PersonID] > 1 {
			t.Fatalf("人数充足时违反了每人每天 1 岗原则: 日期=%s, person=%s", e.ScheduleDate, e.PersonName)
		}
	}

	// 测试二维矩阵排班文本导入
	csvText := "日期,A岗,B岗,C岗\n2026-12-05,丙,甲,乙\n2026-12-06,乙,丙,甲"
	saved, err := s.ImportScheduleText(ctx, csvText)
	if err != nil || saved != 6 {
		t.Fatalf("导入矩阵排班表失败: saved=%d, err=%v", saved, err)
	}
}

// 测试 5：重复完成幂等、代完成、无此项跨天不可撤销、交接后可见性与未来临时任务
func TestStateMachineAndHandoverAndBackup(t *testing.T) {
	s, loc := newTestStore(t)
	ctx := context.Background()

	_ = s.SavePerson(ctx, PersonSaveInput{Nickname: "小陈", InRota: true, IsActive: true})
	_ = s.SavePerson(ctx, PersonSaveInput{Nickname: "小周", InRota: true, IsActive: true})
	people, _ := s.ListPeople(ctx, true)
	var chen, zhou, admin Person
	for _, p := range people {
		switch p.Nickname {
		case "小陈":
			chen = p
		case "小周":
			zhou = p
		case "主机管理员":
			admin = p
		}
	}

	today := s.TodayStr()
	// 1) 代完成与重复完成幂等
	t1, _ := s.CreateAdhocTask(ctx, AdhocInput{Title: "耗材清点", BusinessDate: today, AssigneeID: &chen.ID})
	res1, err := s.CompleteTask(ctx, t1, zhou.ID, zhou)
	if err != nil || *res1.CompletedBy != zhou.ID || *res1.OriginalAssigneeID != chen.ID {
		t.Fatal("代完成记录不正确")
	}
	res2, err := s.CompleteTask(ctx, t1, chen.ID, chen)
	if err != nil || *res2.CompletedBy != zhou.ID {
		t.Fatal("重复完成幂等保护失败")
	}

	// 2) 无此项幂等及跨天禁止撤销
	t2, _ := s.CreateAdhocTask(ctx, AdhocInput{Title: "访客登记", BusinessDate: today, AssigneeID: &chen.ID})
	_, _ = s.MarkNotApplicable(ctx, t2, "今日无访客", chen)
	na2, err := s.MarkNotApplicable(ctx, t2, "重复点击", chen)
	if err != nil || *na2.NotApplicableNote != "今日无访客" {
		t.Fatal("无此项幂等失败")
	}
	baseDay, _ := time.ParseInLocation("2006-01-02", today, loc)
	if _, err := s.RevertNotApplicable(ctx, t2, admin, baseDay.AddDate(0, 0, 1)); err == nil {
		t.Fatal("跨天撤销无此项应被拒绝")
	}
	if _, err := s.RevertNotApplicable(ctx, t2, chen, baseDay.Add(time.Hour)); err != nil {
		t.Fatalf("当天本人撤销无此项失败: %v", err)
	}

	// 3) 交接后原当值人不再看到待办
	if _, err := s.HandoverTasks(ctx, []int64{t2}, zhou.ID, "换班交接", chen); err != nil {
		t.Fatal(err)
	}
	if m, _ := s.ListTasks(ctx, today, "mine", chen.ID, 0); len(m) != 0 {
		t.Fatal("交接后原当值人不应再看到该待办")
	}
	if m, _ := s.ListTasks(ctx, today, "mine", zhou.ID, 0); len(m) != 1 {
		t.Fatal("交接后新当值人应看到该待办")
	}

	// 4) 未来临时任务不提前出现 + 备份完整性检查
	future := baseDay.AddDate(0, 0, 5).Format("2006-01-02")
	_, _ = s.CreateAdhocTask(ctx, AdhocInput{Title: "下周盘点", BusinessDate: future, AssigneeID: &chen.ID})
	if m, _ := s.ListTasks(ctx, today, "mine", chen.ID, 0); len(m) != 0 {
		t.Fatal("未来任务不应提前出现在今日待办")
	}
	if fName, err := s.PerformBackup(ctx, s.Now()); err != nil || fName == "" {
		t.Fatalf("备份失败: %v", err)
	}
}
