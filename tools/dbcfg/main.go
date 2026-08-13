// dbcfg 只读诊断工具: 输出 SQLite 中 strategy_config 表的全部键值（核对 A/B/D 实盘参数）。
// 用法: go run ./tools/dbcfg <db路径>
package main

import (
	"database/sql"
	"fmt"
	"os"

	_ "github.com/mattn/go-sqlite3"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Println("用法: dbcfg <db路径>")
		os.Exit(1)
	}
	db, err := sql.Open("sqlite3", "file:"+os.Args[1]+"?mode=ro&_journal_mode=WAL&_busy_timeout=3000")
	if err != nil {
		fmt.Println("打开失败:", err)
		os.Exit(1)
	}
	defer db.Close()
	rows, err := db.Query("SELECT key, value FROM strategy_config ORDER BY key")
	if err != nil {
		fmt.Println("查询失败:", err)
		os.Exit(1)
	}
	defer rows.Close()
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			fmt.Println("读取失败:", err)
			os.Exit(1)
		}
		fmt.Printf("%s = %s\n", k, v)
	}
}
