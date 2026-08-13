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
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
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
