// Package storage 提供 SQLite 数据库初始化和表结构管理
package storage

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	_ "github.com/mattn/go-sqlite3"
)

// DB 封装数据库连接
type DB struct {
	Conn *sql.DB
}

// DBPathForMode 根据运行模式返回对应的数据库文件路径。
// 两种模式使用独立数据库文件，完全隔离数据：
//   - SIMULATION → data/quant_simulation.db
//   - LIVE       → data/quant_live.db
//
// 参数:
//   - baseDir: 数据目录（如 "data"）
//   - mode: 运行模式（SIMULATION / LIVE）
//
// 返回: 数据库文件完整路径
func DBPathForMode(baseDir, mode string) string {
	// 模式名转小写作为文件后缀
	suffix := strings.ToLower(mode)
	return filepath.Join(baseDir, fmt.Sprintf("quant_%s.db", suffix))
}

// NewDB 创建并初始化数据库连接
// dbPath: SQLite 文件路径，如 "data/quant.db"
// 返回: DB 实例或错误
func NewDB(dbPath string) (*DB, error) {
	// 确保目录存在
	dir := filepath.Dir(dbPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("创建数据目录失败: %w", err)
	}

	// WAL 模式下 synchronous=NORMAL 是官方推荐组合：
	// 提交时不立即 fsync（仅在 checkpoint 时同步），大幅降低批量写入的同步开销，
	// 同时仍保证崩溃一致性（最多丢失最后一次 checkpoint 后的事务，不损坏数据库）。
	conn, err := sql.Open("sqlite3", dbPath+"?_journal_mode=WAL&_busy_timeout=5000&_synchronous=NORMAL")
	if err != nil {
		return nil, fmt.Errorf("打开数据库失败: %w", err)
	}

	// 单连接（SQLite 写入限制）
	conn.SetMaxOpenConns(1)

	db := &DB{Conn: conn}
	if err := db.migrate(); err != nil {
		conn.Close()
		return nil, fmt.Errorf("数据库迁移失败: %w", err)
	}

	return db, nil
}

// migrate 执行建表 SQL
func (db *DB) migrate() error {
	schema := `
CREATE TABLE IF NOT EXISTS positions (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    symbol TEXT NOT NULL,
    side TEXT NOT NULL DEFAULT 'LONG',
    entry_price REAL NOT NULL,
    amount REAL NOT NULL,
    leverage INTEGER NOT NULL DEFAULT 10,
    highest_price REAL,
    trailing_active INTEGER NOT NULL DEFAULT 0,
    current_stop_price REAL NOT NULL DEFAULT 0,
    status TEXT NOT NULL DEFAULT 'OPEN',
    opened_at INTEGER NOT NULL,
    closed_at INTEGER,
    close_reason TEXT,
    realized_pnl REAL,
    exit_price REAL,
    fee REAL NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS trade_logs (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    timestamp INTEGER NOT NULL,
    level TEXT NOT NULL DEFAULT 'info',
    module TEXT NOT NULL DEFAULT '',
    message TEXT NOT NULL DEFAULT '',
    symbol TEXT DEFAULT '',
    price REAL DEFAULT 0,
    amount REAL DEFAULT 0,
    position_id INTEGER DEFAULT 0
);

CREATE TABLE IF NOT EXISTS strategy_config (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    key TEXT NOT NULL UNIQUE,
    value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS orders (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    position_id INTEGER NOT NULL,
    exchange_order_id INTEGER NOT NULL,
    algo_id INTEGER NOT NULL DEFAULT 0,
    symbol TEXT NOT NULL,
    order_type TEXT NOT NULL,
    side TEXT NOT NULL DEFAULT 'SELL',
    status TEXT NOT NULL DEFAULT 'NEW',
    stop_price REAL,
    activation_price REAL,
    callback_rate REAL,
    amount REAL NOT NULL,
    filled_price REAL,
    filled_amount REAL,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    FOREIGN KEY (position_id) REFERENCES positions(id)
);

CREATE TABLE IF NOT EXISTS order_events (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    order_id INTEGER NOT NULL,
    exchange_order_id INTEGER NOT NULL,
    event_type TEXT NOT NULL,
    old_status TEXT,
    new_status TEXT,
    price REAL,
    message TEXT,
    timestamp INTEGER NOT NULL,
    FOREIGN KEY (order_id) REFERENCES orders(id)
);

CREATE INDEX IF NOT EXISTS idx_positions_status ON positions(status);
CREATE INDEX IF NOT EXISTS idx_positions_symbol ON positions(symbol);
CREATE INDEX IF NOT EXISTS idx_trade_logs_timestamp ON trade_logs(timestamp);
CREATE INDEX IF NOT EXISTS idx_orders_position ON orders(position_id);
CREATE INDEX IF NOT EXISTS idx_orders_status ON orders(status);
CREATE INDEX IF NOT EXISTS idx_orders_exchange ON orders(exchange_order_id);
CREATE INDEX IF NOT EXISTS idx_order_events_order ON order_events(order_id);
CREATE INDEX IF NOT EXISTS idx_order_events_time ON order_events(timestamp);

CREATE TABLE IF NOT EXISTS daily_summaries (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    mode TEXT NOT NULL DEFAULT 'SIMULATION',
    summary_date TEXT NOT NULL,
    summary_type TEXT NOT NULL DEFAULT 'daily',
    market_notes TEXT NOT NULL DEFAULT '',
    coin_analysis TEXT NOT NULL DEFAULT '',
    suggestions TEXT NOT NULL DEFAULT '',
    today_pnl REAL NOT NULL DEFAULT 0,
    win_rate REAL NOT NULL DEFAULT 0,
    trade_count INTEGER NOT NULL DEFAULT 0,
    rating INTEGER NOT NULL DEFAULT 0,
    feature_json TEXT NOT NULL DEFAULT '{}',
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    deleted_at INTEGER NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_daily_summaries_mode_date_type
    ON daily_summaries(mode, summary_date, summary_type);
CREATE INDEX IF NOT EXISTS idx_daily_summaries_date ON daily_summaries(summary_date);

CREATE TABLE IF NOT EXISTS data_audit_log (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    mode TEXT NOT NULL,
    action TEXT NOT NULL,
    target TEXT NOT NULL DEFAULT '',
    detail TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_audit_mode_time ON data_audit_log(mode, created_at);
`
	_, err := db.Conn.Exec(schema)
	if err != nil {
		return err
	}
	// 存量数据库补齐新增列（CREATE TABLE IF NOT EXISTS 不会修改已存在的表）
	if err := db.migratePositionsColumns(); err != nil {
		return err
	}
	if err := db.migrateTradeLogsColumns(); err != nil {
		return err
	}
	return db.migrateOrdersColumns()
}

// migratePositionsColumns 为已存在的 positions 表补齐 exit_price / fee 列
// 通过 PRAGMA table_info 检测列是否存在，缺失时执行 ALTER TABLE ADD COLUMN，
// 保证新旧数据库结构一致（幂等：已有列时跳过）
// 返回: 迁移失败时返回错误
func (db *DB) migratePositionsColumns() error {
	cols, err := db.tableColumns("positions")
	if err != nil {
		return err
	}
	if !cols["exit_price"] {
		if _, err := db.Conn.Exec(`ALTER TABLE positions ADD COLUMN exit_price REAL`); err != nil {
			return fmt.Errorf("补充 exit_price 列失败: %w", err)
		}
	}
	if !cols["fee"] {
		if _, err := db.Conn.Exec(`ALTER TABLE positions ADD COLUMN fee REAL NOT NULL DEFAULT 0`); err != nil {
			return fmt.Errorf("补充 fee 列失败: %w", err)
		}
	}
	return nil
}

// migrateTradeLogsColumns 为已存在的 trade_logs 表补齐 position_id 列
// 通过 PRAGMA table_info 检测列是否存在，缺失时执行 ALTER TABLE ADD COLUMN，
// 保证新旧数据库结构一致（幂等：已有列时跳过）
// 返回: 迁移失败时返回错误
func (db *DB) migrateTradeLogsColumns() error {
	cols, err := db.tableColumns("trade_logs")
	if err != nil {
		return err
	}
	if !cols["position_id"] {
		if _, err := db.Conn.Exec(`ALTER TABLE trade_logs ADD COLUMN position_id INTEGER DEFAULT 0`); err != nil {
			return fmt.Errorf("补充 position_id 列失败: %w", err)
		}
	}
	return nil
}

// migrateOrdersColumns 为已存在的 orders 表补齐 algo_id 列
// Algo Order API 迁移后，条件单使用 algoId 标识（区别于旧接口的 orderId）
// 通过 PRAGMA table_info 检测列是否存在，缺失时执行 ALTER TABLE ADD COLUMN，
// 保证新旧数据库结构一致（幂等：已有列时跳过）
// 返回: 迁移失败时返回错误
func (db *DB) migrateOrdersColumns() error {
	cols, err := db.tableColumns("orders")
	if err != nil {
		return err
	}
	if !cols["algo_id"] {
		if _, err := db.Conn.Exec(`ALTER TABLE orders ADD COLUMN algo_id INTEGER NOT NULL DEFAULT 0`); err != nil {
			return fmt.Errorf("补充 algo_id 列失败: %w", err)
		}
		log.Printf("[DB] orders 表新增 algo_id 列")
	}
	return nil
}

// tableColumns 查询指定表的列名集合
// 参数:
//   - table: 表名
//
// 返回:
//   - map[string]bool: 列名 -> 是否存在
//   - error: 查询失败时返回错误
func (db *DB) tableColumns(table string) (map[string]bool, error) {
	rows, err := db.Conn.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	cols := make(map[string]bool)
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull int
		var dfltValue interface{}
		var pk int
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dfltValue, &pk); err != nil {
			return nil, err
		}
		cols[name] = true
	}
	return cols, nil
}

// Close 关闭数据库连接
func (db *DB) Close() error {
	if db.Conn != nil {
		return db.Conn.Close()
	}
	return nil
}
