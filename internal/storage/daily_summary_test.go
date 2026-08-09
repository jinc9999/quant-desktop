package storage

import "testing"

func TestDailySummary_CRUD(t *testing.T) {
	db := newTestDB(t)
	s := &DailySummary{
		Mode: "SIMULATION", SummaryDate: "2026-08-09", SummaryType: "daily",
		MarketNotes: "市场测试", CoinAnalysis: "单币测试", Suggestions: "建议测试",
		TodayPnl: 10.5, WinRate: 55, TradeCount: 20, Rating: 6, FeatureJSON: "{}",
	}
	id, exists, err := db.SaveDailySummary(s)
	if err != nil || exists || id <= 0 {
		t.Fatalf("首次保存应新建: id=%d exists=%v err=%v", id, exists, err)
	}

	// 同日期同类型再次保存 = 更新
	s.TodayPnl = 12
	id2, exists2, err := db.SaveDailySummary(s)
	if err != nil || !exists2 || id2 != id {
		t.Fatalf("再次保存应更新: id2=%d exists2=%v err=%v", id2, exists2, err)
	}

	// 查询（时间范围）
	list, err := db.GetDailySummaries("SIMULATION", "2026-08-09", "2026-08-09", "all")
	if err != nil || len(list) != 1 || list[0].TodayPnl != 12 {
		t.Fatalf("查询结果不符: len=%d err=%v", len(list), err)
	}

	// 按 ID 查询
	got, err := db.GetDailySummaryByID("SIMULATION", id)
	if err != nil || got.SummaryDate != "2026-08-09" {
		t.Fatalf("按ID查询失败: %v", err)
	}

	// 软删除后不可见
	ok, err := db.DeleteDailySummary("SIMULATION", id)
	if err != nil || !ok {
		t.Fatalf("删除失败: ok=%v err=%v", ok, err)
	}
	list2, _ := db.GetDailySummaries("SIMULATION", "", "", "all")
	if len(list2) != 0 {
		t.Fatalf("软删除后仍可查询到 %d 条", len(list2))
	}

	// 审计日志
	if err := db.InsertAuditLog("SIMULATION", "CREATE", "daily_summaries/1", "2026-08-09"); err != nil {
		t.Fatalf("审计日志写入失败: %v", err)
	}
}

func TestDailySummary_Validate(t *testing.T) {
	db := newTestDB(t)
	base := &DailySummary{
		Mode: "SIMULATION", SummaryDate: "2026-08-09", SummaryType: "daily",
		MarketNotes: "x", CoinAnalysis: "x", Suggestions: "x",
	}
	cases := []struct {
		name string
		mut  func(*DailySummary)
	}{
		{"日期格式错误", func(s *DailySummary) { s.SummaryDate = "2026/08/09" }},
		{"类型非法", func(s *DailySummary) { s.SummaryType = "monthly" }},
		{"评分越界", func(s *DailySummary) { s.Rating = 11 }},
		{"胜率越界", func(s *DailySummary) { s.WinRate = 101 }},
		{"内容为空", func(s *DailySummary) { s.MarketNotes, s.CoinAnalysis, s.Suggestions = "", "", "" }},
		{"featureJson 非法", func(s *DailySummary) { s.FeatureJSON = "{bad" }},
	}
	for _, c := range cases {
		s := *base
		c.mut(&s)
		if _, _, err := db.SaveDailySummary(&s); err == nil {
			t.Fatalf("%s 应校验失败", c.name)
		}
	}
}

// TestDailySummary_AutoType 系统自动记录（auto）：无正文也可保存，且与 manual 记录互不覆盖
func TestDailySummary_AutoType(t *testing.T) {
	db := newTestDB(t)
	auto := &DailySummary{
		Mode: "SIMULATION", SummaryDate: "2026-08-09", SummaryType: "auto",
		TodayPnl: -5.2, WinRate: 40, TradeCount: 12, FeatureJSON: "{}",
	}
	id, _, err := db.SaveDailySummary(auto)
	if err != nil || id <= 0 {
		t.Fatalf("auto 类型空正文应可保存: id=%d err=%v", id, err)
	}
	manual := &DailySummary{
		Mode: "SIMULATION", SummaryDate: "2026-08-09", SummaryType: "daily",
		MarketNotes: "手动备注", CoinAnalysis: "x", Suggestions: "x",
	}
	id2, _, err := db.SaveDailySummary(manual)
	if err != nil || id2 <= 0 || id2 == id {
		t.Fatalf("auto 与 manual 应分别存储: id=%d id2=%d err=%v", id, id2, err)
	}
	list, err := db.GetDailySummaries("SIMULATION", "", "", "all")
	if err != nil || len(list) != 2 {
		t.Fatalf("应查到 2 条（auto+manual）: len=%d err=%v", len(list), err)
	}
}

// TestDailySummary_ResaveAfterDelete 验证软删除后再次保存同一 (mode, date, type) 可复活原记录。
// 回归背景：daily_summaries 唯一索引包含软删除行，旧实现先查 deleted_at=0 再 INSERT，
// 删除后重新保存会撞唯一约束失败。
func TestDailySummary_ResaveAfterDelete(t *testing.T) {
	db := newTestDB(t)
	s := &DailySummary{
		Mode: "SIMULATION", SummaryDate: "2026-08-09", SummaryType: "daily",
		MarketNotes: "旧备注", CoinAnalysis: "x", Suggestions: "x",
		TodayPnl: 3.5, WinRate: 50, TradeCount: 2, Rating: 5, FeatureJSON: "{}",
	}
	id, _, err := db.SaveDailySummary(s)
	if err != nil || id <= 0 {
		t.Fatalf("首次保存失败: id=%d err=%v", id, err)
	}
	if ok, err := db.DeleteDailySummary("SIMULATION", id); err != nil || !ok {
		t.Fatalf("删除失败: ok=%v err=%v", ok, err)
	}

	s.MarketNotes = "复活后的新备注"
	s.TodayPnl = 9.5
	id2, exists, err := db.SaveDailySummary(s)
	if err != nil {
		t.Fatalf("删除后重新保存应成功（修复唯一索引冲突）: %v", err)
	}
	if id2 != id {
		t.Errorf("复活应复用原记录 ID: 期望 %d, 实际 %d", id, id2)
	}
	if !exists {
		t.Error("复活已有记录应返回 exists=true")
	}
	got, err := db.GetDailySummaryByID("SIMULATION", id)
	if err != nil || got == nil {
		t.Fatalf("复活后按 ID 查询失败: %v", err)
	}
	if got.MarketNotes != "复活后的新备注" || got.TodayPnl != 9.5 {
		t.Errorf("复活后内容未更新: notes=%q pnl=%f", got.MarketNotes, got.TodayPnl)
	}
}
