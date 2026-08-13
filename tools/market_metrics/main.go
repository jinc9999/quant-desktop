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
		for _, k := range k5 {
			if k.openTime < dayStart || k.open <= 0 {
				continue
			}
			if (k.close-k.open)/k.open*100 >= 2.5 {
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
				{"异动机会", fmt.Sprintf("%d 个币 / %d 次", m.OpportunityCount, m.OpportunityTotal), "15m单根涨≥3% 的次数（策略的肉）"},
				{"5m爆拉", fmt.Sprintf("%d 次", m.BurstTotal), "5m单根涨≥2.5%（智慧版1.5倍机会）"},
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
			err = analyzeBurst(*analyzeProxy)
		} else {
			err = analyzeStrategy(*analyzeProxy, *analyzeDB)
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

// analyzeBurst 复盘分析（--raw）：今天每根"5m 单根实体≥2.5%"的爆拉之后，
// 价格续涨情况 / 15m 累计是否突破 3% / 继续上涨幅度分布。
func analyzeBurst(proxy string) error {
	client := httpClient(proxy)
	tickers, err := fetchTickers(client)
	if err != nil {
		return fmt.Errorf("拉取行情失败: %w", err)
	}
	now := time.Now().In(beijing)
	dayStart := beijingDayStartUTC(now)

	pool := make([]ticker24, 0, len(tickers))
	for _, t := range tickers {
		if t.QuoteVolume >= minPoolVol {
			pool = append(pool, t)
		}
	}

	type event struct {
		symbol       string
		burstGain    float64 // 爆拉 5m 实体 %
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
			if k.openTime < dayStart || k.open <= 0 {
				continue
			}
			burst := (k.close - k.open) / k.open * 100
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
		{"爆拉事件", fmt.Sprintf("%d 币 / %d 次", len(symbols), n), "5m 单根实体≥2.5%"},
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

// max5mEntity 开仓时当前 15m 周期内最大 5m 实体涨幅（桶判定口径）
func max5mEntity(k5 []kline, j int) float64 {
	mx := 0.0
	for z := j; z >= 0 && z > j-3; z-- {
		if k5[z].open > 0 {
			e := (k5[z].close - k5[z].open) / k5[z].open * 100
			if e > mx {
				mx = e
			}
		}
	}
	return mx
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
}

// loadActualTrades 读取客户端库当天实际开仓记录（含未平仓），用于"机会 vs 实际"对比。
func loadActualTrades(clientDB string, dayStart int64) ([]actualTrade, error) {
	db, err := sql.Open("sqlite3", "file:"+clientDB+"?mode=ro")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.Query(`SELECT symbol, ifnull(realized_pnl,0), opened_at, status
		FROM positions WHERE opened_at >= ?`, dayStart)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []actualTrade
	for rows.Next() {
		var t actualTrade
		var st string
		if err := rows.Scan(&t.Symbol, &t.Pnl, &t.Opened, &st); err != nil {
			continue
		}
		t.Status = st
		out = append(out, t)
	}
	return out, nil
}

// ==================== 策略口径模拟（analyze 默认）====================
// 按 D 策略真实规则回放当天 5m 数据：
//   入仓: 15m 累计(收盘 vs 周期开盘) ≥3%
//   激活: 价格 ≥ 入场价×1.02 → 移动止盈开始（跟踪回调 3%）
//   追单: 已激活且再次命中信号 → 追加独立单（同币最多 1+2=3 仓）
//   平仓: 止损 -3% / 跟踪止盈(激活后从最高回撤3%) / 超时 180 分钟(36 根)
//   冷却: 移动止盈平仓后 15 分钟可再入，止损/超时后 30 分钟
//   名义: 每仓 100U（10U 保证金 × 10x，未按 5m 爆拉分桶放大，口径标注）

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

func analyzeStrategy(proxy, clientDB string) error {
	client := httpClient(proxy)
	tickers, err := fetchTickers(client)
	if err != nil {
		return fmt.Errorf("拉取行情失败: %w", err)
	}
	now := time.Now().In(beijing)
	dayStart := beijingDayStartUTC(now)

	pool := make([]ticker24, 0, len(tickers))
	for _, t := range tickers {
		if t.QuoteVolume >= minPoolVol {
			pool = append(pool, t)
		}
	}

	const (
		nominal    = 100.0 // 每仓名义 100U
		gainReq    = 3.0
		slPct      = 0.03
		actPct     = 0.02
		cbPct      = 0.03
		maxBars    = 36
		cdTrail    = int64(15 * 60 * 1000)
		cdStop     = int64(30 * 60 * 1000)
		maxAddOn   = 2
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
		actuals, _ = loadActualTrades(clientDB, dayStart)
	}
	actualByBucket := map[string]int{"爆拉桶": 0, "中间桶": 0, "温和桶": 0}
	actualPnlByBucket := map[string]float64{"爆拉桶": 0, "中间桶": 0, "温和桶": 0}
	actualClosedByBucket := map[string]int{"爆拉桶": 0, "中间桶": 0, "温和桶": 0}
	k5Cache := map[string][]kline{}

	for i, t := range pool {
		if i >= 160 {
			break
		}
		k5, err5 := fetchKlines(client, t.Symbol, "5m", 288)
		if err5 != nil || len(k5) < 40 {
			continue
		}
		k5Cache[t.Symbol] = k5
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
				m5 = max5mEntity(k5, j)
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
					rejects = append(rejects, rejectRec{symbol: t.Symbol, timeStr: time.UnixMilli(k.openTime).In(beijing).Format("15:04"), bucket: bkt, reason: "maxpos", seq: seq})
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
							rejects = append(rejects, rejectRec{symbol: t.Symbol, timeStr: time.UnixMilli(k.openTime).In(beijing).Format("15:04"), bucket: bkt, reason: "no_active", seq: seq})
						} else {
							rejects = append(rejects, rejectRec{symbol: t.Symbol, timeStr: time.UnixMilli(k.openTime).In(beijing).Format("15:04"), bucket: bkt, reason: "addon_limit", seq: seq})
						}
					}
				}
			} else if signal {
				rejects = append(rejects, rejectRec{symbol: t.Symbol, timeStr: time.UnixMilli(k.openTime).In(beijing).Format("15:04"), bucket: bucketOf(m5), reason: "cooldown", seq: seq})
			}
		}
	}

	// 实际交易回放桶（复用已拉 K 线，缺则补拉）
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
		bkt := bucketOf(max5mEntity(k5, idx))
		actualByBucket[bkt]++
		if a.Status == "CLOSED" {
			actualClosedByBucket[bkt]++
			actualPnlByBucket[bkt] += a.Pnl
		}
	}

	if len(allClosed) == 0 {
		fmt.Println("今天按策略口径暂无完整平仓样本")
		return nil
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
	fmt.Println("\n⚠ 口径：本模拟按 5m 收盘/高低价近似回放，未含手续费滑点、未按 5m 爆拉分桶放大仓位；用于复盘当日策略环境，非实盘对账。")

	// 三桶分析（按开仓时 5m 爆拉分桶）
	fmt.Println("\n三桶分析 · 可开仓机会 vs 实际（可开仓机会=策略规则下能开的动作数（含追单、含全局10仓上限）；实际=客户端库当天开仓；少做=可开-实际）:")
	var bucketRows [][]string
	var bucketList []map[string]interface{}
	for _, name := range []string{"爆拉桶", "中间桶", "温和桶"} {
		b := buckets[name]
		opportunity := b.opens // 可开仓机会 = 策略规则模拟能开的动作数（含追单）
		actual := actualByBucket[name]
		missed := opportunity - actual
		if missed < 0 {
			missed = 0
		}
		conv := 0.0
		if opportunity > 0 {
			conv = float64(actual) / float64(opportunity) * 100
		}
		bucketRows = append(bucketRows, []string{
			name,
			fmt.Sprintf("%d", opportunity),
			fmt.Sprintf("%d", actual),
			fmt.Sprintf("%+.2f U", actualPnlByBucket[name]),
			fmt.Sprintf("%d", missed),
			fmt.Sprintf("%.1f%%", conv),
		})
		bucketList = append(bucketList, map[string]interface{}{
			"bucket": name, "opportunity": opportunity, "actual": actual,
			"actualPnl": round2(actualPnlByBucket[name]), "missed": missed, "conversion": round2(conv),
		})
	}
	// 合计行
	tOpp, tAct, tPnl, tMiss := 0, 0, 0.0, 0
	tConv := 0.0
	for _, bkt := range []string{"爆拉桶", "中间桶", "温和桶"} {
		b := buckets[bkt]
		tOpp += b.opens
		tAct += actualByBucket[bkt]
		tPnl += actualPnlByBucket[bkt]
		tMiss += b.opens - actualByBucket[bkt]
	}
	if tOpp > 0 {
		tConv = float64(tAct) / float64(tOpp) * 100
	}
	bucketRows = append(bucketRows, []string{
		"合计", fmt.Sprintf("%d", tOpp), fmt.Sprintf("%d", tAct),
		fmt.Sprintf("%+.2f U", tPnl), fmt.Sprintf("%d", tMiss), fmt.Sprintf("%.1f%%", tConv),
	})
	bucketList = append(bucketList, map[string]interface{}{
		"bucket": "合计", "opportunity": tOpp, "actual": tAct,
		"actualPnl": round2(tPnl), "missed": tMiss, "conversion": round2(tConv),
	})
	fmt.Println(fmtTable([]string{"桶", "可开仓机会", "实际开仓", "实际盈亏", "少做", "转化率"}, bucketRows))

	// ==================== 逐单根因明细 ====================
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
	fmt.Println("\n逐单拦截明细（策略规则内未开仓的信号，按原因）:")
	var rejectRows [][]string
	for _, k := range []string{"maxpos", "cooldown", "no_active", "addon_limit"} {
		rejectRows = append(rejectRows, []string{rejectNames[k], fmt.Sprintf("%d", rejectCnt[k])})
	}
	fmt.Println(fmtTable([]string{"拦截原因", "次数"}, rejectRows))

	// 执行损耗差集：按币按序号精确匹配（同币第 n 个模拟开仓对应第 n 个实际开仓，
	// 模拟多出的单即"可开但实际未成交"，demo/tick/零星失败等执行损耗）
	simBySym := map[string][]simOpenRec{}
	for _, so := range simOpens {
		simBySym[so.symbol] = append(simBySym[so.symbol], so)
	}
	actBySym := map[string][]actualTrade{}
	for _, a := range actuals {
		actBySym[a.Symbol] = append(actBySym[a.Symbol], a)
	}
	var gap []simOpenRec
	for sym, sims := range simBySym {
		acts := actBySym[sym]
		for i, so := range sims {
			if i >= len(acts) {
				gap = append(gap, so)
			}
		}
	}
	gapCnt := map[string]int{}
	for _, g := range gap {
		gapCnt[g.bucket]++
	}
	fmt.Println("\n执行损耗明细（模拟规则可开但实际未成交，demo/tick粒度/零星失败）:")
	var gapRows [][]string
	for _, bkt := range []string{"爆拉桶", "中间桶", "温和桶"} {
		gapRows = append(gapRows, []string{bkt, fmt.Sprintf("%d", gapCnt[bkt])})
	}
	fmt.Println(fmtTable([]string{"桶", "执行损耗单数"}, gapRows))

	// 明细样例（每原因前 8 条）
	fmt.Println("\n明细样例（完整明细见 market_data/analysis_<date>.csv）:")
	shown := map[string]int{}
	for _, r := range rejects {
		if shown[r.reason] >= 8 {
			continue
		}
		shown[r.reason]++
		fmt.Printf("  [%s] #%03d %-12s %s %s\n", rejectNames[r.reason], r.seq, r.symbol, r.timeStr, r.bucket)
	}
	gapBySym := map[string]int{}
	for _, g := range gap {
		gapBySym[g.symbol]++
	}
	gapShown := 0
	for _, g := range gap {
		if gapShown >= 15 {
			break
		}
		gapShown++
		seq := gapBySym[g.symbol] - countAfter(gap, g) + 1
		tag := "首仓"
		if g.addOn {
			tag = "追单"
		}
		fmt.Printf("  [执行损耗] %-12s #%03d %s %s %s\n", g.symbol, seq, time.UnixMilli(g.ts).In(beijing).Format("15:04"), g.bucket, tag)
	}

	// 完整明细写 CSV
	date := now.Format("2006-01-02")
	if err := writeAnalysisCSV(date, rejects, gap); err != nil {
		log.Printf("⚠ 写分析明细 CSV 失败: %v", err)
	}

	// 写入 daily_summaries（type=strategy），供前端"每日策略总结"页展示
	if clientDB != "" {
		rejectList := make([]map[string]interface{}, 0, len(rejects))
		for _, r := range rejects {
			rejectList = append(rejectList, map[string]interface{}{
				"symbol": r.symbol, "time": r.timeStr, "bucket": r.bucket, "reason": r.reason, "seq": r.seq,
			})
		}
		gapSeq := map[string]int{}
		gapList := make([]map[string]interface{}, 0, len(gap))
		for _, g := range gap {
			gapSeq[g.symbol]++
			gapList = append(gapList, map[string]interface{}{
				"symbol": g.symbol, "time": time.UnixMilli(g.ts).In(beijing).Format("15:04"),
				"bucket": g.bucket, "seq": gapSeq[g.symbol], "addOn": g.addOn,
			})
		}
		sim := map[string]interface{}{
			"signals": totalSignals, "opens": totalOpens, "addons": totalAddons,
			"closed": len(allClosed), "winRate": round2(safeDiv(float64(wins), float64(n))*100),
			"pnl": round2(dayPnl), "avg": round2(safeDiv(dayPnl, float64(n))),
			"stop": reasonCnt["STOP_LOSS"], "trail": reasonCnt["TRAILING"], "maxhold": reasonCnt["MAX_HOLD"],
			"addonClosed": addOnClosed,
			"rejects": map[string]interface{}{
				"maxpos": rejectCnt["maxpos"], "cooldown": rejectCnt["cooldown"],
				"noActive": rejectCnt["no_active"], "addonLimit": rejectCnt["addon_limit"],
			},
			"gap": gapCnt,
		}
		if err := writeStrategySummary(clientDB, sim, bucketList, rejectList, gapList); err != nil {
			log.Printf("⚠ 写入策略总结失败: %v", err)
		} else {
			log.Printf("✅ 已写入每日策略总结（%s）", clientDB)
		}
	}
	return nil
}

// writeAnalysisCSV 输出逐单拦截与执行损耗明细（可打开查看每一单）
func writeAnalysisCSV(date string, rejects []rejectRec, gap []simOpenRec) error {
	path := fmt.Sprintf(`D:\0001_ba-A - 03\market_data\analysis_%s.csv`, date)
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := csv.NewWriter(f)
	defer w.Flush()
	w.Write([]string{"类型", "币种", "时间", "桶", "原因/说明"})
	for _, r := range rejects {
		w.Write([]string{"拦截", r.symbol, r.timeStr, r.bucket, r.reason})
	}
	for _, g := range gap {
		why := "执行损耗(模拟可开实际未成交)"
		if g.addOn {
			why += "-追单"
		}
		w.Write([]string{"执行损耗", g.symbol, time.UnixMilli(g.ts).In(beijing).Format("15:04"), g.bucket, why})
	}
	return w.Error()
}

func countAfter(list []simOpenRec, target simOpenRec) int {
	n := 0
	for _, g := range list {
		if g.symbol == target.symbol && g.ts > target.ts {
			n++
		}
	}
	return n
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }

func safeDiv(a, b float64) float64 {
	if b == 0 {
		return 0
	}
	return a / b
}

// writeStrategySummary 将策略口径模拟与三桶分析写入客户端库 daily_summaries（type=strategy）
func writeStrategySummary(clientDB string, sim map[string]interface{}, buckets []map[string]interface{}, rejects, gap []map[string]interface{}) error {
	db, err := sql.Open("sqlite3", "file:"+clientDB+"?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		return err
	}
	defer db.Close()
	meta, _ := json.Marshal(map[string]interface{}{
		"sim": sim, "buckets": buckets, "rejects": rejects, "gap": gap,
	})
	now := time.Now().In(beijing)
	date := now.Format("2006-01-02")
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
