// sim_live_compare 模拟盘 vs 实盘数据库对比分析工具（只读）
// 用法: go run ./tools/sim_live_compare <模拟盘db> <实盘db>
package main

import (
	"database/sql"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

type Pos struct {
	ID         int64
	Symbol     string
	Side       string
	EntryPrice float64
	Amount     float64
	Leverage   int
	Status     string
	OpenedAt   int64
	ClosedAt   int64
	Reason     string
	Pnl        float64
	ExitPrice  float64
	Fee        float64
}

func openDB(path string) (*sql.DB, error) {
	return sql.Open("sqlite3", "file:"+path+"?mode=ro&_journal_mode=WAL&_busy_timeout=5000")
}

func hasTable(db *sql.DB, name string) bool {
	var n int
	_ = db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?", name).Scan(&n)
	return n > 0
}

func loadPositions(db *sql.DB) []Pos {
	var out []Pos
	rows, err := db.Query(`SELECT id, ifnull(symbol,''), ifnull(side,'LONG'), ifnull(entry_price,0), ifnull(amount,0),
		ifnull(leverage,10), ifnull(status,''), ifnull(opened_at,0), ifnull(closed_at,0),
		ifnull(close_reason,''), ifnull(realized_pnl,0), ifnull(exit_price,0), ifnull(fee,0)
		FROM positions`)
	if err != nil {
		fmt.Println("  positions 查询失败:", err)
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var p Pos
		_ = rows.Scan(&p.ID, &p.Symbol, &p.Side, &p.EntryPrice, &p.Amount, &p.Leverage,
			&p.Status, &p.OpenedAt, &p.ClosedAt, &p.Reason, &p.Pnl, &p.ExitPrice, &p.Fee)
		out = append(out, p)
	}
	return out
}

func loadConfig(db *sql.DB) map[string]string {
	m := map[string]string{}
	if !hasTable(db, "strategy_config") {
		return m
	}
	rows, err := db.Query("SELECT key, value FROM strategy_config")
	if err != nil {
		return m
	}
	defer rows.Close()
	for rows.Next() {
		var k, v string
		_ = rows.Scan(&k, &v)
		m[k] = v
	}
	return m
}

func loadPositionsByStatus(db *sql.DB) map[string][]Pos {
	all := loadPositions(db)
	out := map[string][]Pos{}
	for _, p := range all {
		out[p.Status] = append(out[p.Status], p)
	}
	return out
}

func closed(p []Pos) []Pos {
	var out []Pos
	for _, x := range p {
		if x.Status == "CLOSED" || x.Status == "GHOST" {
			out = append(out, x)
		}
	}
	return out
}

func sum(v []float64) float64 {
	var s float64
	for _, x := range v {
		s += x
	}
	return s
}

func stats(p []Pos) (count int, pnl, fee float64, winRate float64, avgPnl float64) {
	if len(p) == 0 {
		return 0, 0, 0, 0, 0
	}
	wins := 0
	for _, x := range p {
		pnl += x.Pnl
		fee += x.Fee
		if x.Pnl > 0 {
			wins++
		}
	}
	return len(p), pnl, fee, float64(wins) / float64(len(p)) * 100, pnl / float64(len(p))
}

func fmtT(ms int64) string {
	if ms <= 0 {
		return "-"
	}
	return time.UnixMilli(ms).In(time.Local).Format("01-02 15:04:05")
}

func fmtDay(ms int64) string {
	return time.UnixMilli(ms).In(time.Local).Format("2006-01-02")
}

func minMax(p []Pos) (int64, int64) {
	var lo, hi int64
	for i, x := range p {
		if i == 0 || x.OpenedAt < lo {
			lo = x.OpenedAt
		}
		if i == 0 || x.OpenedAt > hi {
			hi = x.OpenedAt
		}
	}
	return lo, hi
}

func report(db *sql.DB, label string) {
	fmt.Printf("\n================ %s ================\n", label)
	byStatus := loadPositionsByStatus(db)
	total := 0
	for _, v := range byStatus {
		total += len(v)
	}
	fmt.Printf("持仓记录总数: %d（状态: ", total)
	keys := make([]string, 0, len(byStatus))
	for k := range byStatus {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Printf("%s=%d ", k, len(byStatus[k]))
	}
	fmt.Println(")")

	all := closed(byStatus["CLOSED"])
	lo, hi := minMax(append(append([]Pos{}, byStatus["OPEN"]...), all...))
	fmt.Printf("时间范围: %s ~ %s（本地时间）\n", fmtT(lo), fmtT(hi))
	if len(all) == 0 {
		fmt.Println("无已平仓记录")
		return
	}
	n, pnl, fee, wr, avg := stats(all)
	fmt.Printf("已平仓: %d 笔 | 合计盈亏 %.2f U | 合计手续费 %.4f U | 胜率 %.1f%% | 单笔均值 %.3f U\n",
		n, pnl, fee, wr, avg)

	// 平仓原因分布
	byReason := map[string][]Pos{}
	for _, x := range all {
		r := x.Reason
		if r == "" {
			r = "(空)"
		}
		byReason[r] = append(byReason[r], x)
	}
	fmt.Println("\n-- 平仓原因分布 --")
	rs := make([]string, 0, len(byReason))
	for k := range byReason {
		rs = append(rs, k)
	}
	sort.Strings(rs)
	for _, r := range rs {
		rn, rp, rf, rw, ra := stats(byReason[r])
		fmt.Printf("  %-12s 笔数=%-4d 盈亏=%8.2f 手续费=%6.3f 胜率=%5.1f%% 单笔均值=%6.3f\n",
			r, rn, rp, rf, rw, ra)
	}

	// 小时分布
	byHour := map[int][]Pos{}
	for _, x := range all {
		h := time.UnixMilli(x.OpenedAt).In(time.Local).Hour()
		byHour[h] = append(byHour[h], x)
	}
	fmt.Println("\n-- 开仓时段分布（本地小时） --")
	for h := 0; h < 24; h++ {
		if len(byHour[h]) == 0 {
			continue
		}
		hn, hp, _, hw, _ := stats(byHour[h])
		fmt.Printf("  %02d时 笔数=%-4d 盈亏=%8.2f 胜率=%5.1f%%\n", h, hn, hp, hw)
	}

	// 每日盈亏序列
	byDay := map[string][]Pos{}
	for _, x := range all {
		byDay[fmtDay(x.OpenedAt)] = append(byDay[fmtDay(x.OpenedAt)], x)
	}
	days := make([]string, 0, len(byDay))
	for k := range byDay {
		days = append(days, k)
	}
	sort.Strings(days)
	fmt.Println("\n-- 每日盈亏 --")
	for _, d := range days {
		dn, dp, df, dw, _ := stats(byDay[d])
		fmt.Printf("  %s 笔数=%-3d 盈亏=%8.2f 手续费=%6.3f 胜率=%5.1f%%\n", d, dn, dp, df, dw)
	}

	// 币种 Top
	bySym := map[string][]Pos{}
	for _, x := range all {
		bySym[x.Symbol] = append(bySym[x.Symbol], x)
	}
	type symStat struct {
		sym string
		n   int
		p   float64
	}
	var ss []symStat
	for s, v := range bySym {
		_, p, _, _, _ := stats(v)
		ss = append(ss, symStat{s, len(v), p})
	}
	sort.Slice(ss, func(i, j int) bool { return ss[i].p > ss[j].p })
	fmt.Println("\n-- 币种盈亏 Top 15 --")
	for i, s := range ss {
		if i >= 15 {
			break
		}
		fmt.Printf("  %-14s 笔数=%-3d 盈亏=%8.2f\n", s.sym, s.n, s.p)
	}
}

func configCompare(sim, live map[string]string) {
	fmt.Println("\n================ 策略参数对比（strategy_config） ================")
	keys := map[string]bool{}
	for k := range sim {
		keys[k] = true
	}
	for k := range live {
		keys[k] = true
	}
	ks := make([]string, 0, len(keys))
	for k := range keys {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	diff := 0
	for _, k := range ks {
		sv := sim[k]
		lv := live[k]
		mark := "  "
		if sv != lv {
			mark = "≠ "
			diff++
		}
		if sv == "" {
			sv = "(无)"
		}
		if lv == "" {
			lv = "(无)"
		}
		fmt.Printf("%s%-28s 模拟盘=%s  实盘=%s\n", mark, k, sv, lv)
	}
	if diff == 0 {
		fmt.Println("（所有已存参数一致）")
	} else {
		fmt.Printf("（发现 %d 个参数不一致，见上方 ≠ 行）\n", diff)
	}
}

// pairTrades 同信号配对：同币种、开仓时间差 <= 180s，贪心最近配对
func pairTrades(sim, live []Pos, overlapStart, overlapEnd int64) (pairs [][2]Pos, simOnly, liveOnly []Pos) {
	var sl, ll []Pos
	for _, p := range sim {
		if p.Status == "CLOSED" && p.OpenedAt >= overlapStart && p.OpenedAt <= overlapEnd {
			sl = append(sl, p)
		}
	}
	for _, p := range live {
		if p.Status == "CLOSED" && p.OpenedAt >= overlapStart && p.OpenedAt <= overlapEnd {
			ll = append(ll, p)
		}
	}
	used := make([]bool, len(ll))
	simOnly = append(simOnly, sl...)
	for _, s := range sl {
		best := -1
		bestGap := int64(180001)
		for j, l := range ll {
			if used[j] {
				continue
			}
			// 价格约束：同信号开仓价应几乎一致（±10%），防止同币种不同仓错配
			if l.EntryPrice > 0 && s.EntryPrice > 0 {
				ratio := l.EntryPrice / s.EntryPrice
				if ratio < 0.9 || ratio > 1.1 {
					continue
				}
			}
			gap := s.OpenedAt - l.OpenedAt
			if gap < 0 {
				gap = -gap
			}
			if gap <= 180000 && gap < bestGap {
				bestGap = gap
				best = j
			}
		}
		if best >= 0 {
			used[best] = true
			pairs = append(pairs, [2]Pos{s, ll[best]})
		}
	}
	// 重新计算 simOnly/liveOnly
	simOnly = simOnly[:0]
	pairedSim := map[int64]bool{}
	pairedLive := map[int64]bool{}
	for _, pr := range pairs {
		pairedSim[pr[0].ID] = true
		pairedLive[pr[1].ID] = true
	}
	for _, p := range sl {
		if !pairedSim[p.ID] {
			simOnly = append(simOnly, p)
		}
	}
	for _, p := range ll {
		if !pairedLive[p.ID] {
			liveOnly = append(liveOnly, p)
		}
	}
	return
}

func main() {
	if len(os.Args) < 3 {
		fmt.Println("用法: go run ./tools/sim_live_compare <模拟盘db> <实盘db> [币种过滤]")
		os.Exit(1)
	}
	symbolFilter := ""
	if len(os.Args) >= 4 {
		symbolFilter = strings.ToUpper(os.Args[3])
	}
	simDB, err := openDB(os.Args[1])
	if err != nil {
		fmt.Println("打开模拟盘失败:", err)
		os.Exit(1)
	}
	defer simDB.Close()
	liveDB, err := openDB(os.Args[2])
	if err != nil {
		fmt.Println("打开实盘失败:", err)
		os.Exit(1)
	}
	defer liveDB.Close()

	if symbolFilter != "" {
		dumpSymbol(simDB, "模拟盘", symbolFilter)
		dumpSymbol(liveDB, "实盘", symbolFilter)
		return
	}

	simCfg := loadConfig(simDB)
	liveCfg := loadConfig(liveDB)
	configCompare(simCfg, liveCfg)

	simByStatus := loadPositionsByStatus(simDB)
	liveByStatus := loadPositionsByStatus(liveDB)
	simAll := append(append([]Pos{}, simByStatus["OPEN"]...), simByStatus["CLOSED"]...)
	liveAll := append(append([]Pos{}, liveByStatus["OPEN"]...), liveByStatus["CLOSED"]...)
	report(simDB, "模拟盘 A")
	report(liveDB, "实盘 A")

	// 重叠窗口与配对
	simLo, simHi := minMax(simAll)
	liveLo, liveHi := minMax(liveAll)
	ovStart := simLo
	if liveLo > ovStart {
		ovStart = liveLo
	}
	ovEnd := simHi
	if liveHi < ovEnd {
		ovEnd = liveHi
	}
	fmt.Printf("\n================ 同信号配对（重叠窗口 %s ~ %s） ================\n",
		fmtT(ovStart), fmtT(ovEnd))
	if ovEnd <= ovStart {
		fmt.Println("两个库无重叠时间窗口，无法配对")
		return
	}
	pairs, simOnly, liveOnly := pairTrades(simByStatus["CLOSED"], liveByStatus["CLOSED"], ovStart, ovEnd)
	fmt.Printf("配对成功: %d 对 | 模拟盘独有: %d 笔 | 实盘独有: %d 笔\n", len(pairs), len(simOnly), len(liveOnly))

	var sumOpenDiff, sumCloseDiff, sumPnlDiff float64
	reasonAgree := 0
	var openDiffs, pnlDiffs []float64
	for _, pr := range pairs {
		s, l := pr[0], pr[1]
		openDiff := (l.EntryPrice - s.EntryPrice) / s.EntryPrice * 100
		closeDiff := 0.0
		if s.ExitPrice > 0 {
			closeDiff = (l.ExitPrice - s.ExitPrice) / s.ExitPrice * 100
		}
		sumOpenDiff += openDiff
		sumCloseDiff += closeDiff
		pnlDiff := s.Pnl - l.Pnl
		sumPnlDiff += pnlDiff
		openDiffs = append(openDiffs, openDiff)
		pnlDiffs = append(pnlDiffs, pnlDiff)
		if s.Reason == l.Reason {
			reasonAgree++
		}
		fmt.Printf("  %-14s 开仓 %s vs %s（差 %+.2f%%）| 平仓 %s vs %s（差 %+.2f%%）| 盈亏 %+.2f vs %+.2f（差 %+.2f）| 原因 %s/%s\n",
			s.Symbol, fmtT(s.OpenedAt), fmtT(l.OpenedAt), openDiff,
			fmtT(s.ClosedAt), fmtT(l.ClosedAt), closeDiff, s.Pnl, l.Pnl, pnlDiff, s.Reason, l.Reason)
	}
	if len(pairs) > 0 {
		fmt.Printf("\n配对均值: 开仓价差 %+.3f%% | 平仓价差 %+.3f%% | 单笔盈亏差(模-实) %+.3f U | 平仓原因一致率 %.1f%%\n",
			sumOpenDiff/float64(len(pairs)), sumCloseDiff/float64(len(pairs)),
			sumPnlDiff/float64(len(pairs)), float64(reasonAgree)/float64(len(pairs))*100)
		// 相关性
		if len(pnlDiffs) >= 3 {
			fmt.Printf("配对单笔盈亏差绝对值: 中位 %.3f U | 最大 %.3f U\n", medianAbs(pnlDiffs), maxAbs(pnlDiffs))
		}
		_ = openDiffs
	}

	// 独有交易摘要
	if len(simOnly) > 0 {
		n, p, _, _, _ := stats(simOnly)
		fmt.Printf("\n模拟盘独有 %d 笔: 盈亏合计 %.2f U\n", n, p)
	}
	if len(liveOnly) > 0 {
		n, p, _, _, _ := stats(liveOnly)
		fmt.Printf("实盘独有 %d 笔: 盈亏合计 %.2f U\n", n, p)
	}

	// 日志事件统计
	fmt.Println("\n================ 关键事件日志统计 ================")
	countEvent(simDB, "模拟盘", []string{"余额不足", "数量修正", "-4131", "幽灵", "开仓确认", "降级", "减半"})
	countEvent(liveDB, "实盘", []string{"余额不足", "数量修正", "-4131", "幽灵", "开仓确认", "降级", "减半"})

	// 启动配置日志（判断实盘是否加载了持久化参数）
	fmt.Println("\n================ 启动/配置相关日志（最近 20 条） ================")
	dumpConfigLogs(simDB, "模拟盘")
	dumpConfigLogs(liveDB, "实盘")

	// 订单真实成交价 vs 本地记账价（滑点）
	fmt.Println("\n================ 订单成交价 vs 本地记账价 ================")
	dumpSlippage(simDB, "模拟盘")
	dumpSlippage(liveDB, "实盘")

	fmt.Println("\n================ orders 表概览 ================")
	dumpOrders(simDB, "模拟盘")
	dumpOrders(liveDB, "实盘")

	fmt.Println("\n================ 条件单成交价 vs 本地出场记账价（真实平仓滑点） ================")
	dumpCloseSlippage(simDB, "模拟盘")
	dumpCloseSlippage(liveDB, "实盘")

	fmt.Println("\n================ 模拟盘独有交易的实盘取证 ================")
	forensics(simDB, liveDB, simOnly, ovStart, ovEnd)

	fmt.Println("\n================ 实盘日志时段覆盖（按小时） ================")
	dumpLogHours(liveDB, ovStart, ovEnd)
}

// dumpSymbol 打印指定币种的全部持仓原始记录
func dumpSymbol(db *sql.DB, label, symbol string) {
	fmt.Printf("\n==== %s %s 原始记录 ====\n", label, symbol)
	rows, err := db.Query(`SELECT id, ifnull(side,''), ifnull(entry_price,0), ifnull(amount,0),
		ifnull(leverage,0), ifnull(status,''), ifnull(opened_at,0), ifnull(closed_at,0),
		ifnull(close_reason,''), ifnull(realized_pnl,0), ifnull(exit_price,0), ifnull(fee,0)
		FROM positions WHERE symbol=? ORDER BY opened_at`, symbol)
	if err != nil {
		fmt.Println("  查询失败:", err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var side, status, reason string
		var ep, amt, pnl, xp, fee float64
		var lev int
		var oa, ca int64
		_ = rows.Scan(&id, &side, &ep, &amt, &lev, &status, &oa, &ca, &reason, &pnl, &xp, &fee)
		fmt.Printf("  #%-4d %-5s 开=%s 入场=%g 数量=%g 杠杆=%d | 平=%s 出场=%g 原因=%-12s 盈亏=%+.2f 费=%.4f 状态=%s\n",
			id, side, fmtT(oa), ep, amt, lev, fmtT(ca), xp, reason, pnl, fee, status)
	}
	// 关联订单（止损/跟踪状态回放）
	if hasTable(db, "orders") {
		orows, err := db.Query(`SELECT o.position_id, o.order_type, o.side, o.status, o.stop_price, o.activation_price, o.callback_rate, o.filled_price,
			datetime(o.created_at/1000,'unixepoch','localtime'), datetime(o.updated_at/1000,'unixepoch','localtime')
			FROM orders o JOIN positions p ON p.id=o.position_id WHERE p.symbol=?
			ORDER BY o.created_at`, symbol)
		if err == nil {
			defer orows.Close()
			for orows.Next() {
				var pid int64
				var ot, side, st string
				var sp, ap, cb, fp float64
				var ct, ut string
				_ = orows.Scan(&pid, &ot, &side, &st, &sp, &ap, &cb, &fp, &ct, &ut)
				fpStr := "-"
				if fp > 0 {
					fpStr = fmt.Sprintf("%g", fp)
				}
				fmt.Printf("    订单 #%-4d %-20s %-4s 状态=%-12s 触发价=%g 激活价=%g 回调=%g%% 成交价=%s 创建=%s 更新=%s\n",
					pid, ot, side, st, sp, ap, cb, fpStr, ct, ut)
			}
		}
	}
}

func countEvent(db *sql.DB, label string, keywords []string) {
	fmt.Printf("[%s]\n", label)
	if !hasTable(db, "trade_logs") {
		fmt.Println("  无 trade_logs")
		return
	}
	for _, kw := range keywords {
		var n int
		_ = db.QueryRow("SELECT COUNT(*) FROM trade_logs WHERE message LIKE ?", "%"+kw+"%").Scan(&n)
		fmt.Printf("  含「%s」日志: %d 条\n", kw, n)
	}
}

func dumpConfigLogs(db *sql.DB, label string) {
	fmt.Printf("[%s]\n", label)
	if !hasTable(db, "trade_logs") {
		fmt.Println("  无 trade_logs")
		return
	}
	rows, err := db.Query(`SELECT datetime(timestamp/1000,'unixepoch','localtime'), module, substr(message,1,120)
		FROM trade_logs
		WHERE message LIKE '%配置%' OR message LIKE '%策略已启动%' OR message LIKE '%默认%' OR message LIKE '%持久化%'
		ORDER BY timestamp DESC LIMIT 20`)
	if err != nil {
		fmt.Println("  查询失败:", err)
		return
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var ts, mod, msg string
		_ = rows.Scan(&ts, &mod, &msg)
		fmt.Printf("  [%s] %s %s\n", ts, mod, msg)
		n++
	}
	if n == 0 {
		fmt.Println("  （无相关日志）")
	}
}

func dumpSlippage(db *sql.DB, label string) {
	fmt.Printf("[%s]\n", label)
	if !hasTable(db, "orders") {
		fmt.Println("  无 orders 表")
		return
	}
	// 市价开仓单：成交均价 vs 本地持仓记账价
	rows, err := db.Query(`SELECT p.symbol, p.id, p.entry_price, o.filled_price, o.filled_amount, o.status,
		datetime(p.opened_at/1000,'unixepoch','localtime')
		FROM orders o JOIN positions p ON p.id = o.position_id
		WHERE o.order_type='MARKET' AND o.side='BUY' AND o.filled_price > 0 AND p.entry_price > 0
		ORDER BY o.created_at DESC LIMIT 25`)
	if err != nil {
		fmt.Println("  查询失败:", err)
		return
	}
	defer rows.Close()
	n := 0
	var sumDiff float64
	for rows.Next() {
		var sym string
		var pid int64
		var ep, fp, fa float64
		var st, ts string
		_ = rows.Scan(&sym, &pid, &ep, &fp, &fa, &st, &ts)
		diff := (fp - ep) / ep * 100
		sumDiff += diff
		fmt.Printf("  %-12s #%-4d 开=%s 记账价=%g 成交均价=%g 差=%+.3f%% 状态=%s\n",
			sym, pid, ts, ep, fp, diff, st)
		n++
	}
	if n > 0 {
		fmt.Printf("  → 样本 %d 笔，成交价相对记账价平均 %+.3f%%\n", n, sumDiff/float64(n))
	} else {
		fmt.Println("  （无匹配的市价开仓成交记录）")
	}
}

func dumpOrders(db *sql.DB, label string) {
	fmt.Printf("[%s]\n", label)
	if !hasTable(db, "orders") {
		fmt.Println("  无 orders 表")
		return
	}
	rows, err := db.Query(`SELECT order_type, side, status, COUNT(*), COUNT(CASE WHEN filled_price>0 THEN 1 END)
		FROM orders GROUP BY order_type, side, status ORDER BY COUNT(*) DESC LIMIT 15`)
	if err != nil {
		fmt.Println("  查询失败:", err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var ot, side, st string
		var n, filled int
		_ = rows.Scan(&ot, &side, &st, &n, &filled)
		fmt.Printf("  %-18s %-5s %-10s 笔数=%-4d 已成交通数=%d\n", ot, side, st, n, filled)
	}
	// 样本：条件单成交价 vs 持仓出场价
	rows2, err := db.Query(`SELECT o.symbol, o.position_id, o.order_type, o.filled_price, o.amount,
		datetime(o.created_at/1000,'unixepoch','localtime')
		FROM orders o WHERE o.filled_price > 0 ORDER BY o.created_at DESC LIMIT 8`)
	if err == nil {
		defer rows2.Close()
		for rows2.Next() {
			var sym, ot, ts string
			var pid int64
			var fp, amt float64
			_ = rows2.Scan(&sym, &pid, &ot, &fp, &amt, &ts)
			fmt.Printf("  * %-12s #%-4d %-18s 成交价=%g 数量=%g @ %s\n", sym, pid, ot, fp, amt, ts)
		}
	}
}

func dumpCloseSlippage(db *sql.DB, label string) {
	fmt.Printf("[%s]\n", label)
	if !hasTable(db, "orders") {
		fmt.Println("  无 orders 表")
		return
	}
	rows, err := db.Query(`SELECT p.symbol, p.id, p.exit_price, o.filled_price, o.order_type,
		datetime(p.closed_at/1000,'unixepoch','localtime')
		FROM orders o JOIN positions p ON p.id = o.position_id
		WHERE o.status='FILLED' AND o.filled_price > 0 AND p.exit_price > 0
		ORDER BY p.closed_at DESC`)
	if err != nil {
		fmt.Println("  查询失败:", err)
		return
	}
	defer rows.Close()
	type row struct {
		sym, ot, ts string
		pid         int64
		ep, fp      float64
		diff        float64
	}
	var rows2 []row
	var sumDiff, sumAbs float64
	for rows.Next() {
		var r row
		_ = rows.Scan(&r.sym, &r.pid, &r.ep, &r.fp, &r.ot, &r.ts)
		r.diff = (r.fp - r.ep) / r.ep * 100
		sumDiff += r.diff
		sumAbs += math.Abs(r.diff)
		rows2 = append(rows2, r)
	}
	if len(rows2) == 0 {
		fmt.Println("  （无 FILLED 条件单 + 本地出场价记录）")
		return
	}
	fmt.Printf("  样本 %d 笔：成交价相对本地记账价 均值 %+.3f%% | 绝对偏差均值 %.3f%%\n",
		len(rows2), sumDiff/float64(len(rows2)), sumAbs/float64(len(rows2)))
	for _, r := range rows2 {
		fmt.Printf("  %-12s #%-4d %-20s 本地出场=%g 真实成交=%g 差=%+.3f%% @ %s\n",
			r.sym, r.pid, r.ot, r.ep, r.fp, r.diff, r.ts)
	}
}

type LogRow struct {
	Ts  int64
	Msg string
}

func loadLogs(db *sql.DB, since, until int64) []LogRow {
	var out []LogRow
	if !hasTable(db, "trade_logs") {
		return out
	}
	rows, err := db.Query(`SELECT timestamp, ifnull(message,'') FROM trade_logs
		WHERE timestamp >= ? AND timestamp <= ? ORDER BY timestamp`, since, until)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var l LogRow
		_ = rows.Scan(&l.Ts, &l.Msg)
		out = append(out, l)
	}
	return out
}

func forensics(simDB, liveDB *sql.DB, simOnly []Pos, ovStart, ovEnd int64) {
	livePositions := loadPositions(liveDB)
	liveLogs := loadLogs(liveDB, ovStart-600000, ovEnd+600000)
	kw := []string{"跳过", "冷却", "熔断", "拉黑", "黑名单", "余额", "开仓失败", "开仓被拒", "不支持"}
	fmt.Printf("实盘日志窗口内样本: %d 条 | 实盘全部持仓: %d 条\n", len(liveLogs), len(livePositions))
	bySym := map[string][]Pos{}
	for _, p := range livePositions {
		bySym[p.Symbol] = append(bySym[p.Symbol], p)
	}
	cat := map[string]int{}
	for _, s := range simOnly {
		foundPos := false
		for _, lp := range bySym[s.Symbol] {
			if abs64(lp.OpenedAt-s.OpenedAt) <= 600000 {
				foundPos = true
				break
			}
		}
		if foundPos {
			cat["实盘也开了仓（配对漏掉/价格差异）"]++
			fmt.Printf("  %-12s 开=%s 模盈=%+.2f → 实盘同币 ±10 分钟内也有持仓（配对漏掉）\n",
				s.Symbol, fmtT(s.OpenedAt), s.Pnl)
			continue
		}
		evidence := ""
		for _, l := range liveLogs {
			if abs64(l.Ts-s.OpenedAt) <= 600000 && (strings.Contains(l.Msg, s.Symbol) || containsAny(l.Msg, kw)) {
				evidence = fmt.Sprintf("[%s] %s", fmtT(l.Ts), truncate(l.Msg, 90))
				break
			}
		}
		if evidence != "" {
			cat["实盘有相关日志（跳过/失败等）"]++
			fmt.Printf("  %-12s 开=%s 模盈=%+.2f → %s\n", s.Symbol, fmtT(s.OpenedAt), s.Pnl, evidence)
		} else {
			cat["实盘无任何记录（信号未触发或未同步）"]++
			fmt.Printf("  %-12s 开=%s 模盈=%+.2f → 实盘无记录\n", s.Symbol, fmtT(s.OpenedAt), s.Pnl)
		}
	}
	fmt.Println("分类汇总:")
	for k, v := range cat {
		fmt.Printf("  %s: %d 笔\n", k, v)
	}
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

func containsAny(s string, kws []string) bool {
	for _, k := range kws {
		if strings.Contains(s, k) {
			return true
		}
	}
	return false
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func dumpLogHours(db *sql.DB, since, until int64) {
	if !hasTable(db, "trade_logs") {
		fmt.Println("  无 trade_logs")
		return
	}
	rows, err := db.Query(`SELECT strftime('%Y-%m-%d %H:00','now','localtime'), COUNT(*)
		FROM trade_logs WHERE timestamp >= ? AND timestamp <= ?
		GROUP BY 1 ORDER BY 1`, since, until)
	if err != nil {
		fmt.Println("  查询失败:", err)
		return
	}
	defer rows.Close()
	// 改为按 timestamp 计算本地小时
	rows2, err := db.Query(`SELECT (timestamp/3600000) FROM trade_logs WHERE timestamp >= ? AND timestamp <= ?`, since, until)
	if err != nil {
		return
	}
	defer rows2.Close()
	hours := map[int64]int{}
	for rows2.Next() {
		var h int64
		_ = rows2.Scan(&h)
		hours[h]++
	}
	if len(hours) == 0 {
		fmt.Println("  窗口内无实盘日志")
		return
	}
	hs := make([]int64, 0, len(hours))
	for h := range hours {
		hs = append(hs, h)
	}
	sort.Slice(hs, func(i, j int) bool { return hs[i] < hs[j] })
	for _, h := range hs {
		fmt.Printf("  %s 日志=%d 条\n", time.UnixMilli(h*3600000).In(time.Local).Format("01-02 15:00"), hours[h])
	}
	_ = rows
}

func medianAbs(v []float64) float64 {
	a := make([]float64, len(v))
	for i, x := range v {
		if x < 0 {
			x = -x
		}
		a[i] = x
	}
	sort.Float64s(a)
	return a[len(a)/2]
}

func maxAbs(v []float64) float64 {
	m := 0.0
	for _, x := range v {
		if math.Abs(x) > m {
			m = math.Abs(x)
		}
	}
	return m
}
