// livepna 实盘交易深度分析器: 按币/按笔/持仓时长/时段/追涨vs回踩 解剖盈亏。
// 用法: go run ./tools/livepna <quant_live.db路径> <输出目录>
package main

import (
	"database/sql"
	"encoding/csv"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

type trade struct {
	Symbol    string
	OpenedAt  int64
	ClosedAt  int64
	EntryPx   float64
	ExitPx    float64
	Amount    float64
	Pnl       float64
	Fee       float64
	Reason    string
	PnlPct    float64 // 相对名义价值的百分比
	HeldMin   int
	ChaseType string // chase=追涨 / pullback=回踩 / first=首笔
}

func main() {
	if len(os.Args) < 3 {
		fmt.Println("用法: livepna <db路径> <输出目录>")
		os.Exit(1)
	}
	db, err := sql.Open("sqlite3", "file:"+os.Args[1]+"?mode=ro&_journal_mode=WAL")
	if err != nil {
		fmt.Println("打开失败:", err)
		os.Exit(1)
	}
	defer db.Close()
	outDir := os.Args[2]
	os.MkdirAll(outDir, 0o755)

	rows, err := db.Query(`SELECT symbol, opened_at, closed_at, entry_price, exit_price, amount, realized_pnl, ifnull(fee,0), ifnull(close_reason,'?')
		FROM positions WHERE status='CLOSED' ORDER BY opened_at`)
	if err != nil {
		fmt.Println("查询失败:", err)
		os.Exit(1)
	}
	defer rows.Close()

	var trades []*trade
	for rows.Next() {
		t := &trade{}
		var reason string
		if err := rows.Scan(&t.Symbol, &t.OpenedAt, &t.ClosedAt, &t.EntryPx, &t.ExitPx, &t.Amount, &t.Pnl, &t.Fee, &reason); err != nil {
			continue
		}
		t.Reason = reason
		if t.EntryPx > 0 && t.Amount > 0 {
			t.PnlPct = t.Pnl / (t.EntryPx * t.Amount) * 100
		}
		t.HeldMin = int((t.ClosedAt - t.OpenedAt) / 60000)
		trades = append(trades, t)
	}

	// 追涨/回踩分类: 同一币按时间顺序，第二笔入场价 > 第一笔 → 追涨；< → 回踩
	lastEntry := map[string]float64{}
	for _, t := range trades {
		if prev, ok := lastEntry[t.Symbol]; ok {
			if t.EntryPx > prev {
				t.ChaseType = "chase"
			} else if t.EntryPx < prev {
				t.ChaseType = "pullback"
			} else {
				t.ChaseType = "flat"
			}
		} else {
			t.ChaseType = "first"
		}
		lastEntry[t.Symbol] = t.EntryPx
	}

	// ===== 每币汇总 =====
	type coinStat struct {
		symbol      string
		count       int
		wins        int
		losses      int
		pnl         float64
		first, last string
		reasons     map[string]int
	}
	coins := map[string]*coinStat{}
	for _, t := range trades {
		c, ok := coins[t.Symbol]
		if !ok {
			c = &coinStat{symbol: t.Symbol, reasons: map[string]int{}}
			coins[t.Symbol] = c
		}
		c.count++
		if t.Pnl > 0 {
			c.wins++
		} else {
			c.losses++
		}
		c.pnl += t.Pnl
		c.reasons[t.Reason]++
	}
	var coinList []*coinStat
	for _, c := range coins {
		coinList = append(coinList, c)
	}
	sort.Slice(coinList, func(i, j int) bool { return coinList[i].pnl > coinList[j].pnl })

	// CSV: 每笔明细
	f1, _ := os.Create(filepath.Join(outDir, "per_trade.csv"))
	w1 := csv.NewWriter(f1)
	w1.Write([]string{"symbol", "opened_at", "closed_at", "entry", "exit", "amount", "pnl", "fee", "pnl_pct", "held_min", "reason", "chase_type"})
	for _, t := range trades {
		w1.Write([]string{t.Symbol, time.UnixMilli(t.OpenedAt).Format("2006-01-02 15:04:05"), time.UnixMilli(t.ClosedAt).Format("2006-01-02 15:04:05"),
			strconv.FormatFloat(t.EntryPx, 'f', 8, 64), strconv.FormatFloat(t.ExitPx, 'f', 8, 64), strconv.FormatFloat(t.Amount, 'f', 6, 64),
			strconv.FormatFloat(t.Pnl, 'f', 2, 64), strconv.FormatFloat(t.Fee, 'f', 4, 64), strconv.FormatFloat(t.PnlPct, 'f', 2, 64),
			strconv.Itoa(t.HeldMin), t.Reason, t.ChaseType})
	}
	w1.Flush()
	f1.Close()

	// CSV: 每币汇总
	f2, _ := os.Create(filepath.Join(outDir, "per_coin.csv"))
	w2 := csv.NewWriter(f2)
	w2.Write([]string{"symbol", "count", "wins", "losses", "win_rate", "pnl", "avg_pnl_per_trade", "reasons"})
	for _, c := range coinList {
		wr := 0.0
		if c.count > 0 {
			wr = float64(c.wins) / float64(c.count) * 100
		}
		w2.Write([]string{c.symbol, strconv.Itoa(c.count), strconv.Itoa(c.wins), strconv.Itoa(c.losses),
			strconv.FormatFloat(wr, 'f', 1, 64), strconv.FormatFloat(c.pnl, 'f', 2, 64),
			strconv.FormatFloat(c.pnl/float64(c.count), 'f', 2, 64), fmt.Sprint(c.reasons)})
	}
	w2.Flush()
	f2.Close()

	// ===== 输出汇总 =====
	fmt.Printf("总交易: %d 笔 | 总盈亏: %.2fU | 总手续费记录: %.4fU\n", len(trades), sumPnl(trades), sumFee(trades))
	fmt.Printf("\n===== 每币盈亏排名（前 20 / 后 10）=====\n")
	fmt.Printf("%-12s %5s %5s %5s %6s %10s %8s  %s\n", "币种", "笔数", "盈", "亏", "胜率%", "总盈亏", "均/笔", "平仓原因")
	for i, c := range coinList {
		if i < 20 || i >= len(coinList)-10 {
			wr := 0.0
			if c.count > 0 {
				wr = float64(c.wins) / float64(c.count) * 100
			}
			fmt.Printf("%-12s %5d %5d %5d %6.1f %10.2f %8.2f  %v\n", c.symbol, c.count, c.wins, c.losses, wr, c.pnl, c.pnl/float64(c.count), c.reasons)
		}
	}

	// 集中度
	sort.Slice(coinList, func(i, j int) bool { return coinList[i].pnl > coinList[j].pnl })
	top10 := 0.0
	for i := 0; i < 10 && i < len(coinList); i++ {
		top10 += coinList[i].pnl
	}
	total := sumPnl(trades)
	fmt.Printf("\n前 10 大盈利币合计: %.2fU（占总盈亏 %.1f%%）\n", top10, safePct(top10, total))

	// ===== 持仓时长分布 =====
	fmt.Printf("\n===== 持仓时长分布（按 15 分钟桶）=====\n")
	type bucket struct{ min int }
	buckets := map[int]*[2]int{} // 桶 -> [笔数, 盈亏*100]
	for _, t := range trades {
		b := t.HeldMin / 15 * 15
		if buckets[b] == nil {
			buckets[b] = &[2]int{}
		}
		buckets[b][0]++
		buckets[b][1] += int(t.Pnl * 100)
	}
	var bk []int
	for k := range buckets {
		bk = append(bk, k)
	}
	sort.Ints(bk)
	for _, k := range bk {
		fmt.Printf("%3d~%3d 分钟: %4d 笔  盈亏 %7.2fU\n", k, k+14, buckets[k][0], float64(buckets[k][1])/100)
	}

	// ===== 时段分布 =====
	fmt.Printf("\n===== 按小时盈亏分布（UTC+8 本地）=====\n")
	hourly := map[int]*[2]int{}
	for _, t := range trades {
		h := time.UnixMilli(t.OpenedAt).In(time.FixedZone("CST", 8*3600)).Hour()
		if hourly[h] == nil {
			hourly[h] = &[2]int{}
		}
		hourly[h][0]++
		hourly[h][1] += int(t.Pnl * 100)
	}
	for h := 0; h < 24; h++ {
		if hourly[h] != nil {
			fmt.Printf("%02d:00 时: %4d 笔  盈亏 %7.2fU\n", h, hourly[h][0], float64(hourly[h][1])/100)
		}
	}

	// ===== 追涨 vs 回踩 =====
	fmt.Printf("\n===== 追涨 vs 回踩（按连续入场价方向分类）=====\n")
	byType := map[string]*[2]int{}
	for _, t := range trades {
		if byType[t.ChaseType] == nil {
			byType[t.ChaseType] = &[2]int{}
		}
		byType[t.ChaseType][0]++
		byType[t.ChaseType][1] += int(t.Pnl * 100)
	}
	for _, k := range []string{"first", "chase", "pullback", "flat"} {
		if byType[k] != nil {
			fmt.Printf("%-8s: %4d 笔  盈亏 %7.2fU  平均 %.3fU/笔\n", k, byType[k][0], float64(byType[k][1])/100, float64(byType[k][1])/100/float64(byType[k][0]))
		}
	}

	// 追涨/回踩 每币明细（CSV）
	f3, _ := os.Create(filepath.Join(outDir, "chase_vs_pullback.csv"))
	w3 := csv.NewWriter(f3)
	w3.Write([]string{"symbol", "chase_count", "chase_pnl", "pullback_count", "pullback_pnl"})
	for _, c := range coinList {
		var cc, cp, pc, pp int
		for _, t := range trades {
			if t.Symbol != c.symbol {
				continue
			}
			if t.ChaseType == "chase" {
				cc++
				cp += int(t.Pnl * 100)
			} else if t.ChaseType == "pullback" {
				pc++
				pp += int(t.Pnl * 100)
			}
		}
		if cc+pc > 0 {
			w3.Write([]string{c.symbol, strconv.Itoa(cc), strconv.FormatFloat(float64(cp)/100, 'f', 2, 64),
				strconv.Itoa(pc), strconv.FormatFloat(float64(pp)/100, 'f', 2, 64)})
		}
	}
	w3.Flush()
	f3.Close()

	// ===== 逐笔解剖: 亏损王 + 盈利王完整序列 =====
	var dissectCoins []string
	// 亏损王（按 pnl 升序取前 8）
	sort.Slice(coinList, func(i, j int) bool { return coinList[i].pnl < coinList[j].pnl })
	for i := 0; i < 8 && i < len(coinList); i++ {
		dissectCoins = append(dissectCoins, coinList[i].symbol)
	}
	// 盈利王（按 pnl 降序取前 8）
	sort.Slice(coinList, func(i, j int) bool { return coinList[i].pnl > coinList[j].pnl })
	for i := 0; i < 8 && i < len(coinList); i++ {
		if !containsStr(dissectCoins, coinList[i].symbol) {
			dissectCoins = append(dissectCoins, coinList[i].symbol)
		}
	}

	var sb strings.Builder
	sb.WriteString("# 实盘逐笔解剖（亏损王 + 盈利王）\n\n")
	for _, sym := range dissectCoins {
		sb.WriteString(fmt.Sprintf("## %s\n\n", sym))
		sb.WriteString("| 时间 | 入场 | 出场 | 盈亏 | 盈亏% | 持仓分 | 原因 | 类型 |\n|---|---|---|---|---|---|---|---|\n")
		for _, t := range trades {
			if t.Symbol != sym {
				continue
			}
			sb.WriteString(fmt.Sprintf("| %s | %.6f | %.6f | %.2f | %.2f%% | %d | %s | %s |\n",
				time.UnixMilli(t.OpenedAt).Format("01-02 15:04"), t.EntryPx, t.ExitPx, t.Pnl, t.PnlPct, t.HeldMin, t.Reason, t.ChaseType))
		}
		sb.WriteString("\n")
	}
	os.WriteFile(filepath.Join(outDir, "逐笔解剖.md"), []byte(sb.String()), 0o644)
	fmt.Printf("\n逐笔解剖报告: %s\\逐笔解剖.md\n", outDir)

	fmt.Printf("\n输出文件: %s\\per_trade.csv / per_coin.csv / chase_vs_pullback.csv\n", outDir)
}

func containsStr(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func sumPnl(ts []*trade) float64 {
	s := 0.0
	for _, t := range ts {
		s += t.Pnl
	}
	return s
}

func sumFee(ts []*trade) float64 {
	s := 0.0
	for _, t := range ts {
		s += t.Fee
	}
	return s
}

func safePct(a, b float64) float64 {
	if b == 0 {
		return 0
	}
	return a / b * 100
}
