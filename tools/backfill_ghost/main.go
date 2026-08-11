// backfill_ghost 幽灵单盈亏回填工具。
//
// 背景：程序离线期间交易所条件单/强平把仓位平掉，本地重启后只看到
// "交易所无此持仓"，记录为 GHOST、盈亏记 0（如 B 策略凌晨 GUA×4/HOME×3/CYS/PROM，
// 真实亏损约 50U 未进账，导致本地统计与账户权益对不上）。
//
// 原理：从交易所拉 REALIZED_PNL 与 COMMISSION 资金流水，按币种对账——
//
//	缺口 = 交易所已实现 - 本地非幽灵已实现
//	手续费缺口 = 交易所佣金成本 - 本地手续费
//
// 将缺口按数量占比回填到该币种的 GHOST 持仓。
//
// 用法:
//
//	go run ./tools/backfill_ghost --db <quant_simulation.db> --days 15 --dry   # 预览
//	go run ./tools/backfill_ghost --db <quant_simulation.db> --days 15        # 写库
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"time"

	"quant-desktop/internal/binance"
	"quant-desktop/internal/storage"
)

type closedRow struct {
	ID     int64
	Symbol string
	Reason string
	Pnl    float64
	Fee    float64
	Amount float64
	Closed int64
}

func main() {
	dbPath := flag.String("db", "", "模拟盘数据库路径（quant_simulation.db）")
	days := flag.Int("days", 15, "对账窗口天数（建议覆盖最早幽灵单之前）")
	dry := flag.Bool("dry", false, "只打印不写库")
	incomeOnly := flag.Bool("income-only", false, "只打印账户资金流水分类汇总（含转入/资金费，不写库）")
	flag.Parse()
	if *dbPath == "" {
		log.Fatal("请指定 --db <数据库路径>")
	}

	db, err := storage.NewDB(*dbPath)
	if err != nil {
		log.Fatalf("打开数据库失败: %v", err)
	}
	defer db.Close()

	apiKey, apiSecret, err := db.LoadCredentials("SIMULATION")
	if err != nil {
		log.Fatalf("加载凭据失败: %v", err)
	}
	proxyAddr, proxyPort, _ := db.LoadProxyConfig()
	client := binance.NewClient(apiKey, apiSecret, "SIMULATION", proxyAddr, proxyPort)
	ctx := context.Background()
	if err := client.SyncServerTime(ctx); err != nil {
		log.Printf("⚠ 服务器时间同步失败: %v", err)
	}

	end := time.Now().UnixMilli()
	start := time.Now().AddDate(0, 0, -*days).UnixMilli()

	if *incomeOnly {
		bal, err := client.GetFuturesBalance(ctx)
		if err != nil {
			log.Fatalf("查询余额失败: %v", err)
		}
		list, err := fetchAllIncome(client, ctx, start, end)
		if err != nil {
			log.Fatalf("拉取资金流水失败: %v", err)
		}
		byType := map[string]float64{}
		byCnt := map[string]int{}
		total := 0.0
		for _, h := range list {
			byType[h.typ] += h.val
			byCnt[h.typ]++
			total += h.val
		}
		fmt.Printf("当前钱包余额: %.2fU\n", bal.TotalWalletBalance)
		for t, v := range byType {
			fmt.Printf("  %-16s %4d 条 %+10.4fU\n", t, byCnt[t], v)
		}
		fmt.Printf("全部收入合计: %+.2fU\n", total)
		fmt.Printf("推算初始余额 = 当前钱包 - 全部收入 = %.2fU\n", bal.TotalWalletBalance-total)
		return
	}

	realized, err := fetchIncome(client, ctx, "REALIZED_PNL", start, end)
	if err != nil {
		log.Fatalf("拉取已实现盈亏流水失败: %v", err)
	}
	commissions, err := fetchIncome(client, ctx, "COMMISSION", start, end)
	if err != nil {
		log.Fatalf("拉取手续费流水失败: %v", err)
	}
	fmt.Printf("交易所流水: REALIZED_PNL %d 条 / COMMISSION %d 条（窗口 %d 天）\n",
		len(realized), len(commissions), *days)

	rows, err := db.Conn.Query(
		`SELECT id, symbol, close_reason, COALESCE(realized_pnl,0), COALESCE(fee,0), amount, closed_at
		 FROM positions WHERE status='CLOSED'`)
	if err != nil {
		log.Fatalf("查询本地平仓失败: %v", err)
	}
	var all []closedRow
	for rows.Next() {
		var r closedRow
		if err := rows.Scan(&r.ID, &r.Symbol, &r.Reason, &r.Pnl, &r.Fee, &r.Amount, &r.Closed); err != nil {
			log.Fatalf("读取平仓记录失败: %v", err)
		}
		all = append(all, r)
	}
	rows.Close()

	exRealized := sumBySymbol(realized)
	exCommission := sumBySymbol(commissions) // 负值
	localRealized := map[string]float64{}
	localFee := map[string]float64{}
	ghosts := map[string][]closedRow{}
	for _, r := range all {
		localFee[r.Symbol] += r.Fee
		if r.Reason != "GHOST" {
			localRealized[r.Symbol] += r.Pnl
		} else {
			ghosts[r.Symbol] = append(ghosts[r.Symbol], r)
		}
	}

	var totBackR, totBackF float64
	symbols := map[string]bool{}
	for _, r := range all {
		symbols[r.Symbol] = true
	}
	for sym := range symbols {
		exR := exRealized[sym]
		exF := -exCommission[sym] // 佣金成本为正
		deltaR := exR - localRealized[sym]
		deltaF := exF - localFee[sym]
		gs := ghosts[sym]
		if len(gs) == 0 {
			if abs(deltaR) > 0.05 || abs(deltaF) > 0.05 {
				fmt.Printf("⚠ %s 无幽灵单但存在差异：交易所已实现 %+.2f vs 本地 %+.2f（差 %+.2f），未回填\n",
					sym, exR, localRealized[sym], deltaR)
			}
			continue
		}
		totalAmt := 0.0
		for _, g := range gs {
			totalAmt += g.Amount
		}
		fmt.Printf("— %s 幽灵单 %d 笔：交易所已实现 %+.2f / 本地 %+.2f → 回填盈亏 %+.2f，回填手续费 %+.2f\n",
			sym, len(gs), exR, localRealized[sym], deltaR, deltaF)
		allocR := 0.0
		for i, g := range gs {
			shareR := deltaR * g.Amount / totalAmt
			shareF := deltaF * g.Amount / totalAmt
			if i == len(gs)-1 { // 最后一条吸收取整误差
				shareR = deltaR - allocR
			}
			allocR += shareR
			newPnl := round2(shareR)
			newFee := round2(shareF)
			fmt.Printf("   #%d %s 回填 realized_pnl=%+.2f fee=%.4f\n", g.ID, g.Symbol, newPnl, newFee)
			totBackR += newPnl
			totBackF += newFee
			if !*dry {
				if _, err := db.Conn.Exec(
					`UPDATE positions SET realized_pnl=?, fee=? WHERE id=? AND status='CLOSED'`,
					newPnl, newFee, g.ID); err != nil {
					log.Printf("更新 #%d 失败: %v", g.ID, err)
				}
			}
		}
	}
	fmt.Printf("\n合计回填：盈亏 %+.2fU，手续费 %+.2fU（%s）\n", totBackR, totBackF,
		map[bool]string{true: "仅预览，未写库", false: "已写库"}[*dry])
}

// incomeRow 资金流水条目（仅保留诊断所需字段）
type incomeRow struct {
	typ string
	val float64
}

// fetchAllIncome 拉取全部类型资金流水（分页），供 --income-only 诊断账户收支全貌。
func fetchAllIncome(c *binance.Client, ctx context.Context, start, end int64) ([]incomeRow, error) {
	var out []incomeRow
	cur := start
	for cur < end {
		list, err := c.GetIncomeHistory(ctx, "", cur, end)
		if err != nil {
			return nil, err
		}
		if len(list) == 0 {
			break
		}
		for _, h := range list {
			out = append(out, incomeRow{typ: h.IncomeType, val: parseF(h.Income)})
		}
		last := list[len(list)-1].Time
		if last <= cur || len(list) < 1000 {
			break
		}
		cur = last + 1
	}
	return out, nil
}

// fetchIncome 拉取指定类型流水并分页（每页 1000，按时间翻页），按币种汇总。
func fetchIncome(c *binance.Client, ctx context.Context, incomeType string, start, end int64) (map[string]float64, error) {
	out := map[string]float64{}
	cur := start
	for cur < end {
		list, err := c.GetIncomeHistory(ctx, incomeType, cur, end)
		if err != nil {
			return nil, err
		}
		if len(list) == 0 {
			break
		}
		for _, h := range list {
			out[h.Symbol] += parseF(h.Income)
		}
		last := list[len(list)-1].Time
		if last <= cur || len(list) < 1000 {
			break
		}
		cur = last + 1
	}
	return out, nil
}

func sumBySymbol(m map[string]float64) map[string]float64 { return m }

func parseF(s string) float64 {
	var v float64
	fmt.Sscanf(s, "%f", &v)
	return v
}

func abs(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

func round2(v float64) float64 {
	return float64(int64(v*100+0.5*float64(sign(v)))) / 100
}

func sign(v float64) int {
	if v < 0 {
		return -1
	}
	return 1
}

var _ = storage.DB{}
