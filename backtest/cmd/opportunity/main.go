// Command opportunity 统计币安市场今日「涨幅超过阈值」的机会次数（纯行情统计，与策略无关）。
//
// 三种口径（同币多次均计数）：
//   cycle5m  : 每根已收盘 5m，5m收盘 vs 当前15m周期开盘 >阈值（与策略信号同款公式）
//   cycle15m : 每根已收盘 15m，15m收盘 vs 15m开盘 >阈值
//   bar5m    : 每根已收盘 5m，5m收盘 vs 5m开盘 >阈值
//
// 用法:
//   go run ./cmd/opportunity -date 2026-08-14 -gain 5 -proxy http://127.0.0.1:10808
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

var beijing = time.FixedZone("CST", 8*3600)

const fapiBase = "https://fapi.binance.com"

type ticker24 struct {
	Symbol      string
	QuoteVolume float64
}

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
	}
	return nil
}

type kline struct {
	openTime int64
	open     float64
	close    float64
}

func main() {
	dateStr := flag.String("date", "", "统计日期 YYYY-MM-DD（默认今天，北京时间）")
	gain := flag.Float64("gain", 5, "涨幅阈值 %%")
	minVol := flag.Float64("minvol", 20000000, "池子 24h 成交额下限")
	maxSym := flag.Int("max", 160, "池内最多币数")
	proxy := flag.String("proxy", "", "HTTP 代理")
	workers := flag.Int("workers", 8, "并发")
	flag.Parse()

	now := time.Now().In(beijing)
	if *dateStr != "" {
		t, err := time.ParseInLocation("2006-01-02", *dateStr, beijing)
		if err != nil {
			log.Fatalf("日期格式错误: %v", err)
		}
		now = t
	}
	dayStart := now.UnixMilli()
	dayEnd := dayStart + 24*60*60*1000

	client := &http.Client{Timeout: 30 * time.Second}
	if *proxy != "" {
		pu, err := url.Parse(*proxy)
		if err != nil {
			log.Fatalf("代理错误: %v", err)
		}
		client.Transport = &http.Transport{Proxy: http.ProxyURL(pu)}
	}

	pool := fetchPool(client, *minVol, *maxSym)
	log.Printf("池子 %d 个币（24h成交额≥%.0f）", len(pool), *minVol)

	var mu sync.Mutex
	cycle5m := map[string]int{}
	cycle15m := map[string]int{}
	bar5m := map[string]int{}
	var wg sync.WaitGroup
	sem := make(chan struct{}, *workers)
	for _, sym := range pool {
		wg.Add(1)
		sem <- struct{}{}
		go func(s string) {
			defer wg.Done()
			defer func() { <-sem }()
			bars, err := fetchKlinesRange(client, s, dayStart, dayEnd)
			if err != nil || len(bars) == 0 {
				return
			}
			c5, c15, b5 := countGains(bars, *gain)
			mu.Lock()
			if c5 > 0 {
				cycle5m[s] = c5
			}
			if c15 > 0 {
				cycle15m[s] = c15
			}
			if b5 > 0 {
				bar5m[s] = b5
			}
			mu.Unlock()
		}(sym)
	}
	wg.Wait()

	printStat("口径A 5m收盘 vs 15m周期开盘 >阈值（与策略信号同款公式）", cycle5m)
	printStat("口径B 15m收盘 vs 15m开盘 >阈值", cycle15m)
	printStat("口径C 5m收盘 vs 5m开盘 >阈值", bar5m)
}

func printStat(title string, m map[string]int) {
	total := 0
	for _, v := range m {
		total += v
	}
	fmt.Printf("\n=== %s ===\n总次数: %d，涉及币数: %d\n", title, total, len(m))
	type kv struct {
		s string
		n int
	}
	var arr []kv
	for s, n := range m {
		arr = append(arr, kv{s, n})
	}
	sort.Slice(arr, func(i, j int) bool { return arr[i].n > arr[j].n })
	for _, a := range arr {
		fmt.Printf("  %-14s %d 次\n", a.s, a.n)
	}
}

// countGains 统计三种口径的涨幅超阈值次数（同币多次均计数）。
func countGains(bars []kline, gain float64) (cycle5m, cycle15m, bar5m int) {
	// 按 15m 周期分桶，周期开盘 = 该周期第一根 5m 的开盘
	type cyc struct {
		open  float64
		closes []float64
	}
	cycles := map[int64]*cyc{}
	var order []int64
	for _, k := range bars {
		pid := k.openTime / 900000
		c, ok := cycles[pid]
		if !ok {
			c = &cyc{open: k.open}
			cycles[pid] = c
			order = append(order, pid)
		}
		c.closes = append(c.closes, k.close)
		// 口径C：5m 单根实体
		if k.open > 0 && (k.close-k.open)/k.open*100 > gain {
			bar5m++
		}
	}
	for _, pid := range order {
		c := cycles[pid]
		// 口径A：周期内每根已收盘5m 收盘 vs 周期开盘
		for _, cl := range c.closes {
			if c.open > 0 && (cl-c.open)/c.open*100 > gain {
				cycle5m++
			}
		}
		// 口径B：15m 收盘 vs 15m 开盘（周期走完才有收盘）
		if len(c.closes) >= 3 && c.open > 0 {
			cl := c.closes[len(c.closes)-1]
			if (cl-c.open)/c.open*100 > gain {
				cycle15m++
			}
		}
	}
	return
}

func fetchPool(client *http.Client, minVol float64, maxSym int) []string {
	resp, err := client.Get(fapiBase + "/fapi/v1/ticker/24hr")
	if err != nil {
		log.Fatalf("拉取行情失败: %v", err)
	}
	defer resp.Body.Close()
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

func fetchKlinesRange(client *http.Client, symbol string, startMs, endMs int64) ([]kline, error) {
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
		for i, dst := range []*float64{&k.open, &k.close} {
			if err := json.Unmarshal(row[1+i], &s); err != nil {
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
