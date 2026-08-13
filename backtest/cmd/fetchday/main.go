// Command fetchday 抓取指定日期的实盘 5m K 线（+ 前序预热），
// 按回测引擎的 CSV 格式写入 data 目录，供 backtest -start/-end 单日重放。
//
// 用法:
//
//	go run ./cmd/fetchday -date 2026-08-13 -out data_replay -proxy http://127.0.0.1:10808
package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

var beijing = time.FixedZone("CST", 8*3600)

const fapiBase = "https://fapi.binance.com"

func main() {
	dateStr := flag.String("date", "", "目标日期 YYYY-MM-DD（北京时间）")
	outDir := flag.String("out", "data_replay", "输出目录（每币一个 {SYMBOL}.csv）")
	proxy := flag.String("proxy", os.Getenv("HTTPS_PROXY"), "HTTP 代理地址")
	minVol := flag.Float64("minvol", 20000000, "机会池 24h 成交额下限 USDT（默认 2000 万，与 D 策略一致）")
	maxSym := flag.Int("max", 160, "池内最多处理的币数（按成交额降序）")
	workers := flag.Int("workers", 8, "并发拉取数")
	flag.Parse()
	if *dateStr == "" {
		log.Fatal("必须指定 -date YYYY-MM-DD")
	}
	t, err := time.ParseInLocation("2006-01-02", *dateStr, beijing)
	if err != nil {
		log.Fatalf("日期格式错误: %v", err)
	}
	dayStart := t.UnixMilli()
	dayEnd := dayStart + 24*60*60*1000
	warmup := int64(24 * 60 * 60 * 1000)

	client := &http.Client{Timeout: 30 * time.Second}
	if *proxy != "" {
		pu, err := url.Parse(*proxy)
		if err != nil {
			log.Fatalf("代理格式错误: %v", err)
		}
		client.Transport = &http.Transport{Proxy: http.ProxyURL(pu)}
	}
	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		log.Fatalf("创建输出目录失败: %v", err)
	}

	pool := fetchPool(client, *minVol, *maxSym)
	log.Printf("机会池: %d 个币（24h 成交额 ≥ %.0f）", len(pool), *minVol)

	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, *workers)
	written := 0
	for _, sym := range pool {
		wg.Add(1)
		sem <- struct{}{}
		go func(s string) {
			defer wg.Done()
			defer func() { <-sem }()
			bars, err := fetchKlinesRange(client, s, dayStart-warmup, dayEnd)
			if err != nil {
				log.Printf("⚠ %s 拉取失败: %v", s, err)
				return
			}
			if len(bars) == 0 {
				return
			}
			path := filepath.Join(*outDir, s+".csv")
			f, err := os.Create(path)
			if err != nil {
				log.Printf("⚠ %s 写文件失败: %v", s, err)
				return
			}
			w := csv.NewWriter(f)
			w.Write([]string{"open_time", "open", "high", "low", "close", "volume", "close_time", "quote_volume", "trades", "taker_buy_base"})
			for _, b := range bars {
				w.Write([]string{
					strconv.FormatInt(b.openTime, 10),
					strconv.FormatFloat(b.open, 'f', -1, 64),
					strconv.FormatFloat(b.high, 'f', -1, 64),
					strconv.FormatFloat(b.low, 'f', -1, 64),
					strconv.FormatFloat(b.close, 'f', -1, 64),
					strconv.FormatFloat(b.volume, 'f', -1, 64),
					strconv.FormatInt(b.closeTime, 10),
					strconv.FormatFloat(b.quoteVolume, 'f', -1, 64),
					strconv.FormatInt(b.trades, 10),
					strconv.FormatFloat(b.takerBuyBase, 'f', -1, 64),
				})
			}
			w.Flush()
			f.Close()
			mu.Lock()
			written++
			mu.Unlock()
		}(sym)
	}
	wg.Wait()
	log.Printf("✅ 已写入 %d 个币的 5m K 线到 %s（%s 全天 + 24h 预热）", written, *outDir, *dateStr)
}

type ticker24 struct {
	Symbol      string  `json:"symbol"`
	QuoteVolume float64 `json:"quoteVolume"`
}

// UnmarshalJSON 兼容币安 ticker 接口的字符串数字字段。
func (t *ticker24) UnmarshalJSON(data []byte) error {
	var raw struct {
		Symbol      string          `json:"symbol"`
		QuoteVolume json.RawMessage `json:"quoteVolume"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	t.Symbol = raw.Symbol
	var s string
	if err := json.Unmarshal(raw.QuoteVolume, &s); err == nil {
		if v, perr := strconv.ParseFloat(s, 64); perr == nil {
			t.QuoteVolume = v
		}
	} else {
		_ = json.Unmarshal(raw.QuoteVolume, &t.QuoteVolume)
	}
	return nil
}

func fetchPool(client *http.Client, minVol float64, maxSym int) []string {
	resp, err := client.Get(fapiBase + "/fapi/v1/ticker/24hr")
	if err != nil {
		log.Fatalf("拉取行情失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		log.Fatalf("行情接口 HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var all []ticker24
	if err := json.NewDecoder(resp.Body).Decode(&all); err != nil {
		log.Fatalf("解析行情失败: %v", err)
	}
	var pool []ticker24
	for _, t := range all {
		if t.QuoteVolume >= minVol {
			pool = append(pool, t)
		}
	}
	sort.Slice(pool, func(i, j int) bool { return pool[i].QuoteVolume > pool[j].QuoteVolume })
	if len(pool) > maxSym {
		pool = pool[:maxSym]
	}
	out := make([]string, 0, len(pool))
	for _, t := range pool {
		out = append(out, t.Symbol)
	}
	return out
}

type klineRow struct {
	openTime     int64
	open, high, low, close, volume, quoteVolume, takerBuyBase float64
	closeTime    int64
	trades       int64
}

func fetchKlinesRange(client *http.Client, symbol string, startMs, endMs int64) ([]klineRow, error) {
	u := fmt.Sprintf("%s/fapi/v1/klines?symbol=%s&interval=5m&startTime=%d&endTime=%d&limit=1500",
		fapiBase, url.QueryEscape(symbol), startMs, endMs)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var raw [][]json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, err
	}
	out := make([]klineRow, 0, len(raw))
	for _, row := range raw {
		if len(row) < 11 {
			continue
		}
		var k klineRow
		if err := json.Unmarshal(row[0], &k.openTime); err != nil {
			continue
		}
		if err := json.Unmarshal(row[6], &k.closeTime); err != nil {
			continue
		}
		if err := json.Unmarshal(row[8], &k.trades); err != nil {
			continue
		}
		nums := []struct {
			dst *float64
			idx int
		}{
			{&k.open, 1}, {&k.high, 2}, {&k.low, 3}, {&k.close, 4},
			{&k.volume, 5}, {&k.quoteVolume, 7}, {&k.takerBuyBase, 9},
		}
		for _, n := range nums {
			var s string
			if err := json.Unmarshal(row[n.idx], &s); err != nil {
				continue
			}
			if v, err := strconv.ParseFloat(s, 64); err == nil {
				*n.dst = v
			}
		}
		out = append(out, k)
	}
	return out, nil
}
