// dbdump 临时诊断工具: 只读打开 SQLite，导出交易日志/持仓/委托摘要。
// 用法: go run ./tools/dbdump <db路径>
package main

import (
	"database/sql"
	"fmt"
	"os"

	_ "github.com/mattn/go-sqlite3"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Println("用法: dbdump <db路径>")
		os.Exit(1)
	}
	db, err := sql.Open("sqlite3", "file:"+os.Args[1]+"?mode=ro&_journal_mode=WAL")
	if err != nil {
		fmt.Println("打开失败:", err)
		os.Exit(1)
	}
	defer db.Close()

	fmt.Println("=== 数据表 ===")
	rows, _ := db.Query("SELECT name FROM sqlite_master WHERE type='table' ORDER BY name")
	for rows.Next() {
		var t string
		rows.Scan(&t)
		fmt.Println(" ", t)
	}
	rows.Close()

	// 交易日志最近 100 条
	if hasTable(db, "trade_logs") {
		fmt.Println("\n=== trade_logs 最近 100 条 ===")
		r, err := db.Query(`SELECT datetime(timestamp/1000,'unixepoch','localtime'), level, module, ifnull(symbol,''), substr(message,1,150)
			FROM trade_logs ORDER BY timestamp DESC LIMIT 100`)
		if err != nil {
			fmt.Println("查询 trade_logs 失败:", err)
		} else {
			for r.Next() {
				var ts, lv, mod, sym, msg string
				r.Scan(&ts, &lv, &mod, &sym, &msg)
				fmt.Printf("[%s] %s/%s %s %s\n", ts, lv, mod, sym, msg)
			}
			r.Close()
		}
	}

	// 持仓按天/状态汇总
	if hasTable(db, "positions") {
		fmt.Println("\n=== positions 按天/状态汇总（近 30 天） ===")
		cols := tableCols(db, "positions")
		hasPnl := contains(cols, "realized_pnl")
		pnlExpr := "'-'"
		if hasPnl {
			pnlExpr = "round(sum(realized_pnl),2)"
		}
		q := fmt.Sprintf(`SELECT date(opened_at/1000,'unixepoch','localtime'), status, count(*), %s FROM positions
			GROUP BY 1,2 ORDER BY 1 DESC LIMIT 30`, pnlExpr)
		r, err := db.Query(q)
		if err != nil {
			fmt.Println("查询 positions 失败:", err)
		} else {
			for r.Next() {
				var d, st string
				var n int
				var pnl sql.NullString
				r.Scan(&d, &st, &n, &pnl)
				fmt.Printf("%s %-8s %4d 笔 盈亏=%s\n", d, st, n, pnl.String)
			}
			r.Close()
		}
	}

	// 最近平仓明细 30 条
	if hasTable(db, "positions") {
		cols := tableCols(db, "positions")
		if contains(cols, "realized_pnl") && contains(cols, "close_reason") {
			fmt.Println("\n=== positions 最近平仓 30 笔 ===")
			r, err := db.Query(`SELECT datetime(opened_at/1000,'unixepoch','localtime'), datetime(closed_at/1000,'unixepoch','localtime'),
				symbol, side, round(entry_price,6), round(exit_price,6), round(realized_pnl,2), ifnull(close_reason,'')
				FROM positions WHERE status='CLOSED' ORDER BY closed_at DESC LIMIT 30`)
			if err != nil {
				fmt.Println("查询平仓明细失败:", err)
			} else {
				for r.Next() {
					var ot, ct, sym, side, reason string
					var ep, xp, pnl float64
					r.Scan(&ot, &ct, &sym, &side, &ep, &xp, &pnl, &reason)
					fmt.Printf("%s -> %s %-12s %-4s 入=%v 出=%v 盈亏=%.2f %s\n", ot, ct, sym, side, ep, xp, pnl, reason)
				}
				r.Close()
			}
		}
	}

	// 未平仓持仓
	if hasTable(db, "positions") {
		r, err := db.Query(`SELECT symbol, side, round(entry_price,6), round(amount,6), status,
			datetime(opened_at/1000,'unixepoch','localtime') FROM positions WHERE status='OPEN' ORDER BY opened_at DESC LIMIT 30`)
		if err == nil {
			n := 0
			for r.Next() {
				n++
				var sym, side, st, ot string
				var ep, amt float64
				r.Scan(&sym, &side, &ep, &amt, &st, &ot)
				fmt.Printf("OPEN %s %s 入=%v 量=%v 开仓=%s\n", sym, side, ep, amt, ot)
			}
			r.Close()
			if n == 0 {
				fmt.Println("（无 OPEN 持仓）")
			}
		}
	}

	// 策略配置
	if hasTable(db, "strategy_config") {
		fmt.Println("\n=== strategy_config ===")
		cols := tableCols(db, "strategy_config")
		_ = cols
		r, _ := db.Query(`SELECT key, value FROM strategy_config WHERE value LIKE '%scanIntervalSec%' OR value LIKE '%minQuoteVolume%' OR value LIKE '%enableNewListingFilter%' LIMIT 5`)
		names, _ := r.Columns()
		vals := make([]any, len(names))
		ptrs := make([]any, len(names))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		found := false
		for r.Next() {
			found = true
			r.Scan(ptrs...)
			for i, n := range names {
				fmt.Printf("  %s = %.1000v\n", n, vals[i])
			}
			fmt.Println("  ---")
		}
		r.Close()
		if !found {
			fmt.Println("  （未找到策略 JSON 配置行）")
		}
		// 全部键名
		r2, _ := db.Query("SELECT key FROM strategy_config")
		fmt.Println("  全部键: ")
		for r2.Next() {
			var k string
			r2.Scan(&k)
			fmt.Print(" ", k)
		}
		fmt.Println()
		r2.Close()
	}

	// 委托类型统计
	if hasTable(db, "orders") {
		fmt.Println("\n=== orders 表结构 ===")
		for _, c := range tableCols(db, "orders") {
			fmt.Print(c, " ")
		}
		fmt.Println()
		fmt.Println("=== orders 统计 ===")
		r, err := db.Query(`SELECT ifnull(order_type,'?'), ifnull(status,'?'), count(*) FROM orders GROUP BY 1,2 ORDER BY 3 DESC LIMIT 20`)
		if err != nil {
			fmt.Println("查询 orders 失败(尝试列名 order_type):", err)
		} else {
			for r.Next() {
				var typ, st string
				var n int
				r.Scan(&typ, &st, &n)
				fmt.Printf("  %-22s %-16s %d\n", typ, st, n)
			}
			r.Close()
		}
	}

	// 单币反复开仓异常（冷却期是否生效）
	if hasTable(db, "positions") {
		fmt.Println("\n=== 08-06 各币开仓次数 TOP 15（检查冷却异常） ===")
		r, err := db.Query(`SELECT symbol, count(*) c, count(DISTINCT date(opened_at/1000,'unixepoch')) d
			FROM positions WHERE status='CLOSED' AND opened_at >= strftime('%s','2026-08-06')*1000 GROUP BY symbol ORDER BY c DESC LIMIT 15`)
		if err != nil {
			fmt.Println("查询失败:", err)
		} else {
			for r.Next() {
				var sym string
				var c, d int
				r.Scan(&sym, &c, &d)
				fmt.Printf("  %-12s %d 笔（%d 天）\n", sym, c, d)
			}
			r.Close()
		}
	}

	// 按天/平仓原因分布 + 手续费（实盘诊断）
	if hasTable(db, "positions") {
		cols := tableCols(db, "positions")
		if contains(cols, "fee") && contains(cols, "close_reason") {
			fmt.Println("\n=== 按天/平仓原因 盈亏+手续费 ===")
			r, err := db.Query(`SELECT date(closed_at/1000,'unixepoch','localtime'), ifnull(close_reason,'?'),
				count(*), round(sum(realized_pnl),2), round(sum(fee),4)
				FROM positions WHERE status='CLOSED' GROUP BY 1,2 ORDER BY 1,2`)
			if err != nil {
				fmt.Println("查询失败:", err)
			} else {
				for r.Next() {
					var d, reason string
					var n int
					var pnl, fee float64
					r.Scan(&d, &reason, &n, &pnl, &fee)
					fmt.Printf("%s %-14s %4d 笔 盈亏=%8.2f 手续费=%8.4f\n", d, reason, n, pnl, fee)
				}
				r.Close()
			}
		}
	}

	// 08-06 / 08-07 单币开仓次数 TOP（实盘冷却验证）
	if hasTable(db, "positions") {
		for _, day := range []string{"2026-08-06", "2026-08-07"} {
			fmt.Printf("\n=== %s 单币开仓次数 TOP 10 ===\n", day)
			r, err := db.Query(`SELECT symbol, count(*) FROM positions
				WHERE status='CLOSED' AND date(opened_at/1000,'unixepoch','localtime')=? GROUP BY symbol ORDER BY 2 DESC LIMIT 10`, day)
			if err != nil {
				fmt.Println("查询失败:", err)
			} else {
				for r.Next() {
					var sym string
					var c int
					r.Scan(&sym, &c)
					fmt.Printf("  %-12s %d 笔\n", sym, c)
				}
				r.Close()
			}
		}
	}
}

func hasTable(db *sql.DB, name string) bool {
	var x string
	err := db.QueryRow("SELECT name FROM sqlite_master WHERE type='table' AND name=?", name).Scan(&x)
	return err == nil
}

func tableCols(db *sql.DB, table string) []string {
	rows, _ := db.Query("PRAGMA table_info(" + table + ")")
	var out []string
	for rows.Next() {
		var cid int
		var name, typ string
		var notnull, pk int
		var dflt sql.NullString
		rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk)
		out = append(out, name)
	}
	rows.Close()
	return out
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
