// market_metrics 每日市场策略环境指标采集 + 周/月/年总结生成。
//
// 用途：把"今天市场给没给肉、肉好不好吃"量化成指标，与策略每日盈亏放一起归因，
// 回答"回测好实盘差"是策略问题还是市场环境问题。
//
// 采集（collect）：对 24h 成交额 >= 2000 万 USDT 的合约池拉 15m/5m K 线，
// 按北京时间自然日聚合出策略环境指标，写入 market_data.db 的 market_daily_metrics 表。
//
// 报告（report）：读 market_daily_metrics + 客户端库 daily_summaries(auto)，
// 生成 daily/weekly/monthly/yearly 大白话总结（表格化文本）写回 daily_summaries。
//
// 用法:
//   go run ./tools/market_metrics collect
//   go run ./tools/market_metrics report --mode SIMULATION --type daily --db D:\...\quant_simulation.db
//   go run ./tools/market_metrics report --mode SIMULATION --type weekly --db ...
// 代理：默认读 HTTPS_PROXY 环境变量，可用 --proxy http://127.0.0.1:10808 覆盖。
package main

import (
	"context"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

const (
	marketDBPath = `D:\0001_ba-A - 03\market_data\market_data.db`
	minPoolVol   = 20_000_000.0 // 机会池：24h 成交额下限（与 A/D 策略一致）
	fapiBase     = "https://fapi.binance.com"
)

var beijing = time.FixedZone("CST", 8*3600)

// ==================== K 线与行情 ====================

type kline struct {
	openTime int64
	open     float64
	high     float64
	low      float64
	close    float64
}

func httpClient(proxy string) *http.Client {
	tr := &http.Transport{}
	if proxy != "" {
		if u, err := url.Parse(proxy); err == nil {
			tr.Proxy = http.ProxyURL(u)
		}
	}
	return &http.Client{Transport: tr, Timeout: 30 * time.Second}
}

func getJSON(client *http.Client, path string, out interface{}) error {
	return getURL(client, fapiBase+path, out)
}

func getURL(client *http.Client, fullURL string, out interface{}) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fullURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "quant-market-metrics/1.0")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// fetchKlines 拉取 K 线（公开端点，无签名），返回按时间升序的 K 线切片。
func fetchKlines(client *http.Client, symbol, interval string, limit int) ([]kline, error) {
	var raw [][]json.RawMessage
	path := fmt.Sprintf("/fapi/v1/klines?symbol=%s&interval=%s&limit=%d",
		url.QueryEscape(symbol), interval, limit)
	if err := getJSON(client, path, &raw); err != nil {
		return nil, err
	}
	out := make([]kline, 0, len(raw))
	for _, row := range raw {
		if len(row) < 5 {
			continue
		}
		var k kline
		if err := json.Unmarshal(row[0], &k.openTime); err != nil {
			continue
		}
		var s string
		for i, dst := range []*float64{&k.open, &k.high, &k.low, &k.close} {
			if err := json.Unmarshal(row[i+1], &s); err != nil {
				continue
			}
			if v, err := strconv.ParseFloat(s, 64); err == nil {
				*dst = v
			}
		}
		out = append(out, k)
	}
	return out, nil
}

type ticker24 struct {
	Symbol      string  `json:"symbol"`
	LastPrice   float64 `json:"lastPrice"`
	PriceChange float64 `json:"priceChangePercent"`
	QuoteVolume float64 `json:"quoteVolume"`
}

// UnmarshalJSON 兼容币安 ticker 接口的字符串数字字段。
func (t *ticker24) UnmarshalJSON(data []byte) error {
	var raw struct {
		Symbol      string          `json:"symbol"`
		LastPrice   json.RawMessage `json:"lastPrice"`
		PriceChange json.RawMessage `json:"priceChangePercent"`
		QuoteVolume json.RawMessage `json:"quoteVolume"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	t.Symbol = raw.Symbol
	t.LastPrice = numField(raw.LastPrice)
	t.PriceChange = numField(raw.PriceChange)
	t.QuoteVolume = numField(raw.QuoteVolume)
	return nil
}

func numField(raw json.RawMessage) float64 {
	if len(raw) == 0 {
		return 0
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		v, _ := strconv.ParseFloat(s, 64)
		return v
	}
	var f float64
	_ = json.Unmarshal(raw, &f)
	return f
}

func fetchTickers(client *http.Client) ([]ticker24, error) {
	var raw []ticker24
	if err := getJSON(client, "/fapi/v1/ticker/24hr", &raw); err != nil {
		return nil, err
	}
	out := raw[:0]
	for _, t := range raw {
		if strings.HasSuffix(t.Symbol, "USDT") && t.QuoteVolume > 0 {
			out = append(out, t)
		}
	}
	return out, nil
}

// ==================== 指标采集 ====================

type dailyMetrics struct {
	Date               string
	PoolWidth          int
	OpportunityCount   int
	OpportunityTotal   int
	BurstCoinCount     int
	BurstTotal         int
	FakeBreakoutCount  int
	FakeBreakoutRate   float64
	Max15mUp           float64
	Max15mDown         float64
	BigDropCount       int
	BTCATRPct          float64
	TotalQuoteVolume   float64
	UpCount            int
	DownCount          int
	FakeBreakoutDenom  int
	FNG                int     // 恐惧贪婪指数 0-100（0=未采到）
	BTCChg24h          float64 // BTC 24h 涨跌幅 %
	ETHChg24h          float64 // ETH 24h 涨跌幅 %
	BTCFundingRate     float64 // BTC 当前资金费率 %
}

// beijingDayStartUTC 北京时间某自然日 00:00 对应的 UTC 毫秒。
func beijingDayStartUTC(day time.Time) int64 {
	bd := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, beijing)
	return bd.UnixMilli()
}

func atrPct(kl []kline) float64 {
	if len(kl) < 2 {
		return 0
	}
	var sum float64
	n := 0
	for i := 1; i < len(kl); i++ {
		tr := kl[i].high - kl[i].low
		if c := kl[i-1].close; c > 0 {
			if v := kl[i].high - c; v > tr {
				tr = v
			}
			if v := c - kl[i].low; v > tr {
				tr = v
			}
		}
		sum += tr
		n++
	}
	if n == 0 || kl[len(kl)-1].close <= 0 {
		return 0
	}
	return sum / float64(n) / kl[len(kl)-1].close * 100
}

func collect(proxy string) error {
	client := httpClient(proxy)
	tickers, err := fetchTickers(client)
	if err != nil {
		return fmt.Errorf("拉取行情失败: %w", err)
	}
	now := time.Now().In(beijing)
	dayStart := beijingDayStartUTC(now)
	day := now.Format("2006-01-02")

	pool := make([]ticker24, 0, len(tickers))
	for _, t := range tickers {
		if t.QuoteVolume >= minPoolVol {
			pool = append(pool, t)
		}
	}
	sort.Slice(pool, func(i, j int) bool { return pool[i].QuoteVolume > pool[j].QuoteVolume })

	m := dailyMetrics{
		Date:             day,
		PoolWidth:        len(pool),
		TotalQuoteVolume: 0,
	}
	for _, t := range tickers {
		m.TotalQuoteVolume += t.QuoteVolume
		switch t.Symbol {
		case "BTCUSDT":
			m.BTCChg24h = t.PriceChange
		case "ETHUSDT":
			m.ETHChg24h = t.PriceChange
		}
		if t.PriceChange > 0 {
			m.UpCount++
		} else if t.PriceChange < 0 {
			m.DownCount++
		}
		if t.PriceChange <= -10 {
			m.BigDropCount++
		}
	}

	// BTC 15m ATR（用 96 根 = 24h）
	if btc, err := fetchKlines(client, "BTCUSDT", "15m", 96); err == nil {
		m.BTCATRPct = atrPct(btc)
	}
	// 恐惧贪婪指数（Alternative.me 公开 API）
	var fng struct {
		Data []struct {
			Value string `json:"value"`
		} `json:"data"`
	}
	if err := getURL(client, "https://api.alternative.me/fng/?limit=1", &fng); err == nil && len(fng.Data) > 0 {
		if v, e := strconv.Atoi(fng.Data[0].Value); e == nil {
			m.FNG = v
		}
	} else if err != nil {
		log.Printf("⚠ FNG 采集失败: %v", err)
	}
	// BTC 资金费率（premiumIndex，公开端点）
	var prem struct {
		LastFundingRate string `json:"lastFundingRate"`
	}
	if err := getJSON(client, "/fapi/v1/premiumIndex?symbol=BTCUSDT", &prem); err == nil {
		if v, e := strconv.ParseFloat(prem.LastFundingRate, 64); e == nil {
			m.BTCFundingRate = v * 100
		}
	} else {
		log.Printf("⚠ BTC 费率采集失败: %v", err)
	}
	log.Printf("大盘: FNG=%d BTC24h=%.2f%% ETH24h=%.2f%% BTC费率=%.4f%%",
		m.FNG, m.BTCChg24h, m.ETHChg24h, m.BTCFundingRate)

	oppSet := map[string]bool{}
	burstSet := map[string]bool{}
	fakeSet := map[string]bool{}
	fakeDenom := map[string]bool{}
	for i, t := range pool {
		if i >= 160 {
			break // 池内最多处理 160 个币（限时保护）
		}
		k15, err15 := fetchKlines(client, t.Symbol, "15m", 96)
		if err15 != nil {
			continue
		}
		for _, k := range k15 {
			if k.openTime < dayStart || k.open <= 0 {
				continue
			}
			entity := (k.close - k.open) / k.open * 100
			if entity >= 3 {
				m.OpportunityTotal++
				oppSet[t.Symbol] = true
			}
			if entity > m.Max15mUp {
				m.Max15mUp = entity
			}
			if entity < m.Max15mDown {
				m.Max15mDown = entity
			}
			// 假突破：盘中触及 +3% 但收盘未站稳
			if k.high >= k.open*1.03 {
				fakeDenom[t.Symbol] = true
				if k.close < k.open*1.03 {
					fakeSet[t.Symbol] = true
				}
			}
		}
		k5, err5 := fetchKlines(client, t.Symbol, "5m", 288)
		if err5 != nil {
			continue
		}
		for j := 1; j < len(k5); j++ {
			k := k5[j]
			prev := k5[j-1].close
			if k.openTime < dayStart || k.close <= 0 || prev <= 0 {
				continue
			}
			if (k.close-prev)/prev*100 >= 2.5 {
				m.BurstTotal++
				burstSet[t.Symbol] = true
			}
		}
	}
	m.OpportunityCount = len(oppSet)
	m.BurstCoinCount = len(burstSet)
	m.FakeBreakoutCount = len(fakeSet)
	m.FakeBreakoutDenom = len(fakeDenom)
	if m.FakeBreakoutDenom > 0 {
		m.FakeBreakoutRate = float64(m.FakeBreakoutCount) / float64(m.FakeBreakoutDenom) * 100
	}

	if err := saveMetrics(m); err != nil {
		return err
	}
	log.Printf("✅ 已写入 %s 市场指标: 池=%d 机会=%d(%d次) 爆拉=%d(%d次) 假突破=%d/%d(%.1f%%) BTC_ATR=%.2f%% 最大15m涨=%.2f%% 跌=%.2f%%",
		day, m.PoolWidth, m.OpportunityCount, m.OpportunityTotal, m.BurstCoinCount, m.BurstTotal,
		m.FakeBreakoutCount, m.FakeBreakoutDenom, m.FakeBreakoutRate, m.BTCATRPct, m.Max15mUp, m.Max15mDown)
	return nil
}

func saveMetrics(m dailyMetrics) error {
	db, err := sql.Open("sqlite3", marketDBPath+"?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		return err
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS market_daily_metrics (
		date TEXT PRIMARY KEY,
		pool_width INTEGER NOT NULL DEFAULT 0,
		opportunity_count INTEGER NOT NULL DEFAULT 0,
		opportunity_total INTEGER NOT NULL DEFAULT 0,
		burst_coin_count INTEGER NOT NULL DEFAULT 0,
		burst_total INTEGER NOT NULL DEFAULT 0,
		fake_breakout_count INTEGER NOT NULL DEFAULT 0,
		fake_breakout_rate REAL NOT NULL DEFAULT 0,
		max_15m_up REAL NOT NULL DEFAULT 0,
		max_15m_down REAL NOT NULL DEFAULT 0,
		big_drop_count INTEGER NOT NULL DEFAULT 0,
		btc_atr_pct REAL NOT NULL DEFAULT 0,
		total_quote_volume REAL NOT NULL DEFAULT 0,
		up_count INTEGER NOT NULL DEFAULT 0,
		down_count INTEGER NOT NULL DEFAULT 0,
		fng INTEGER NOT NULL DEFAULT 0,
		btc_chg_24h REAL NOT NULL DEFAULT 0,
		eth_chg_24h REAL NOT NULL DEFAULT 0,
		btc_funding_rate REAL NOT NULL DEFAULT 0,
		updated_at INTEGER NOT NULL
	)`); err != nil {
		return err
	}
	// 旧表补列（SQLite 无 ADD COLUMN IF NOT EXISTS，先查列再补）
	for _, col := range []string{"fng", "btc_chg_24h", "eth_chg_24h", "btc_funding_rate"} {
		var n int
		db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('market_daily_metrics') WHERE name=?`, col).Scan(&n)
		if n == 0 {
			typ := "INTEGER NOT NULL DEFAULT 0"
			if col != "fng" {
				typ = "REAL NOT NULL DEFAULT 0"
			}
			if _, err := db.Exec(`ALTER TABLE market_daily_metrics ADD COLUMN ` + col + ` ` + typ); err != nil {
				return err
			}
		}
	}
	_, err = db.Exec(`INSERT INTO market_daily_metrics
		(date, pool_width, opportunity_count, opportunity_total, burst_coin_count, burst_total,
		 fake_breakout_count, fake_breakout_rate, max_15m_up, max_15m_down, big_drop_count,
		 btc_atr_pct, total_quote_volume, up_count, down_count, fng, btc_chg_24h, eth_chg_24h,
		 btc_funding_rate, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(date) DO UPDATE SET
		 pool_width=excluded.pool_width, opportunity_count=excluded.opportunity_count,
		 opportunity_total=excluded.opportunity_total, burst_coin_count=excluded.burst_coin_count,
		 burst_total=excluded.burst_total, fake_breakout_count=excluded.fake_breakout_count,
		 fake_breakout_rate=excluded.fake_breakout_rate, max_15m_up=excluded.max_15m_up,
		 max_15m_down=excluded.max_15m_down, big_drop_count=excluded.big_drop_count,
		 btc_atr_pct=excluded.btc_atr_pct, total_quote_volume=excluded.total_quote_volume,
		 up_count=excluded.up_count, down_count=excluded.down_count,
		 fng=excluded.fng, btc_chg_24h=excluded.btc_chg_24h, eth_chg_24h=excluded.eth_chg_24h,
		 btc_funding_rate=excluded.btc_funding_rate, updated_at=excluded.updated_at`,
		m.Date, m.PoolWidth, m.OpportunityCount, m.OpportunityTotal, m.BurstCoinCount, m.BurstTotal,
		m.FakeBreakoutCount, m.FakeBreakoutRate, m.Max15mUp, m.Max15mDown, m.BigDropCount,
		m.BTCATRPct, m.TotalQuoteVolume, m.UpCount, m.DownCount, m.FNG, m.BTCChg24h, m.ETHChg24h,
		m.BTCFundingRate, time.Now().UnixMilli())
	return err
}

// ==================== 报告 ====================

type metricRow struct {
	Date              string  `json:"date"`
	PoolWidth         int     `json:"poolWidth"`
	OpportunityCount  int     `json:"opportunityCount"`
	OpportunityTotal  int     `json:"opportunityTotal"`
	BurstCoinCount    int     `json:"burstCoinCount"`
	BurstTotal        int     `json:"burstTotal"`
	FakeBreakoutRate  float64 `json:"fakeBreakoutRate"`
	BTCATRPct         float64 `json:"btcATRPct"`
	Max15mUp          float64 `json:"max15mUp"`
	Max15mDown        float64 `json:"max15mDown"`
	FNG               int     `json:"fng"`
	BTCChg24h         float64 `json:"btcChg24h"`
	ETHChg24h         float64 `json:"ethChg24h"`
	BTCFundingRate    float64 `json:"btcFundingRate"`
}

type summaryRow struct {
	Date       string
	TodayPnl   float64
	TradeCount int
	WinRate    float64
}

// reportMeta 结构化写入 feature_json 的总结元数据（前端渲染真表格用）
type reportMeta struct {
	Metrics     []metricRow `json:"metrics"`
	Attribution string      `json:"attribution"`
}

func loadMetrics(from, to string) ([]metricRow, error) {
	db, err := sql.Open("sqlite3", "file:"+marketDBPath+"?mode=ro")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.Query(`SELECT date, pool_width, opportunity_count, opportunity_total,
		burst_coin_count, burst_total, fake_breakout_rate, btc_atr_pct, max_15m_up, max_15m_down,
		fng, btc_chg_24h, eth_chg_24h, btc_funding_rate
		FROM market_daily_metrics WHERE date BETWEEN ? AND ? ORDER BY date`, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []metricRow
	for rows.Next() {
		var r metricRow
		if err := rows.Scan(&r.Date, &r.PoolWidth, &r.OpportunityCount, &r.OpportunityTotal,
			&r.BurstCoinCount, &r.BurstTotal, &r.FakeBreakoutRate, &r.BTCATRPct, &r.Max15mUp, &r.Max15mDown,
			&r.FNG, &r.BTCChg24h, &r.ETHChg24h, &r.BTCFundingRate); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

func loadSummaries(mode, from, to string) ([]summaryRow, error) {
	db, err := sql.Open("sqlite3", "file:"+flagDB+"?mode=ro")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.Query(`SELECT a.summary_date, a.today_pnl, a.trade_count, a.win_rate
		FROM daily_summaries a
		JOIN (SELECT summary_date, MAX(id) mid FROM daily_summaries
		      WHERE mode=? AND summary_type='auto' AND summary_date BETWEEN ? AND ?
		      GROUP BY summary_date) b ON a.id=b.mid
		ORDER BY a.summary_date`, mode, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []summaryRow
	for rows.Next() {
		var r summaryRow
		if err := rows.Scan(&r.Date, &r.TodayPnl, &r.TradeCount, &r.WinRate); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

func fmtTable(head []string, rows [][]string) string {
	widths := make([]int, len(head))
	for i, h := range head {
		widths[i] = len(h)
	}
	for _, r := range rows {
		for i, c := range r {
			if len(c) > widths[i] {
				widths[i] = len(c)
			}
		}
	}
	line := func(cells []string) string {
		var sb strings.Builder
		for i, c := range cells {
			fmt.Fprintf(&sb, "| %-*s ", widths[i], c)
		}
		sb.WriteString("|")
		return sb.String()
	}
	sep := func() string {
		var sb strings.Builder
		for _, w := range widths {
			sb.WriteString("|" + strings.Repeat("-", w+2))
		}
		sb.WriteString("|")
		return sb.String()
	}
	var sb strings.Builder
	sb.WriteString(sep() + "\n" + line(head) + "\n" + sep() + "\n")
	for _, r := range rows {
		sb.WriteString(line(r) + "\n")
	}
	sb.WriteString(sep())
	return sb.String()
}

func genReport(mode, typ string) (string, float64, reportMeta, error) {
	now := time.Now().In(beijing)
	today := now.Format("2006-01-02")
	var from string
	switch typ {
	case "daily":
		from = today
	case "weekly":
		from = now.AddDate(0, 0, -6).Format("2006-01-02")
	case "monthly":
		from = now.AddDate(0, -1, 0).Format("2006-01-02")
	case "yearly":
		from = now.AddDate(-1, 0, 0).Format("2006-01-02")
	default:
		return "", 0, reportMeta{}, fmt.Errorf("类型仅支持 daily/weekly/monthly/yearly: %s", typ)
	}
	metrics, err := loadMetrics(from, today)
	if err != nil {
		return "", 0, reportMeta{}, err
	}
	sums, err := loadSummaries(mode, from, today)
	if err != nil {
		return "", 0, reportMeta{}, err
	}

	// 策略表现聚合
	var totalPnl float64
	var totalTrades int
	var wins int
	winDays, lossDays := 0, 0
	best, worst := 0.0, 0.0
	for _, s := range sums {
		totalPnl += s.TodayPnl
		totalTrades += s.TradeCount
		if s.TodayPnl > 0 {
			winDays++
			wins++
		} else if s.TodayPnl < 0 {
			lossDays++
		}
		if s.TodayPnl > best {
			best = s.TodayPnl
		}
		if s.TodayPnl < worst {
			worst = s.TodayPnl
		}
	}
	winRate := 0.0
	if len(sums) > 0 {
		winRate = float64(wins) / float64(len(sums)) * 100
	}

	// 市场聚合
	var oppSum, oppCnt, burstSum, poolSum int
	var fakeSum, atrSum float64
	for _, m := range metrics {
		oppSum += m.OpportunityCount
		oppCnt++
		burstSum += m.BurstTotal
		poolSum += m.PoolWidth
		fakeSum += m.FakeBreakoutRate
		atrSum += m.BTCATRPct
	}
	avg := func(v float64, n int) float64 {
		if n <= 0 {
			return 0
		}
		return v / float64(n)
	}

	var sb strings.Builder
	var attribution strings.Builder
	if typ == "daily" {
		sb.WriteString("## 市场环境（" + today + "）\n")
		if len(metrics) > 0 {
			m := metrics[len(metrics)-1]
			sb.WriteString(fmtTable([]string{"指标", "数值", "解读"}, [][]string{
				{"机会池宽度", fmt.Sprintf("%d 个币", m.PoolWidth), "24h成交额≥2000万 的合约数"},
				{"异动机会", fmt.Sprintf("%d 个币 / %d 次", m.OpportunityCount, m.OpportunityTotal), "15m收盘涨≥3%（收盘 vs 本周期开盘）的次数（策略的肉）"},
				{"5m爆拉", fmt.Sprintf("%d 次", m.BurstTotal), "5m收盘涨≥2.5%（收盘 vs 前一根收盘，智慧版1.5倍机会）"},
				{"假突破率", fmt.Sprintf("%.1f%%", m.FakeBreakoutRate), "冲3%未站稳占比（高=止损会多）"},
				{"最大15m涨/跌", fmt.Sprintf("+%.1f%% / %.1f%%", m.Max15mUp, m.Max15mDown), "当日最猛的单根异动"},
				{"BTC波动", fmt.Sprintf("ATR %.1f%%", m.BTCATRPct), "BTC 24h 波动率（高=肉多滑点狠）"},
			}))
		} else {
			sb.WriteString("（当日指标未采集，先运行 collect）\n")
		}
		sb.WriteString("\n## 策略表现（" + today + "）\n")
		if len(sums) > 0 {
			s := sums[len(sums)-1]
			sb.WriteString(fmtTable([]string{"今日盈亏", "交易数", "胜率"}, [][]string{
				{fmt.Sprintf("%+.2f U", s.TodayPnl), fmt.Sprintf("%d", s.TradeCount), fmt.Sprintf("%.1f%%", s.WinRate)},
			}))
			sb.WriteString("\n## 大白话归因\n")
			if len(metrics) > 0 {
				m := metrics[len(metrics)-1]
				attribution.WriteString(fmt.Sprintf("今天市场给了 %d 次异动机会（%d 个币），其中 5m 爆拉 %d 次；假突破率 %.0f%%。策略交易 %d 笔，盈亏 %+.2fU。",
					m.OpportunityTotal, m.OpportunityCount, m.BurstTotal, m.FakeBreakoutRate, s.TradeCount, s.TodayPnl))
				if m.FakeBreakoutRate >= 40 {
					attribution.WriteString(" 市场骗炮偏多，止损多属正常，不是策略失灵。")
				} else if m.OpportunityTotal < 5 {
					attribution.WriteString(" 今天肉少，交易少/亏损大概率是环境问题。")
				}
				sb.WriteString(attribution.String())
			}
		} else {
			sb.WriteString("（当日无已平仓记录）\n")
		}
	} else {
		label := map[string]string{"weekly": "周", "monthly": "月", "yearly": "年"}[typ]
		sb.WriteString(fmt.Sprintf("## %s总结（%s ~ %s）\n", label, from, today))
		sb.WriteString("## 策略表现\n")
		sb.WriteString(fmtTable([]string{"累计盈亏", "交易数", "盈利日占比", "最好日", "最差日"}, [][]string{
			{fmt.Sprintf("%+.2f U", totalPnl), fmt.Sprintf("%d", totalTrades),
				fmt.Sprintf("%.0f%%", winRate), fmt.Sprintf("%+.2f", best), fmt.Sprintf("%+.2f", worst)},
		}))
		sb.WriteString("\n## 市场环境\n")
		sb.WriteString(fmtTable([]string{"指标", "合计/均值"}, [][]string{
			{"日均机会数", fmt.Sprintf("%.1f（累计 %d）", avg(float64(oppSum), oppCnt), oppSum)},
			{"日均5m爆拉", fmt.Sprintf("%.1f 次", avg(float64(burstSum), oppCnt))},
			{"日均机会池", fmt.Sprintf("%.0f 个币", avg(float64(poolSum), oppCnt))},
			{"平均假突破率", fmt.Sprintf("%.1f%%", avg(fakeSum, oppCnt))},
			{"平均BTC波动", fmt.Sprintf("ATR %.1f%%", avg(atrSum, oppCnt))},
		}))
		if len(metrics) > 0 {
			var dayRows [][]string
			for _, m := range metrics {
				dayRows = append(dayRows, []string{m.Date, fmt.Sprintf("%d", m.OpportunityTotal),
					fmt.Sprintf("%d", m.BurstTotal), fmt.Sprintf("%.0f%%", m.FakeBreakoutRate)})
			}
			sb.WriteString("\n## 逐日环境\n")
			sb.WriteString(fmtTable([]string{"日期", "异动次数", "5m爆拉", "假突破率"}, dayRows))
		}
		sb.WriteString("\n## 大白话\n")
		attribution.WriteString(fmt.Sprintf("%s累计 %d 笔交易、%+.2fU；日均 %.1f 次异动机会、%.1f%% 假突破率。",
			label, totalTrades, totalPnl, avg(float64(oppSum), oppCnt), avg(fakeSum, oppCnt)))
		if totalPnl < 0 && avg(float64(oppSum), oppCnt) < 5 {
			attribution.WriteString(" 这段市场肉少，亏损主要来自环境而非策略。")
		} else if totalPnl > 0 {
			attribution.WriteString(" 环境与策略表现匹配，策略在吃肉。")
		}
		sb.WriteString(attribution.String())
	}
	return sb.String(), totalPnl, reportMeta{Metrics: metrics, Attribution: attribution.String()}, nil
}

// ==================== 写入 daily_summaries ====================

var flagDB string

func writeSummary(mode, typ, clientDB, notes string, pnl float64, meta reportMeta) error {
	db, err := sql.Open("sqlite3", "file:"+clientDB+"?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		return err
	}
	defer db.Close()
	now := time.Now().In(beijing)
	date := now.Format("2006-01-02")
	featureJSON := "{}"
	if b, jerr := json.Marshal(meta); jerr == nil {
		featureJSON = string(b)
	}
	var id int64
	err = db.QueryRow(`SELECT id FROM daily_summaries WHERE mode=? AND summary_date=? AND summary_type=? AND deleted_at=0`,
		mode, date, typ).Scan(&id)
	if err == nil {
		_, err = db.Exec(`UPDATE daily_summaries SET market_notes=?, today_pnl=?, feature_json=?, updated_at=? WHERE id=?`,
			notes, pnl, featureJSON, now.UnixMilli(), id)
	} else {
		_, err = db.Exec(`INSERT INTO daily_summaries
			(mode, summary_date, summary_type, market_notes, today_pnl, win_rate, trade_count, rating, feature_json, created_at, updated_at)
			VALUES (?,?,?,?,?,0,0,0,'{}',?,?)`,
			mode, date, typ, notes, pnl, featureJSON, now.UnixMilli(), now.UnixMilli())
	}
	return err
}

func main() {
	collectCmd := flag.NewFlagSet("collect", flag.ExitOnError)
	proxy := collectCmd.String("proxy", os.Getenv("HTTPS_PROXY"), "HTTP 代理地址")

	analyzeCmd := flag.NewFlagSet("analyze", flag.ExitOnError)
	analyzeProxy := analyzeCmd.String("proxy", os.Getenv("HTTPS_PROXY"), "HTTP 代理地址")
	analyzeRaw := analyzeCmd.Bool("raw", false, "原始爆拉后续走势分析（默认=策略口径模拟）")
	analyzeDB := analyzeCmd.String("db", "", "客户端数据库路径（写入每日策略总结）")
	analyzeDate := analyzeCmd.String("date", "", "分析指定日期（YYYY-MM-DD，默认今天）")
	analyzeBucket1m := analyzeCmd.Bool("bucket1m", false, "3桶改用 1m 收盘 vs 前一根 1m 收盘（对比实验；CSV 加 _1m 后缀，不写每日总结）")

	reportCmd := flag.NewFlagSet("report", flag.ExitOnError)
	mode := reportCmd.String("mode", "SIMULATION", "SIMULATION / LIVE")
	typ := reportCmd.String("type", "daily", "daily / weekly / monthly / yearly")
	flagDB = *reportCmd.String("db", `D:\0001_ba-A - 03\quant-desktop-smart\data\quant_simulation.db`, "客户端数据库路径")

	if len(os.Args) < 2 {
		fmt.Println("用法: market_metrics collect | report --mode SIMULATION --type daily --db <path>")
		os.Exit(1)
	}
	switch os.Args[1] {
	case "collect":
		collectCmd.Parse(os.Args[2:])
		if err := collect(*proxy); err != nil {
			log.Fatalf("采集失败: %v", err)
		}
	case "analyze":
		analyzeCmd.Parse(os.Args[2:])
		var err error
		if *analyzeRaw {
			err = analyzeBurst(*analyzeProxy, *analyzeDate)
		} else {
			err = analyzeStrategy(*analyzeProxy, *analyzeDB, *analyzeDate, *analyzeBucket1m)
		}
		if err != nil {
			log.Fatalf("分析失败: %v", err)
		}
	case "report":
		reportCmd.Parse(os.Args[2:])
		notes, pnl, meta, err := genReport(*mode, *typ)
		if err != nil {
			log.Fatalf("生成报告失败: %v", err)
		}
		if err := writeSummary(*mode, *typ, flagDB, notes, pnl, meta); err != nil {
			log.Fatalf("写入总结失败: %v", err)
		}
		fmt.Println(notes)
		fmt.Printf("\n✅ 已写入 %s (%s) 总结\n", *typ, flagDB)
	default:
		fmt.Println("未知命令:", os.Args[1])
		os.Exit(1)
	}
}

// analyzeBurst 复盘分析（--raw）：今天每根"5m 收盘涨幅≥2.5%"（收盘 vs 前一根收盘）的爆拉之后，
// 价格续涨情况 / 15m 累计是否突破 3% / 继续上涨幅度分布。
func analyzeBurst(proxy, dateStr string) error {
	client := httpClient(proxy)
	tickers, err := fetchTickers(client)
	if err != nil {
		return fmt.Errorf("拉取行情失败: %w", err)
	}
	now := time.Now().In(beijing)
	if dateStr != "" {
		t, perr := time.ParseInLocation("2006-01-02", dateStr, beijing)
		if perr != nil {
			return fmt.Errorf("日期格式错误（应为 YYYY-MM-DD）: %w", perr)
		}
		now = t
	}
	dayStart := beijingDayStartUTC(now)

	pool := make([]ticker24, 0, len(tickers))
	for _, t := range tickers {
		if t.QuoteVolume >= minPoolVol {
			pool = append(pool, t)
		}
	}

	type event struct {
		symbol       string
		burstGain    float64 // 爆拉 5m 收盘涨幅 %（收盘 vs 前一根收盘）
		cycleOpen    float64 // 所在 15m 周期开盘价
		cycleGain    float64 // 爆拉收盘时 15m 累计 %
		maxGainAfter float64 // 爆拉后 6 根 5m 内相对爆拉收盘的最大涨幅 %
		reach3       bool    // 爆拉后 15m 累计（含后续 3 根）达到 3%
		endGain      float64 // 爆拉后 6 根 5m 收盘相对爆拉收盘 %
	}
	var events []event

	for i, t := range pool {
		if i >= 160 {
			break
		}
		k5, err5 := fetchKlines(client, t.Symbol, "5m", 288)
		if err5 != nil || len(k5) < 4 {
			continue
		}
		k15, err15 := fetchKlines(client, t.Symbol, "15m", 96)
		if err15 != nil {
			continue
		}
		// 构建 15m 周期 open 查找（按时间）
		for j := 1; j < len(k5)-6; j++ {
			k := k5[j]
			if k.openTime < dayStart || k.close <= 0 {
				continue
			}
			prev := k5[j-1].close
			if prev <= 0 {
				continue
			}
			burst := (k.close - prev) / prev * 100
			if burst < 2.5 {
				continue
			}
			// 所在 15m 周期 open
			var co float64
			for _, c := range k15 {
				if c.openTime <= k.openTime {
					co = c.open
				} else {
					break
				}
			}
			if co <= 0 {
				continue
			}
			cycleGain := (k.close - co) / co * 100
			// 爆拉后 6 根 5m 的最高价/收盘
			hmax, cend := k.close, k.close
			for z := j + 1; z <= j+6 && z < len(k5); z++ {
				if k5[z].high > hmax {
					hmax = k5[z].high
				}
				cend = k5[z].close
			}
			maxGainAfter := (hmax - k.close) / k.close * 100
			endGain := (cend - k.close) / k.close * 100
			// 爆拉后 3 根内 15m 累计是否达 3%（后续 5m 最高 vs 周期 open）
			reach3 := cycleGain >= 3
			for z := j + 1; z <= j+3 && z < len(k5); z++ {
				if k5[z].high >= co*1.03 {
					reach3 = true
					break
				}
			}
			events = append(events, event{
				symbol: t.Symbol, burstGain: burst, cycleOpen: co, cycleGain: cycleGain,
				maxGainAfter: maxGainAfter, reach3: reach3, endGain: endGain,
			})
		}
	}

	if len(events) == 0 {
		fmt.Println("今天没有 5m 爆拉事件")
		return nil
	}
	// 汇总
	symbols := map[string]bool{}
	reach3N, reach3AtBurst, upN, flatN := 0, 0, 0, 0
	upSum, reach3CycleSum := 0.0, 0.0
	buckets := map[string]int{"≤0%(回落)": 0, "0~1%": 0, "1~3%": 0, "3~5%": 0, ">5%": 0}
	for _, e := range events {
		symbols[e.symbol] = true
		if e.cycleGain >= 3 {
			reach3AtBurst++
		}
		if e.reach3 {
			reach3N++
			reach3CycleSum += e.cycleGain
		}
		if e.maxGainAfter > 0 {
			upN++
			upSum += e.maxGainAfter
		} else {
			flatN++
		}
		switch {
		case e.maxGainAfter <= 0:
			buckets["≤0%(回落)"]++
		case e.maxGainAfter < 1:
			buckets["0~1%"]++
		case e.maxGainAfter < 3:
			buckets["1~3%"]++
		case e.maxGainAfter < 5:
			buckets["3~5%"]++
		default:
			buckets[">5%"]++
		}
	}
	n := len(events)
	pct := func(v int) string { return fmt.Sprintf("%.1f%%", float64(v)/float64(n)*100) }
	fmt.Printf("\n===== 今日 5m 爆拉复盘（%s，池内成交额≥2000万）=====\n", now.Format("2006-01-02"))
	fmt.Println(fmtTable([]string{"指标", "数值", "说明"}, [][]string{
		{"爆拉事件", fmt.Sprintf("%d 币 / %d 次", len(symbols), n), "5m 收盘涨≥2.5%（收盘 vs 前一根收盘）"},
		{"爆拉时已达标", fmt.Sprintf("%d 次（%s）", reach3AtBurst, pct(reach3AtBurst)), "爆拉收盘时该 15m 累计已≥3%"},
		{"爆拉后达 3%", fmt.Sprintf("%d 次（%s）", reach3N, pct(reach3N)), "爆拉后 3 根 5m 内 15m 累计触及 3%"},
		{"爆拉后续涨", fmt.Sprintf("%d 次（%s）", upN, pct(upN)), "爆拉后 6 根 5m 内最高价高于爆拉收盘"},
		{"爆拉后回落", fmt.Sprintf("%d 次（%s）", flatN, pct(flatN)), "爆拉后未再创新高"},
		{"平均续涨幅度", fmt.Sprintf("+%.2f%%", upSum/float64(maxInt(upN, 1))), "续涨事件平均最大涨幅"},
	}))
	fmt.Println("\n续涨幅度分布（相对爆拉收盘价，6 根 5m 内最高）:")
	fmt.Println(fmtTable([]string{"区间", "次数", "占比"}, [][]string{
		{"≤0%（回落）", fmt.Sprintf("%d", buckets["≤0%(回落)"]), pct(buckets["≤0%(回落)"])},
		{"0~1%", fmt.Sprintf("%d", buckets["0~1%"]), pct(buckets["0~1%"])},
		{"1~3%", fmt.Sprintf("%d", buckets["1~3%"]), pct(buckets["1~3%"])},
		{"3~5%", fmt.Sprintf("%d", buckets["3~5%"]), pct(buckets["3~5%"])},
		{">5%", fmt.Sprintf("%d", buckets[">5%"]), pct(buckets[">5%"])},
	}))
	return nil
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// max5mGainClose 开仓时当前 15m 周期内最大 5m 收盘涨幅（桶判定口径，与客户端/回测一致）：
// 每根 5m 的涨幅 = 该根收盘价 vs 前一根 5m 收盘价；周期内首根以前一根（周期外）收盘价为基准。
func max5mGainClose(k5 []kline, j int) float64 {
	if j < 0 || j >= len(k5) {
		return 0
	}
	periodStart := k5[j].openTime - k5[j].openTime%900000
	first := j
	for first >= 1 && k5[first-1].openTime >= periodStart {
		first--
	}
	prev := 0.0
	if first >= 1 {
		prev = k5[first-1].close
	}
	mx := 0.0
	for z := first; z <= j; z++ {
		if k5[z].close <= 0 || prev <= 0 {
			continue
		}
		g := (k5[z].close - prev) / prev * 100
		if g > mx {
			mx = g
		}
		prev = k5[z].close
	}
	return mx
}

// max1mGainClose 与 max5mGainClose 同口径的 1m 版本（--bucket1m 对比实验）：
// 当前 15m 周期内（截至所在 5m 收盘时刻）每根 1m 收盘 vs 前一根 1m 收盘的涨幅取最大。
func max1mGainClose(k1 []kline, j5 int, k5 []kline) float64 {
	if j5 < 0 || j5 >= len(k5) {
		return 0
	}
	periodStart := k5[j5].openTime - k5[j5].openTime%900000
	barEnd := k5[j5].openTime + 300000 // 该 5m 收盘时刻（含该 5m 内全部 1m）
	first := -1
	for z, k := range k1 {
		if k.openTime >= periodStart && k.openTime < barEnd {
			first = z
			break
		}
	}
	if first < 0 {
		return 0
	}
	prev := 0.0
	if first >= 1 {
		prev = k1[first-1].close
	}
	mx := 0.0
	for z := first; z < len(k1) && k1[z].openTime < barEnd; z++ {
		if k1[z].close <= 0 {
			prev = k1[z].close
			continue
		}
		if prev > 0 {
			g := (k1[z].close - prev) / prev * 100
			if g > mx {
				mx = g
			}
		}
		prev = k1[z].close
	}
	return mx
}

// bucketGainAt 分桶涨幅（5m 或 1m 粒度，--bucket1m 对比实验用）
func bucketGainAt(k5, k1 []kline, j int, use1m bool) float64 {
	if use1m {
		return max1mGainClose(k1, j, k5)
	}
	return max5mGainClose(k5, j)
}

func bucketOf(m5 float64) string {
	switch {
	case m5 >= 2.5:
		return "爆拉桶"
	case m5 >= 2:
		return "中间桶"
	default:
		return "温和桶"
	}
}

type actualTrade struct {
	Symbol  string
	Pnl     float64
	Opened  int64
	Status  string
	Closed  int64
}

// loadActualTrades 读取客户端库指定自然日实际开仓记录（含未平仓），用于"机会 vs 实际"对比。
func loadActualTrades(clientDB string, dayStart, dayEnd int64) ([]actualTrade, error) {
	db, err := sql.Open("sqlite3", "file:"+clientDB+"?mode=ro")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.Query(`SELECT symbol, ifnull(realized_pnl,0), opened_at, status, ifnull(closed_at,0)
		FROM positions WHERE opened_at >= ? AND opened_at < ?`, dayStart, dayEnd)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []actualTrade
	for rows.Next() {
		var t actualTrade
		var st string
		var closed int64
		if err := rows.Scan(&t.Symbol, &t.Pnl, &t.Opened, &st, &closed); err != nil {
			continue
		}
		t.Status = st
		t.Closed = closed
		out = append(out, t)
	}
	return out, nil
}

// ==================== 开仓尝试与在线窗口（转化率归因数据） ====================

// attemptRow 客户端开仓尝试记录（open_attempts 表，新构建才有）
type attemptRow struct {
	Ts        int64
	Symbol    string
	Stage     string // candidate / selected / attempted / filled / failed
	Reason    string // 拒绝原因（cooldown / no_active / balance ...）
	Bucket    string
	ErrorCode int64
}

func loadAttempts(clientDB string, dayStart, dayEnd int64) ([]attemptRow, bool, error) {
	if clientDB == "" {
		return nil, false, nil
	}
	db, err := sql.Open("sqlite3", "file:"+clientDB+"?mode=ro")
	if err != nil {
		return nil, false, err
	}
	defer db.Close()
	rows, err := db.Query(`SELECT ts, symbol, stage, reason, ifnull(bucket,''), ifnull(error_code,0)
		FROM open_attempts WHERE ts >= ? AND ts < ? ORDER BY ts`, dayStart, dayEnd)
	if err != nil {
		if strings.Contains(err.Error(), "no such table") {
			return nil, false, nil // 旧构建无此表：无法逐单归因
		}
		return nil, false, err
	}
	defer rows.Close()
	var out []attemptRow
	for rows.Next() {
		var a attemptRow
		if err := rows.Scan(&a.Ts, &a.Symbol, &a.Stage, &a.Reason, &a.Bucket, &a.ErrorCode); err != nil {
			continue
		}
		out = append(out, a)
	}
	return out, true, rows.Err()
}

func loadHeartbeats(clientDB string, dayStart, dayEnd int64) ([]int64, bool, error) {
	if clientDB == "" {
		return nil, false, nil
	}
	db, err := sql.Open("sqlite3", "file:"+clientDB+"?mode=ro")
	if err != nil {
		return nil, false, err
	}
	defer db.Close()
	rows, err := db.Query(`SELECT ts FROM engine_heartbeat WHERE ts >= ? AND ts < ? ORDER BY ts`, dayStart, dayEnd)
	if err != nil {
		if strings.Contains(err.Error(), "no such table") {
			return nil, false, nil
		}
		return nil, false, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var t int64
		if err := rows.Scan(&t); err != nil {
			continue
		}
		out = append(out, t)
	}
	return out, true, rows.Err()
}

// onlineAt 判断时刻 t 策略是否在线：心跳表存在 |最近心跳 - t| <= 3 分钟（心跳每 1 分钟一条）。
// 返回（是否在线, 是否有心跳数据）；无心跳数据（旧构建）时按「未归因」处理。
func onlineAt(beats []int64, t int64) (bool, bool) {
	if len(beats) == 0 {
		return false, false
	}
	i := sort.Search(len(beats), func(i int) bool { return beats[i] >= t })
	best := int64(1) << 62
	if i < len(beats) {
		if d := beats[i] - t; d < best {
			best = d
		}
	}
	if i > 0 {
		if d := t - beats[i-1]; d < best {
			best = d
		}
	}
	return best <= 3*60*1000, true
}

// 反事实回放出口规则常量（与策略口径模拟一致：D 骨架 sl3 / act2 / cb3 / 超时 36 根）
const (
	nominal = 100.0 // 每仓名义 100U（10U 保证金 × 10x）
	slPct   = 0.03  // 止损 -3%
	actPct  = 0.02  // 跟踪止盈激活 +2%
	cbPct   = 0.03  // 激活后回撤 3% 平仓
	maxBars = 36    // 最长持仓 36 根 5m = 180 分钟
)

// replayForcedOpen 反事实回放：一笔被拦截的机会「假如开仓」会怎样。
// 在 5m K 线索引 j 处以该根收盘价入仓（与策略口径模拟一致），按出口规则走完：
// 止损 → 激活(+2%)后跟踪回撤 3% → 超时 180 分钟。
// 返回（虚拟盈亏：100U 名义 × 桶倍数，离场原因，是否已平仓，盘中是否曾达 +2% 盈利高点）。
// 数据到底仍持有 → 返回 ("HOLDING", false)，不计入挡对/误杀。
func replayForcedOpen(k5 []kline, j int, bucket string) (float64, string, bool, bool) {
	mult := 1.0
	switch bucket {
	case "爆拉桶":
		mult = 1.5
	case "温和桶":
		mult = 0.7
	}
	if j < 0 || j >= len(k5) || k5[j].close <= 0 {
		return 0, "", false, false
	}
	entry := k5[j].close
	extreme := entry
	active := false
	hitProfit := false
	held := 1
	for i := j + 1; i < len(k5); i++ {
		k := k5[i]
		held++
		// 口径 B：盘中是否曾达 +2% 浮盈（盈利高点），与最终盈亏无关
		if k.high >= entry*(1+actPct) {
			hitProfit = true
		}
		// 止损优先（与回测保守顺序一致）
		if k.low <= entry*(1-slPct) {
			return (k.low - entry) / entry * nominal * mult, "STOP_LOSS", true, hitProfit
		}
		if !active && k.high >= entry*(1+actPct) {
			active = true
			extreme = k.high
		}
		if active {
			if k.high > extreme {
				extreme = k.high
			}
			if k.low <= extreme*(1-cbPct) {
				return (extreme*(1-cbPct) - entry) / entry * nominal * mult, "TRAILING", true, hitProfit
			}
		}
		if held >= maxBars {
			return (k.close - entry) / entry * nominal * mult, "MAX_HOLD", true, hitProfit
		}
	}
	return 0, "HOLDING", false, hitProfit
}

// 归因结果分类（与 daily_summaries feature_json.lossClass 对应）
const (
	clsFill       = "成交"
	clsRule       = "拦截"
	clsActivation = "激活错配"
	clsOrderFail  = "执行失败"
	clsBalance    = "余额不足"
	clsDataGap    = "数据缺口"
	clsSignalRace = "信号未触发"
	clsOffline    = "客户端未运行"
	clsUnknown    = "未归因"
)

// ruleReasonCN 客户端规则拒绝原因中文名（拦截 = 策略规则内未开，该挡）
var ruleReasonCN = map[string]string{
	"maxpos":            "全局10仓上限",
	"cooldown":          "冷却期内",
	"no_active":         "持仓未激活无法追单",
	"addon_limit":       "同币追单达上限",
	"addon_disabled":    "追加仓关闭",
	"addon_side":        "追单方向不一致",
	"new_listing":       "新币过滤",
	"24h_gain":          "24h涨幅不足",
	"rank":              "24h涨幅排名未通过",
	"pullback":          "山顶过滤器",
	"volume":            "成交额不足",
	"futures_only":      "无合约交易对",
	"round_zero":        "取整后数量为0",
	"open_failed_cd":    "开仓失败冷却中",
	"open_blocked":      "结构性失败拉黑",
	"breaker":           "熔断触发",
	"slots":             "槽位已满",
	"no_price":          "无实时价",
	"balance_query_fail": "余额查询失败",
	"kline_missing":     "K线数据缺失",
}

func isRuleReason(r string) bool {
	_, ok := ruleReasonCN[r]
	return ok
}

// attemptPriority 尝试记录的具体程度（越小越具体，用于 ±10min 窗口内选最贴切的记录）
func attemptPriority(a attemptRow, so simOpenRec) int {
	switch {
	case a.Stage == "failed":
		return 1
	case a.Reason == "balance":
		return 2
	case a.Reason == "kline_missing":
		return 3
	case a.Reason == "no_active" && so.addOn:
		return 4 // 激活错配：模拟判可追单、客户端未激活
	case a.Reason == "no_active":
		return 5
	case isRuleReason(a.Reason):
		return 5
	case a.Stage == "selected" || a.Stage == "attempted" || a.Stage == "filled":
		return 6
	default:
		return 7 // candidate 兜底
	}
}

// classifySim 对一条模拟可开机会做归因（调用方负责先做成交匹配）。
// 返回（分类, 说明, 原始拦截原因）。原始拦截原因仅拦截类非空，供拦截健康度分组。
func classifySim(so simOpenRec, attempts []attemptRow, beats []int64) (string, string, string) {
	best := -1
	bestPrio := 99
	bestD := int64(10 * 60 * 1000) // ±10 分钟窗口（5m K 线开盘时间 vs 客户端实时动作）
	for i := range attempts {
		d := attempts[i].Ts - so.ts
		if d < 0 {
			d = -d
		}
		if d > bestD {
			continue
		}
		prio := attemptPriority(attempts[i], so)
		if prio < bestPrio {
			bestPrio = prio
			best = i
		}
	}
	if best >= 0 {
		a := attempts[best]
		switch {
		case a.Stage == "failed":
			return clsOrderFail, fmt.Sprintf("执行损耗-交易所拒绝 code=%d", a.ErrorCode), ""
		case a.Reason == "balance":
			return clsBalance, "执行损耗-余额不足（逐候选预算）", ""
		case a.Reason == "kline_missing":
			return clsDataGap, "执行损耗-15m K线拉取失败", ""
		case a.Reason == "no_active" && so.addOn:
			return clsActivation, "执行损耗-模拟判可追单但客户端未激活（K线高点补判已修复）", ""
		case a.Reason == "no_active":
			return clsRule, "拦截-该挡（持仓未激活无法追单）", a.Reason
		case isRuleReason(a.Reason):
			return clsRule, "拦截-该挡（" + ruleReasonCN[a.Reason] + "）", a.Reason
		default:
			return clsSignalRace, "信号未触发（客户端看到候选但无成交/失败记录）", ""
		}
	}
	if on, known := onlineAt(beats, so.ts); known {
		if on {
			return clsSignalRace, "信号未触发（tick采样差/收盘判定未覆盖）", ""
		}
		return clsOffline, "客户端未运行（不计损耗）", ""
	}
	return clsUnknown, "旧构建无尝试记录，无法归因", ""
}

// ==================== 策略口径模拟（analyze 默认）====================
// 按 D 策略真实规则回放当天 5m 数据：
//   入仓: 15m 累计(收盘 vs 周期开盘) ≥3%
//   激活: 价格 ≥ 入场价×1.02 → 移动止盈开始（跟踪回调 3%）
//   追单: 已激活且再次命中信号 → 追加独立单（同币最多 1+2=3 仓）
//   平仓: 止损 -3% / 跟踪止盈(激活后从最高回撤3%) / 超时 180 分钟(36 根)
//   冷却: 移动止盈平仓后 15 分钟可再入，止损/超时后 30 分钟
//   名义: 每仓 100U（10U 保证金 × 10x）× 桶倍数（爆拉1.5 / 中间1.0 / 温和0.7）

type simPos struct {
	entry    float64
	extreme  float64
	active   bool
	heldBars int
	addOn    bool
	bucket   string
	mult     float64
	pnl      float64
	reason   string
}

type simState struct {
	positions []*simPos
	cdUntil   int64 // 冷却截止（该币）
	lastCD    string
	signals   int
	opens     int
	addons    int
	closed    []*simPos
}

// rejectRec 一笔"有信号但未开仓"的逐单记录（根因细化用）
type rejectRec struct {
	symbol  string
	ts      int64 // 信号对应 5m K 线开盘时间（反事实回放定位用）
	timeStr string
	bucket  string
	reason  string // maxpos=全局10仓上限 / cooldown=冷却中 / no_active=持仓未激活无法追单 / addon_limit=追单达上限
	seq     int    // 该币当日第几个信号
}

type simOpenRec struct {
	symbol  string
	ts      int64
	bucket  string
	addOn   bool
}

// simDetail 一条模拟可开机会的归因结果（分类 + 说明）
type simDetail struct {
	so     simOpenRec
	cls    string
	why    string
	reason string // 拦截类对应的原始拒绝原因（非拦截为空）
}

// interceptStat 拦截健康度单原因统计（反事实回放聚合）
type interceptStat struct {
	count, closed, holding, win, loss int
	pnl                              float64
}

// interceptDetail 拦截健康度逐单明细（CSV 用）
type interceptDetail struct {
	reason, symbol, bucket string
	ts                     int64
	pnl                    float64
	exit                   string
	closed                 bool
}

// ovAgg 机会价值漏斗单桶聚合（价值口径：次数 → 钱）
type ovAgg struct {
	opp, closedV, profitCnt, hitProfit int
	virtualVal, profitVal              float64
	actCnt, actClosed                  int
	actVal, actProfit                  float64
}

// lossAgg 漏掉的肉：按归因类别聚合未成交机会的虚拟价值
type lossAgg struct {
	cnt, closedV int
	val, miss, dodge float64
}

func analyzeStrategy(proxy, clientDB, dateStr string, bucket1m bool) error {
	client := httpClient(proxy)
	tickers, err := fetchTickers(client)
	if err != nil {
		return fmt.Errorf("拉取行情失败: %w", err)
	}
	now := time.Now().In(beijing)
	if dateStr != "" {
		t, perr := time.ParseInLocation("2006-01-02", dateStr, beijing)
		if perr != nil {
			return fmt.Errorf("日期格式错误（应为 YYYY-MM-DD）: %w", perr)
		}
		now = t
	}
	dayStart := beijingDayStartUTC(now)
	dayEnd := dayStart + 24*60*60*1000
	use1m := bucket1m
	csvSuffix := ""
	if use1m {
		csvSuffix = "_1m"
	}

	pool := make([]ticker24, 0, len(tickers))
	for _, t := range tickers {
		if t.QuoteVolume >= minPoolVol {
			pool = append(pool, t)
		}
	}

	const (
		gainReq  = 3.0
		cdTrail  = int64(15 * 60 * 1000)
		cdStop   = int64(30 * 60 * 1000)
		maxAddOn = 2
	)

	var allClosed []*simPos
	var totalSignals, totalOpens, totalAddons int
	var dayPnl float64
	globalOpen := 0
	var rejects []rejectRec
	var simOpens []simOpenRec
	type bucketStat struct {
		opens, closed, wins int
		pnl                 float64
	}
	buckets := map[string]*bucketStat{
		"爆拉桶": {},
		"中间桶": {},
		"温和桶": {},
	}
	bucketMult := map[string]float64{"爆拉桶": 1.5, "中间桶": 1.0, "温和桶": 0.7}
	signalsByBucket := map[string]int{"爆拉桶": 0, "中间桶": 0, "温和桶": 0}
	dedupSignalsByBucket := map[string]int{"爆拉桶": 0, "中间桶": 0, "温和桶": 0}
	dedupSeen := map[string]bool{}

	// 实际交易（客户端库当天开仓）
	var actuals []actualTrade
	if clientDB != "" {
		actuals, _ = loadActualTrades(clientDB, dayStart, dayEnd)
	}
	// 开仓尝试 + 引擎心跳（转化率逐单归因；旧构建无表时为空）
	var attempts []attemptRow
	var beats []int64
	if clientDB != "" {
		attempts, _, _ = loadAttempts(clientDB, dayStart, dayEnd)
		beats, _, _ = loadHeartbeats(clientDB, dayStart, dayEnd)
	}
	attemptsBySym := map[string][]attemptRow{}
	for _, a := range attempts {
		attemptsBySym[a.Symbol] = append(attemptsBySym[a.Symbol], a)
	}
	actualByBucket := map[string]int{"爆拉桶": 0, "中间桶": 0, "温和桶": 0}
	actualPnlByBucket := map[string]float64{"爆拉桶": 0, "中间桶": 0, "温和桶": 0}
	actualClosedByBucket := map[string]int{"爆拉桶": 0, "中间桶": 0, "温和桶": 0}
	simFirstByBucket := map[string]int{"爆拉桶": 0, "中间桶": 0, "温和桶": 0}
	simAddonByBucket := map[string]int{"爆拉桶": 0, "中间桶": 0, "温和桶": 0}
	addonLimitByBucket := map[string]int{"爆拉桶": 0, "中间桶": 0, "温和桶": 0}
	actFirstByBucket := map[string]int{"爆拉桶": 0, "中间桶": 0, "温和桶": 0}
	actAddonByBucket := map[string]int{"爆拉桶": 0, "中间桶": 0, "温和桶": 0}
	k5Cache := map[string][]kline{}
	k1Cache := map[string][]kline{}
	k1Skipped := 0

	for i, t := range pool {
		if i >= 160 {
			break
		}
		k5, err5 := fetchKlines(client, t.Symbol, "5m", 288)
		if err5 != nil || len(k5) < 40 {
			continue
		}
		k5Cache[t.Symbol] = k5
		if use1m {
			k1, err1 := fetchKlines(client, t.Symbol, "1m", 1440)
			if err1 != nil || len(k1) < 10 {
				k1Skipped++
				continue
			}
			k1Cache[t.Symbol] = k1
		}
		k15, err15 := fetchKlines(client, t.Symbol, "15m", 96)
		if err15 != nil {
			continue
		}
		st := &simState{}
		for j := 0; j < len(k5); j++ {
			k := k5[j]
			if k.openTime < dayStart {
				continue
			}
			// 15m 周期 open
			var co float64
			for _, c := range k15 {
				if c.openTime <= k.openTime {
					co = c.open
				} else {
					break
				}
			}
			cycleGain := 0.0
			if co > 0 {
				cycleGain = (k.close - co) / co * 100
			}
			signal := co > 0 && cycleGain >= gainReq
			seq := 0
			if signal {
				seq = st.signals + 1
				st.signals++
				totalSignals++
			}
			m5 := 0.0
			if signal {
				m5 = bucketGainAt(k5, k1Cache[t.Symbol], j, use1m)
				signalsByBucket[bucketOf(m5)]++
				// 去重机会：同一币同一 15m 周期只算一次（连续达标重复计会夸大机会数）
				periodKey := t.Symbol + "|" + strconv.FormatInt(k.openTime/900000, 10)
				if !dedupSeen[periodKey] {
					dedupSeen[periodKey] = true
					dedupSignalsByBucket[bucketOf(m5)]++
				}
			}

			// 1) 现有持仓监控（先处理平仓再开仓，同根按保守顺序：止损→激活→跟踪→超时）
			kept := st.positions[:0]
			for _, p := range st.positions {
				p.heldBars++
				closed := false
				if k.low <= p.entry*(1-slPct) {
					p.pnl = (k.low - p.entry) / p.entry * nominal * p.mult
					p.reason = "STOP_LOSS"
					closed = true
				} else if !p.active && k.high >= p.entry*(1+actPct) {
					p.active = true
					p.extreme = k.high
				} else if p.active {
					if k.high > p.extreme {
						p.extreme = k.high
					}
					if k.low <= p.extreme*(1-cbPct) {
						p.pnl = (p.extreme*(1-cbPct) - p.entry) / p.entry * nominal * p.mult
						p.reason = "TRAILING"
						closed = true
					}
				}
				if !closed && p.heldBars >= maxBars {
					p.pnl = (k.close - p.entry) / p.entry * nominal * p.mult
					p.reason = "MAX_HOLD"
					closed = true
				}
				if closed {
					globalOpen--
					dayPnl += p.pnl
					allClosed = append(allClosed, p)
					st.closed = append(st.closed, p)
					if b := buckets[p.bucket]; b != nil {
						b.closed++
						b.pnl += p.pnl
						if p.pnl > 0 {
							b.wins++
						}
					}
					if p.reason == "TRAILING" {
						st.cdUntil = k.openTime + cdTrail
						st.lastCD = "TRAILING"
					} else {
						st.cdUntil = k.openTime + cdStop
						st.lastCD = p.reason
					}
				} else {
					kept = append(kept, p)
				}
			}
			st.positions = kept

			// 2) 开仓/追单（未开时记录逐单原因）
			if signal && k.openTime >= st.cdUntil {
				bkt := bucketOf(m5)
				mult := bucketMult[bkt]
				// 全局同时持仓上限（模拟建模：实际 Top10，信号密集时是主要执行约束）
				if globalOpen >= 10 {
					rejects = append(rejects, rejectRec{symbol: t.Symbol, ts: k.openTime, timeStr: time.UnixMilli(k.openTime).In(beijing).Format("15:04"), bucket: bkt, reason: "maxpos", seq: seq})
					continue
				}
				if len(st.positions) == 0 {
					st.positions = append(st.positions, &simPos{entry: k.close, extreme: k.close, heldBars: 1, bucket: bkt, mult: mult})
					simOpens = append(simOpens, simOpenRec{symbol: t.Symbol, ts: k.openTime, bucket: bkt})
					globalOpen++
					st.opens++
					totalOpens++
					buckets[bkt].opens++
				} else {
					// 追单：任一持仓已激活 且 同币未达上限
					anyActive := false
					for _, p := range st.positions {
						if p.active {
							anyActive = true
							break
						}
					}
					if anyActive && len(st.positions) < 1+maxAddOn {
						st.positions = append(st.positions, &simPos{entry: k.close, extreme: k.close, heldBars: 1, addOn: true, bucket: bkt, mult: mult})
						simOpens = append(simOpens, simOpenRec{symbol: t.Symbol, ts: k.openTime, bucket: bkt, addOn: true})
						globalOpen++
						st.addons++
						totalAddons++
						buckets[bkt].opens++
					} else {
						if !anyActive {
							rejects = append(rejects, rejectRec{symbol: t.Symbol, ts: k.openTime, timeStr: time.UnixMilli(k.openTime).In(beijing).Format("15:04"), bucket: bkt, reason: "no_active", seq: seq})
						} else {
							// 已达追单上限：仍算一次追单机会（想追但满 3 仓）
							addonLimitByBucket[bkt]++
							rejects = append(rejects, rejectRec{symbol: t.Symbol, ts: k.openTime, timeStr: time.UnixMilli(k.openTime).In(beijing).Format("15:04"), bucket: bkt, reason: "addon_limit", seq: seq})
						}
					}
				}
			} else if signal {
				rejects = append(rejects, rejectRec{symbol: t.Symbol, ts: k.openTime, timeStr: time.UnixMilli(k.openTime).In(beijing).Format("15:04"), bucket: bucketOf(m5), reason: "cooldown", seq: seq})
			}
		}
	}

	// 模拟开仓拆首仓/追单
	for _, so := range simOpens {
		if so.addOn {
			simAddonByBucket[so.bucket]++
		} else {
			simFirstByBucket[so.bucket]++
		}
	}
	for k, v := range addonLimitByBucket {
		simAddonByBucket[k] += v
	}

	// 实际交易回放桶（复用已拉 K 线，缺则补拉）；按币按时间序，第 1 单=首仓，其余=追单
	// 追单判定：开仓时同币是否有另一笔未平仓（时间重叠）→ 追单；否则为（首仓或循环后的新首仓）
	actualBySymbol := map[string][]actualTrade{}
	for _, a := range actuals {
		actualBySymbol[a.Symbol] = append(actualBySymbol[a.Symbol], a)
	}
	isAddOnActual := func(a actualTrade) bool {
		for _, b := range actualBySymbol[a.Symbol] {
			if b.Opened < a.Opened {
				if b.Status == "OPEN" || (b.Closed > a.Opened) {
					return true
				}
			}
		}
		return false
	}
	for _, a := range actuals {
		k5, ok := k5Cache[a.Symbol]
		if !ok {
			if len(k5Cache) >= 500 {
				continue
			}
			var err error
			k5, err = fetchKlines(client, a.Symbol, "5m", 288)
			if err != nil {
				continue
			}
			k5Cache[a.Symbol] = k5
		}
		if use1m {
			if _, ok := k1Cache[a.Symbol]; !ok {
				k1, err1 := fetchKlines(client, a.Symbol, "1m", 1440)
				if err1 != nil || len(k1) < 10 {
					continue
				}
				k1Cache[a.Symbol] = k1
			}
		}
		idx := -1
		for z := 0; z < len(k5); z++ {
			if k5[z].openTime <= a.Opened {
				idx = z
			} else {
				break
			}
		}
		if idx < 0 {
			continue
		}
		bkt := bucketOf(bucketGainAt(k5, k1Cache[a.Symbol], idx, use1m))
		actualByBucket[bkt]++
		if isAddOnActual(a) {
			actAddonByBucket[bkt]++
		} else {
			actFirstByBucket[bkt]++
		}
		if a.Status == "CLOSED" {
			actualClosedByBucket[bkt]++
			actualPnlByBucket[bkt] += a.Pnl
		}
	}

	n := len(allClosed)
	wins, losses := 0, 0
	winSum, lossSum := 0.0, 0.0
	reasonCnt := map[string]int{}
	activated := 0
	addOnClosed := 0
	for _, p := range allClosed {
		reasonCnt[p.reason]++
		if p.pnl > 0 {
			wins++
			winSum += p.pnl
		} else {
			losses++
			lossSum += p.pnl
		}
		if p.addOn {
			addOnClosed++
		}
	}
	bucketLabel := "5m"
	if use1m {
		bucketLabel = "1m（对比实验）"
	}
	fmt.Printf("桶粒度: %s\n", bucketLabel)
	if k1Skipped > 0 {
		fmt.Printf("（%d 个币 1m K 线拉取失败跳过）\n", k1Skipped)
	}
	fmt.Printf("\n===== D 策略口径当日模拟（%s，10U×10x=100U/仓，未计手续费/滑点）=====\n", now.Format("2006-01-02"))
	fmt.Println(fmtTable([]string{"指标", "数值", "说明"}, [][]string{
		{"15m 信号次数", fmt.Sprintf("%d", totalSignals), "收盘 vs 15m 周期开盘 ≥3%"},
		{"开仓次数", fmt.Sprintf("%d（追单 %d）", totalOpens, totalAddons), "含冷却过滤；同币最多 1+2 仓"},
		{"已平仓", fmt.Sprintf("%d 笔", n), "当日已按规则平出的仓"},
		{"胜率", fmt.Sprintf("%.1f%%", float64(wins)/float64(n)*100), fmt.Sprintf("盈利 %d / 亏损 %d", wins, losses)},
		{"盈亏", fmt.Sprintf("%+.2f U", dayPnl), "按 100U 名义/仓"},
		{"平均每笔", fmt.Sprintf("%+.2f U", dayPnl/float64(n)), ""},
		{"止损/跟踪/超时", fmt.Sprintf("%d / %d / %d", reasonCnt["STOP_LOSS"], reasonCnt["TRAILING"], reasonCnt["MAX_HOLD"]), "平仓原因分布"},
	}))
	if reasonCnt["TRAILING"] > 0 {
		activated = reasonCnt["TRAILING"]
	}
	fmt.Println("\n激活与追单（用平仓前激活状态近似）:")
	fmt.Println(fmtTable([]string{"指标", "数值", "说明"}, [][]string{
		{"移动止盈触发", fmt.Sprintf("%d 笔", activated), "最终以跟踪止盈离场（已激活）"},
		{"追单平仓", fmt.Sprintf("%d 笔", addOnClosed), "追单单独离场笔数"},
		{"追单占比", fmt.Sprintf("%.1f%%", float64(addOnClosed)/float64(n)*100), ""},
	}))
	fmt.Println("\n⚠ 口径：本模拟按 5m 收盘/高低价近似回放，未含手续费滑点，已按 5m 爆拉分桶放大仓位（爆拉1.5/中间1.0/温和0.7）；用于复盘当日策略环境，非实盘对账。")

	// ==================== 转化率归因：模拟机会 → 成交 / 拦截(该挡) / 执行损耗 ====================
	// 1) 实际成交匹配：同币 + 时间窗 ±45min 贪婪匹配（替代旧版按序号硬匹配，
	//    消除 SKYAIUSDT 19:40 机会 / 19:45 成交 之类的时间错位假损耗）
	simMatched := make([]bool, len(simOpens))
	actUsed := make([]bool, len(actuals))
	simActIdx := make([]int, len(simOpens)) // 每条模拟机会匹配到的实际持仓下标（-1=未匹配）
	for i := range simActIdx {
		simActIdx[i] = -1
	}
	for i, so := range simOpens {
		best, bestD := -1, int64(45*60*1000)
		for j, a := range actuals {
			if actUsed[j] || a.Symbol != so.symbol {
				continue
			}
			d := a.Opened - so.ts
			if d < 0 {
				d = -d
			}
			if d < bestD {
				bestD = d
				best = j
			}
		}
		if best >= 0 {
			actUsed[best] = true
			simMatched[i] = true
			simActIdx[i] = best
		}
	}
	// 2) 未成交的逐条归因（拦截=该挡 / 执行损耗=真因 / 未运行 / 未归因）
	var details []simDetail
	for i, so := range simOpens {
		cls, why, reason := clsFill, "实际成交", ""
		if !simMatched[i] {
			cls, why, reason = classifySim(so, attemptsBySym[so.symbol], beats)
		}
		details = append(details, simDetail{so: so, cls: cls, why: why, reason: reason})
	}
	// 客户端多开（模拟无对应机会但实际成交，实时通道更早入场等）
	extraOpens := 0
	for _, used := range actUsed {
		if !used {
			extraOpens++
		}
	}

	// 3) 汇总：按桶 × 首仓/追单
	type aggRow struct {
		firstOpp, firstFill, firstRule, firstLoss, firstOff, firstUnk int
		addonOpp, addonFill, addonRule, addonLoss, addonOff, addonUnk  int
	}
	aggs := map[string]*aggRow{}
	for _, name := range []string{"爆拉桶", "中间桶", "温和桶"} {
		aggs[name] = &aggRow{}
	}
	totalAgg := &aggRow{}
	count := func(ar *aggRow, d simDetail) {
		if d.so.addOn {
			ar.addonOpp++
			switch d.cls {
			case clsFill:
				ar.addonFill++
			case clsRule:
				ar.addonRule++
			case clsOffline:
				ar.addonOff++
			case clsUnknown:
				ar.addonUnk++
			default: // 执行损耗类
				ar.addonLoss++
			}
		} else {
			ar.firstOpp++
			switch d.cls {
			case clsFill:
				ar.firstFill++
			case clsRule:
				ar.firstRule++
			case clsOffline:
				ar.firstOff++
			case clsUnknown:
				ar.firstUnk++
			default:
				ar.firstLoss++
			}
		}
	}
	for _, d := range details {
		count(aggs[d.so.bucket], d)
		count(totalAgg, d)
	}
	// 4) 每日可开仓漏斗（转化率口径：成交/机会；拦截=该挡，损耗=执行层真因）
	fmt.Println("\n每日可开仓漏斗（策略规则可做，含10仓上限；拦截=策略规则内未开=该挡；损耗=执行层真实原因）:")
	fh := []string{"桶", "首仓机会", "首仓成交", "首仓拦截", "首仓损耗", "追单机会", "追单成交", "追单拦截", "追单损耗", "机会合计", "成交合计", "转化率"}
	var funnelRows [][]string
	appendFunnel := func(name string, a *aggRow) {
		opp := a.firstOpp + a.addonOpp
		fill := a.firstFill + a.addonFill
		conv := 0.0
		if opp > 0 {
			conv = float64(fill) / float64(opp) * 100
		}
		funnelRows = append(funnelRows, []string{
			name,
			fmt.Sprintf("%d", a.firstOpp), fmt.Sprintf("%d", a.firstFill),
			fmt.Sprintf("%d", a.firstRule), fmt.Sprintf("%d", a.firstLoss),
			fmt.Sprintf("%d", a.addonOpp), fmt.Sprintf("%d", a.addonFill),
			fmt.Sprintf("%d", a.addonRule), fmt.Sprintf("%d", a.addonLoss),
			fmt.Sprintf("%d", opp), fmt.Sprintf("%d", fill), fmt.Sprintf("%.1f%%", conv),
		})
	}
	for _, name := range []string{"爆拉桶", "中间桶", "温和桶"} {
		appendFunnel(name, aggs[name])
	}
	appendFunnel("合计", totalAgg)
	fmt.Println(fmtTable(fh, funnelRows))
	fmt.Printf("客户端多开（模拟无对应机会但实际成交，多为实时通道更早入场）: %d 笔\n", extraOpens)

	// 5) 逐单归因分类汇总（执行损耗拆到真实原因）
	clsTotal := map[string]int{}
	clsByBucket := map[string]map[string]int{}
	for _, name := range []string{"爆拉桶", "中间桶", "温和桶"} {
		clsByBucket[name] = map[string]int{}
	}
	for _, d := range details {
		clsTotal[d.cls]++
		clsByBucket[d.so.bucket][d.cls]++
	}
	fmt.Println("\n逐单归因汇总（每笔模拟机会的最终去向）:")
	clsNames := []string{clsFill, clsRule, clsActivation, clsOrderFail, clsBalance, clsDataGap, clsSignalRace, clsOffline, clsUnknown}
	var clsRows [][]string
	for _, name := range []string{"爆拉桶", "中间桶", "温和桶"} {
		row := []string{name}
		for _, c := range clsNames {
			row = append(row, fmt.Sprintf("%d", clsByBucket[name][c]))
		}
		clsRows = append(clsRows, row)
	}
	totalRow := []string{"合计"}
	for _, c := range clsNames {
		totalRow = append(totalRow, fmt.Sprintf("%d", clsTotal[c]))
	}
	clsRows = append(clsRows, totalRow)
	fmt.Println(fmtTable(append([]string{"桶"}, clsNames...), clsRows))

	// ==================== 逐单拦截明细（策略规则内未开 = 该挡） ====================
	rejectCnt := map[string]int{"maxpos": 0, "cooldown": 0, "no_active": 0, "addon_limit": 0}
	rejectNames := map[string]string{
		"maxpos":      "全局10仓上限",
		"cooldown":    "冷却期内",
		"no_active":   "持仓未激活(无法追单)",
		"addon_limit": "同币追单达上限",
	}
	for _, r := range rejects {
		rejectCnt[r.reason]++
	}
	fmt.Println("\n逐单拦截明细（策略规则内未开 = 该挡；模拟侧按原口径）:")
	var rejectRows [][]string
	for _, k := range []string{"maxpos", "cooldown", "no_active", "addon_limit"} {
		rejectRows = append(rejectRows, []string{rejectNames[k], fmt.Sprintf("%d", rejectCnt[k])})
	}
	fmt.Println(fmtTable([]string{"拦截原因", "次数"}, rejectRows))

	// 客户端侧规则拦截（该挡，新增归因）：从逐单归因中提取
	clientRuleCnt := map[string]int{}
	for _, d := range details {
		if d.cls == clsRule {
			clientRuleCnt[d.why]++
		}
	}
	if len(clientRuleCnt) > 0 {
		var crRows [][]string
		for k, v := range clientRuleCnt {
			crRows = append(crRows, []string{strings.TrimPrefix(strings.TrimSuffix(k, "）"), "拦截-该挡（"), fmt.Sprintf("%d", v)})
		}
		sort.Slice(crRows, func(i, j int) bool { return crRows[i][1] > crRows[j][1] })
		fmt.Println(fmtTable([]string{"客户端侧拦截（该挡，新增归因）", "次数"}, crRows))
	}

	// ==================== 拦截健康度：反事实回放（该挡验证） ====================
	// 对每笔「拦截 = 策略规则内未开」的机会做反事实回放：假如当时开仓，
	// 按 D 出口规则（止损3% / 激活2% / 跟踪回撤3% / 超时180分）走到平仓的虚拟盈亏。
	// 挡对率 = 虚拟亏损占比（拦对了）；误杀率 = 虚拟盈利占比（拦错了，错过利润）。
	// 目的：用数据验证规则是真该挡，而不是只看「规则挡了」。
	type interceptItem struct {
		symbol string
		ts     int64
		bucket string
		reason string
	}
	var intercepts []interceptItem
	for _, r := range rejects {
		intercepts = append(intercepts, interceptItem{symbol: r.symbol, ts: r.ts, bucket: r.bucket, reason: r.reason})
	}
	for _, d := range details {
		if d.cls == clsRule && d.reason != "" {
			intercepts = append(intercepts, interceptItem{symbol: d.so.symbol, ts: d.so.ts, bucket: d.so.bucket, reason: d.reason})
		}
	}
	ih := map[string]*interceptStat{}
	var ihOrder []string
	ihSkipped := 0
	var ihDetails []interceptDetail
	for _, it := range intercepts {
		k5, ok := k5Cache[it.symbol]
		if !ok {
			ihSkipped++
			continue
		}
		idx := -1
		for z := 0; z < len(k5); z++ {
			if k5[z].openTime == it.ts {
				idx = z
				break
			}
		}
		if idx < 0 {
			ihSkipped++
			continue
		}
		pnl, exit, closed, _ := replayForcedOpen(k5, idx, it.bucket)
		ihDetails = append(ihDetails, interceptDetail{reason: it.reason, symbol: it.symbol, bucket: it.bucket, ts: it.ts, pnl: pnl, exit: exit, closed: closed})
		st, ok2 := ih[it.reason]
		if !ok2 {
			st = &interceptStat{}
			ih[it.reason] = st
			ihOrder = append(ihOrder, it.reason)
		}
		st.count++
		if !closed {
			st.holding++
			continue
		}
		st.closed++
		st.pnl += pnl
		if pnl > 0 {
			st.win++
		} else {
			st.loss++
		}
	}
	if len(ihOrder) > 0 {
		ihName := func(r string) string {
			if cn, ok := ruleReasonCN[r]; ok {
				return cn
			}
			return r
		}
		fmt.Println("\n拦截健康度（反事实验证该挡：拦截=策略规则内未开；假如开仓按 D 出口规则回放）:")
		fmt.Println("挡对率=虚拟亏损占比（拦对了）；误杀率=虚拟盈利占比（拦错了，错过利润）")
		var ihRows [][]string
		var tCnt, tClosed, tHold, tWin, tLoss int
		var tPnl float64
		for _, r := range ihOrder {
			st := ih[r]
			ihRows = append(ihRows, []string{
				ihName(r), fmt.Sprintf("%d", st.count), fmt.Sprintf("%d", st.closed),
				fmt.Sprintf("%d", st.holding), fmt.Sprintf("%+.2f", st.pnl),
				fmt.Sprintf("%.1f%%", safeDiv(float64(st.loss), float64(st.closed))*100),
				fmt.Sprintf("%.1f%%", safeDiv(float64(st.win), float64(st.closed))*100),
				fmt.Sprintf("%+.2f", safeDiv(st.pnl, float64(st.closed))),
			})
			tCnt += st.count
			tClosed += st.closed
			tHold += st.holding
			tWin += st.win
			tLoss += st.loss
			tPnl += st.pnl
		}
		ihRows = append(ihRows, []string{
			"合计", fmt.Sprintf("%d", tCnt), fmt.Sprintf("%d", tClosed), fmt.Sprintf("%d", tHold),
			fmt.Sprintf("%+.2f", tPnl),
			fmt.Sprintf("%.1f%%", safeDiv(float64(tLoss), float64(tClosed))*100),
			fmt.Sprintf("%.1f%%", safeDiv(float64(tWin), float64(tClosed))*100),
			fmt.Sprintf("%+.2f", safeDiv(tPnl, float64(tClosed))),
		})
		fmt.Println(fmtTable([]string{"拦截原因", "次数", "已平仓", "持有中", "虚拟盈亏", "挡对率", "误杀率", "平均每单"}, ihRows))
		if ihSkipped > 0 {
			fmt.Printf("（%d 条因 K 线数据缺失跳过）\n", ihSkipped)
		}
		if err := writeInterceptCSV(now.Format("2006-01-02"), csvSuffix, ihDetails, ihOrder, ih); err != nil {
			log.Printf("⚠ 写拦截健康度 CSV 失败: %v", err)
		}
	}

	// ==================== 机会价值漏斗（价值口径：次数 → 钱） ====================
	// 机会价值 = 每笔可开机会按 D 出口规则回放的虚拟盈亏（理论天花板）；
	// 盈利机会 = 虚拟盈亏>0；实际捕获 = 匹配到的实际成交已平仓盈亏；
	// 捕获率 = 实际盈亏 / 机会价值；盈利捕获率 = 实际正盈亏 / 盈利机会虚拟价值。
	// 该挡的拦截机会若虚拟为负，实际盈亏可超过机会价值（规则加分）。
	ov := map[string]*ovAgg{}
	for _, name := range []string{"爆拉桶", "中间桶", "温和桶"} {
		ov[name] = &ovAgg{}
	}
	totalOv := &ovAgg{}
	ovSkipped := 0
	ovFindIdx := func(k5 []kline, ts int64) int {
		for z := 0; z < len(k5); z++ {
			if k5[z].openTime == ts {
				return z
			}
		}
		return -1
	}
	for i, so := range simOpens {
		k5, ok := k5Cache[so.symbol]
		if !ok {
			ovSkipped++
			continue
		}
		idx := ovFindIdx(k5, so.ts)
		if idx < 0 {
			ovSkipped++
			continue
		}
		pnl, _, closed, hit := replayForcedOpen(k5, idx, so.bucket)
		a := ov[so.bucket]
		a.opp++
		totalOv.opp++
		if hit {
			a.hitProfit++
			totalOv.hitProfit++
		}
		if closed {
			a.closedV++
			a.virtualVal += pnl
			totalOv.closedV++
			totalOv.virtualVal += pnl
			if pnl > 0 {
				a.profitCnt++
				a.profitVal += pnl
				totalOv.profitCnt++
				totalOv.profitVal += pnl
			}
		}
		if simActIdx[i] >= 0 {
			act := actuals[simActIdx[i]]
			a.actCnt++
			totalOv.actCnt++
			if act.Status == "CLOSED" {
				a.actClosed++
				a.actVal += act.Pnl
				totalOv.actClosed++
				totalOv.actVal += act.Pnl
				if act.Pnl > 0 {
					a.actProfit += act.Pnl
					totalOv.actProfit += act.Pnl
				}
			}
		}
	}
	fmt.Println("\n机会价值漏斗（价值口径：机会价值=每笔可开机会按 D 出口规则回放的虚拟盈亏）:")
	fmt.Println("盈利机会=虚拟盈亏>0；曾盈利=盘中曾达 +2% 浮盈；捕获率=实际盈亏/机会价值；盈利捕获率=实际正盈亏/盈利机会价值")
	var ovRows [][]string
	ovAppend := func(name string, a *ovAgg) {
		capRate := "--"
		if a.virtualVal > 0 {
			capRate = fmt.Sprintf("%.1f%%", a.actVal/a.virtualVal*100)
		}
		profitCap := "--"
		if a.profitVal > 0 {
			profitCap = fmt.Sprintf("%.1f%%", a.actProfit/a.profitVal*100)
		}
		ovRows = append(ovRows, []string{
			name, fmt.Sprintf("%d", a.opp), fmt.Sprintf("%d", a.closedV),
			fmt.Sprintf("%+.2f", a.virtualVal), fmt.Sprintf("%d", a.profitCnt),
			fmt.Sprintf("%+.2f", a.profitVal), fmt.Sprintf("%d", a.hitProfit),
			fmt.Sprintf("%d", a.actCnt), fmt.Sprintf("%d", a.actClosed),
			fmt.Sprintf("%+.2f", a.actVal), capRate, profitCap,
		})
	}
	for _, name := range []string{"爆拉桶", "中间桶", "温和桶"} {
		ovAppend(name, ov[name])
	}
	ovAppend("合计", totalOv)
	fmt.Println(fmtTable([]string{"桶", "机会数", "虚拟已平仓", "机会价值", "盈利机会", "盈利价值", "曾盈利机会", "实际成交", "实际已平仓", "实际盈亏", "捕获率", "盈利捕获率"}, ovRows))
	if ovSkipped > 0 {
		fmt.Printf("（%d 条机会因 K 线数据缺失跳过）\n", ovSkipped)
	}

	// 漏掉的肉（未成交机会按归因类别拆账）
	lossByCls := map[string]*lossAgg{}
	for _, c := range []string{clsActivation, clsOrderFail, clsBalance, clsDataGap, clsSignalRace, clsOffline, clsUnknown, clsRule} {
		lossByCls[c] = &lossAgg{}
	}
	for i, so := range simOpens {
		cls := details[i].cls
		a, ok := lossByCls[cls]
		if !ok || simActIdx[i] >= 0 {
			continue
		}
		k5, ok2 := k5Cache[so.symbol]
		if !ok2 {
			continue
		}
		idx := ovFindIdx(k5, so.ts)
		if idx < 0 {
			continue
		}
		pnl, _, closed, _ := replayForcedOpen(k5, idx, so.bucket)
		a.cnt++
		if !closed {
			continue
		}
		a.closedV++
		a.val += pnl
		if pnl > 0 {
			a.miss += pnl
		} else {
			a.dodge += pnl
		}
	}
	fmt.Println("\n漏掉的肉（未成交机会按归因拆账：漏掉盈利=虚拟为正但没吃到；躲过亏损=虚拟为负但没亏）:")
	var lr [][]string
	for _, c := range []string{clsActivation, clsOrderFail, clsBalance, clsDataGap, clsSignalRace, clsOffline, clsUnknown, clsRule} {
		a := lossByCls[c]
		if a.cnt == 0 {
			continue
		}
		lr = append(lr, []string{c, fmt.Sprintf("%d", a.cnt), fmt.Sprintf("%d", a.closedV),
			fmt.Sprintf("%+.2f", a.val), fmt.Sprintf("%+.2f", a.miss), fmt.Sprintf("%+.2f", a.dodge)})
	}
	if len(lr) > 0 {
		fmt.Println(fmtTable([]string{"类别", "机会数", "已平仓", "虚拟盈亏", "漏掉盈利", "躲过亏损"}, lr))
	}
	if err := writeOpportunityValueCSV(now.Format("2006-01-02"), csvSuffix, simOpens, details, simActIdx, actuals, k5Cache, ov, totalOv, lossByCls); err != nil {
		log.Printf("⚠ 写机会价值 CSV 失败: %v", err)
	}

	// 明细样例（完整明细见 CSV）
	fmt.Println("\n明细样例（完整明细见 market_data/analysis_<date>.csv，每小时自动更新）:")
	shown := map[string]int{}
	for _, r := range rejects {
		if shown[r.reason] >= 5 {
			continue
		}
		shown[r.reason]++
		fmt.Printf("  [拦截-该挡] #%03d %-12s %s %s\n", r.seq, r.symbol, r.timeStr, r.bucket)
	}
	clsShown := map[string]int{}
	for _, d := range details {
		if d.cls == clsFill || clsShown[d.cls] >= 8 {
			continue
		}
		clsShown[d.cls]++
		tag := "首仓"
		if d.so.addOn {
			tag = "追单"
		}
		fmt.Printf("  [%s] %-12s %s %s %s\n", d.cls, d.so.symbol, time.UnixMilli(d.so.ts).In(beijing).Format("15:04"), d.so.bucket, tag)
	}

	// 完整明细写 CSV（每小时计划任务自动覆盖更新）
	date := now.Format("2006-01-02")
	if err := writeAnalysisCSV(date, csvSuffix, rejects, details); err != nil {
		log.Printf("⚠ 写分析明细 CSV 失败: %v", err)
	}

	// 写入 daily_summaries（type=strategy），供前端"每日策略总结"页展示
	// --bucket1m 为对比实验：不覆盖标准 5m 每日总结
	if clientDB != "" && !use1m {
		rejectList := make([]map[string]interface{}, 0, len(rejects))
		for _, r := range rejects {
			rejectList = append(rejectList, map[string]interface{}{
				"symbol": r.symbol, "time": r.timeStr, "bucket": r.bucket, "reason": r.reason, "seq": r.seq,
			})
		}
		detailList := make([]map[string]interface{}, 0, len(details))
		for _, d := range details {
			detailList = append(detailList, map[string]interface{}{
				"symbol": d.so.symbol, "time": time.UnixMilli(d.so.ts).In(beijing).Format("15:04"),
				"bucket": d.so.bucket, "addOn": d.so.addOn, "cls": d.cls, "why": d.why,
			})
		}
		bucketList := make([]map[string]interface{}, 0, 4)
		for _, name := range []string{"爆拉桶", "中间桶", "温和桶"} {
			a := aggs[name]
			opp := a.firstOpp + a.addonOpp
			fill := a.firstFill + a.addonFill
			conv := 0.0
			if opp > 0 {
				conv = float64(fill) / float64(opp) * 100
			}
			bucketList = append(bucketList, map[string]interface{}{
				"bucket": name, "opportunity": opp, "actual": fill,
				"actualPnl": round2(actualPnlByBucket[name]), "conversion": round2(conv),
				"first": a.firstOpp, "actualFirst": a.firstFill,
				"addon": a.addonOpp, "actualAddon": a.addonFill,
				"rule": a.firstRule + a.addonRule, "loss": a.firstLoss + a.addonLoss,
				"offline": a.firstOff + a.addonOff, "unknown": a.firstUnk + a.addonUnk,
			})
		}
		totalOpp := totalAgg.firstOpp + totalAgg.addonOpp
		totalFill := totalAgg.firstFill + totalAgg.addonFill
		totalConv := 0.0
		if totalOpp > 0 {
			totalConv = float64(totalFill) / float64(totalOpp) * 100
		}
		bucketList = append(bucketList, map[string]interface{}{
			"bucket": "合计", "opportunity": totalOpp, "actual": totalFill,
			"actualPnl": round2(actualPnlByBucket["爆拉桶"] + actualPnlByBucket["中间桶"] + actualPnlByBucket["温和桶"]),
			"conversion": round2(totalConv),
			"first": totalAgg.firstOpp, "actualFirst": totalAgg.firstFill,
			"addon": totalAgg.addonOpp, "actualAddon": totalAgg.addonFill,
			"rule": totalAgg.firstRule + totalAgg.addonRule, "loss": totalAgg.firstLoss + totalAgg.addonLoss,
			"offline": totalAgg.firstOff + totalAgg.addonOff, "unknown": totalAgg.firstUnk + totalAgg.addonUnk,
		})
		sim := map[string]interface{}{
			"signals": totalSignals, "opens": totalOpens, "addons": totalAddons,
			"closed": len(allClosed), "winRate": round2(safeDiv(float64(wins), float64(n))*100),
			"pnl": round2(dayPnl), "avg": round2(safeDiv(dayPnl, float64(n))),
			"stop": reasonCnt["STOP_LOSS"], "trail": reasonCnt["TRAILING"], "maxhold": reasonCnt["MAX_HOLD"],
			"addonClosed": addOnClosed, "extraOpens": extraOpens,
			"rejects": map[string]interface{}{
				"maxpos": rejectCnt["maxpos"], "cooldown": rejectCnt["cooldown"],
				"noActive": rejectCnt["no_active"], "addonLimit": rejectCnt["addon_limit"],
			},
			"lossClass": clsTotal,
		}
		// 拦截健康度（反事实回放）写入每日总结，供前端「拦截健康度」板块展示
		ihJSON := map[string]interface{}{}
		for _, r := range ihOrder {
			st := ih[r]
			ihJSON[r] = map[string]interface{}{
				"count": st.count, "closed": st.closed, "holding": st.holding,
				"pnl": round2(st.pnl),
				"blockCorrect": round2(safeDiv(float64(st.loss), float64(st.closed))*100),
				"missProfit":   round2(safeDiv(float64(st.win), float64(st.closed))*100),
				"avg":          round2(safeDiv(st.pnl, float64(st.closed))),
			}
		}
		// 机会价值漏斗（价值口径）写入每日总结，供前端「机会价值」板块展示
		ovJSON := map[string]interface{}{}
		for _, name := range []string{"爆拉桶", "中间桶", "温和桶", "合计"} {
			a := totalOv
			if name != "合计" {
				a = ov[name]
			}
			capRate := 0.0
			if a.virtualVal > 0 {
				capRate = a.actVal / a.virtualVal * 100
			}
			profitCap := 0.0
			if a.profitVal > 0 {
				profitCap = a.actProfit / a.profitVal * 100
			}
			ovJSON[name] = map[string]interface{}{
				"opp": a.opp, "closedV": a.closedV, "virtualVal": round2(a.virtualVal),
				"profitCnt": a.profitCnt, "profitVal": round2(a.profitVal), "hitProfit": a.hitProfit,
				"actCnt": a.actCnt, "actClosed": a.actClosed, "actVal": round2(a.actVal),
				"actProfit": round2(a.actProfit),
				"capRate": round2(capRate), "profitCap": round2(profitCap),
			}
		}
		lossJSON := map[string]interface{}{}
		for _, c := range []string{clsActivation, clsOrderFail, clsBalance, clsDataGap, clsSignalRace, clsOffline, clsUnknown, clsRule} {
			a := lossByCls[c]
			if a.cnt == 0 {
				continue
			}
			lossJSON[c] = map[string]interface{}{
				"cnt": a.cnt, "closedV": a.closedV, "val": round2(a.val),
				"miss": round2(a.miss), "dodge": round2(a.dodge),
			}
		}
		ovMeta := map[string]interface{}{"buckets": ovJSON, "loss": lossJSON}
		if err := writeStrategySummary(clientDB, now.Format("2006-01-02"), sim, bucketList, rejectList, detailList, ihJSON, ovMeta); err != nil {
			log.Printf("⚠ 写入策略总结失败: %v", err)
		} else {
			log.Printf("✅ 已写入每日策略总结（%s）", clientDB)
		}
	}
	if use1m {
		log.Printf("（--bucket1m 对比实验：未写 daily_summaries，CSV 已带 _1m 后缀）")
	}
	return nil
}

// writeAnalysisCSV 输出逐单拦截（该挡）与执行损耗归因明细（每小时计划任务自动覆盖更新）
func writeAnalysisCSV(date, suffix string, rejects []rejectRec, details []simDetail) error {
	path := fmt.Sprintf(`D:\0001_ba-A - 03\market_data\analysis_%s%s.csv`, date, suffix)
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := csv.NewWriter(f)
	defer w.Flush()
	w.Write([]string{"类型", "币种", "时间", "桶", "原因/说明"})
	for _, r := range rejects {
		why := "拦截-该挡（" + r.reason + "）"
		if cn, ok := ruleReasonCN[r.reason]; ok {
			why = "拦截-该挡（" + cn + "）"
		}
		w.Write([]string{"拦截", r.symbol, r.timeStr, r.bucket, why})
	}
	for _, d := range details {
		tag := "首仓"
		if d.so.addOn {
			tag = "追单"
		}
		w.Write([]string{d.cls + "-" + tag, d.so.symbol, time.UnixMilli(d.so.ts).In(beijing).Format("15:04"), d.so.bucket, d.why})
	}
	return w.Error()
}

// writeInterceptCSV 输出拦截健康度明细（逐单 + 按原因汇总，每小时自动覆盖更新）
func writeInterceptCSV(date, suffix string, details []interceptDetail, order []string, ih map[string]*interceptStat) error {
	path := fmt.Sprintf(`D:\0001_ba-A - 03\market_data\intercept_health_%s%s.csv`, date, suffix)
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := csv.NewWriter(f)
	defer w.Flush()
	w.Write([]string{"类型", "拦截原因", "币种/次数", "时间/已平仓", "桶/持有中", "虚拟盈亏", "离场原因/挡对率", "结论/误杀率"})
	for _, d := range details {
		conclusion := "持有中"
		if d.closed {
			if d.pnl > 0 {
				conclusion = "误杀(错过利润)"
			} else {
				conclusion = "挡对(躲过亏损)"
			}
		}
		w.Write([]string{"明细", d.reason, d.symbol, time.UnixMilli(d.ts).In(beijing).Format("15:04"),
			d.bucket, fmt.Sprintf("%.2f", d.pnl), d.exit, conclusion})
	}
	for _, r := range order {
		st := ih[r]
		w.Write([]string{"汇总", r, fmt.Sprintf("%d", st.count), fmt.Sprintf("%d", st.closed),
			fmt.Sprintf("%d", st.holding), fmt.Sprintf("%.2f", st.pnl),
			fmt.Sprintf("%.1f%%", safeDiv(float64(st.loss), float64(st.closed))*100),
			fmt.Sprintf("%.1f%%", safeDiv(float64(st.win), float64(st.closed))*100)})
	}
	return w.Error()
}

// writeOpportunityValueCSV 输出机会价值漏斗逐单明细 + 汇总（每小时自动覆盖更新）
func writeOpportunityValueCSV(date, suffix string, simOpens []simOpenRec, details []simDetail, simActIdx []int,
	actuals []actualTrade, k5Cache map[string][]kline, ov map[string]*ovAgg, totalOv *ovAgg, lossByCls map[string]*lossAgg) error {
	path := fmt.Sprintf(`D:\0001_ba-A - 03\market_data\opportunity_value_%s%s.csv`, date, suffix)
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := csv.NewWriter(f)
	defer w.Flush()
	w.Write([]string{"类型", "币种", "时间", "桶", "首仓/追单", "归因", "虚拟盈亏", "离场/持有", "曾盈利+2%", "是否成交", "实际盈亏"})
	for i, so := range simOpens {
		k5, ok := k5Cache[so.symbol]
		if !ok {
			continue
		}
		idx := -1
		for z := 0; z < len(k5); z++ {
			if k5[z].openTime == so.ts {
				idx = z
				break
			}
		}
		if idx < 0 {
			continue
		}
		pnl, exit, closed, hit := replayForcedOpen(k5, idx, so.bucket)
		tag := "首仓"
		if so.addOn {
			tag = "追单"
		}
		matched := "否"
		actPnl := ""
		if simActIdx[i] >= 0 {
			matched = "是"
			a := actuals[simActIdx[i]]
			if a.Status == "CLOSED" {
				actPnl = fmt.Sprintf("%.2f", a.Pnl)
			} else {
				actPnl = "持有中"
			}
		}
		closedStr := "持有中"
		if closed {
			closedStr = exit
		}
		hitStr := "否"
		if hit {
			hitStr = "是"
		}
		w.Write([]string{"明细", so.symbol, time.UnixMilli(so.ts).In(beijing).Format("15:04"),
			so.bucket, tag, details[i].cls, fmt.Sprintf("%.2f", pnl), closedStr, hitStr, matched, actPnl})
	}
	w.Write([]string{"汇总-机会价值", "桶", "机会数", "虚拟已平仓", "机会价值", "盈利机会", "盈利价值", "曾盈利", "实际成交", "实际已平仓", "实际盈亏", "捕获率", "盈利捕获率"})
	ovCSV := func(name string, a *ovAgg) {
		capRate := 0.0
		if a.virtualVal > 0 {
			capRate = a.actVal / a.virtualVal * 100
		}
		profitCap := 0.0
		if a.profitVal > 0 {
			profitCap = a.actProfit / a.profitVal * 100
		}
		w.Write([]string{"汇总-机会价值", name, fmt.Sprintf("%d", a.opp), fmt.Sprintf("%d", a.closedV),
			fmt.Sprintf("%.2f", a.virtualVal), fmt.Sprintf("%d", a.profitCnt), fmt.Sprintf("%.2f", a.profitVal),
			fmt.Sprintf("%d", a.hitProfit), fmt.Sprintf("%d", a.actCnt), fmt.Sprintf("%d", a.actClosed),
			fmt.Sprintf("%.2f", a.actVal), fmt.Sprintf("%.1f%%", capRate), fmt.Sprintf("%.1f%%", profitCap)})
	}
	for _, name := range []string{"爆拉桶", "中间桶", "温和桶"} {
		ovCSV(name, ov[name])
	}
	ovCSV("合计", totalOv)
	w.Write([]string{"汇总-漏掉的肉", "类别", "机会数", "已平仓", "虚拟盈亏", "漏掉盈利", "躲过亏损"})
	for _, c := range []string{clsActivation, clsOrderFail, clsBalance, clsDataGap, clsSignalRace, clsOffline, clsUnknown, clsRule} {
		a := lossByCls[c]
		if a.cnt == 0 {
			continue
		}
		w.Write([]string{"汇总-漏掉的肉", c, fmt.Sprintf("%d", a.cnt), fmt.Sprintf("%d", a.closedV),
			fmt.Sprintf("%.2f", a.val), fmt.Sprintf("%.2f", a.miss), fmt.Sprintf("%.2f", a.dodge)})
	}
	return w.Error()
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }

func safeDiv(a, b float64) float64 {
	if b == 0 {
		return 0
	}
	return a / b
}

// writeStrategySummary 将策略口径模拟、漏斗归因、逐单明细、拦截健康度与机会价值写入客户端库 daily_summaries（type=strategy）
func writeStrategySummary(clientDB, date string, sim map[string]interface{}, buckets []map[string]interface{}, rejects, details []map[string]interface{}, interceptHealth, opportunityValue map[string]interface{}) error {
	db, err := sql.Open("sqlite3", "file:"+clientDB+"?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		return err
	}
	defer db.Close()
	meta, _ := json.Marshal(map[string]interface{}{
		"sim": sim, "buckets": buckets, "rejects": rejects, "details": details,
		"interceptHealth": interceptHealth, "opportunityValue": opportunityValue,
	})
	now := time.Now().In(beijing)
	var id int64
	err = db.QueryRow(`SELECT id FROM daily_summaries WHERE mode='SIMULATION' AND summary_date=? AND summary_type='strategy' AND deleted_at=0`, date).Scan(&id)
	if err == nil {
		_, err = db.Exec(`UPDATE daily_summaries SET feature_json=?, updated_at=? WHERE id=?`, string(meta), now.UnixMilli(), id)
	} else {
		_, err = db.Exec(`INSERT INTO daily_summaries
			(mode, summary_date, summary_type, market_notes, today_pnl, win_rate, trade_count, rating, feature_json, created_at, updated_at)
			VALUES ('SIMULATION',?, 'strategy', '策略口径当日模拟（自动生成）',0,0,0,0,?,?,?)`,
			date, string(meta), now.UnixMilli(), now.UnixMilli())
	}
	return err
}
