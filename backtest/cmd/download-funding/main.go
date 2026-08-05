// Command download-funding 从币安官方历史数据站 data.binance.vision 下载
// USD-M 合约历史资金费率数据（月度 zip 包），并按币种合并为单文件 CSV。
//
// 与 K 线下载器的区别:
//   - 数据源路径为 .../monthly/fundingRate/{SYMBOL}/{SYMBOL}-fundingRate-{YYYY-MM}.zip
//   - CSV 为 3 列（2024 年起格式）: calc_time, funding_interval_hours, last_funding_rate
//     （calc_time 带毫秒偏移，回测端会归一到 8h 结算边界）
//   - 默认从已有 K 线数据目录（../data）推导币种列表，保证回测时两套数据币种一致
//
// 用法:
//
//	go run ./cmd/download-funding -workers 16
//	go run ./cmd/download-funding -symbols BTCUSDT,ETHUSDT -start 2024-01 -end 2026-07
//	go run ./cmd/download-funding -all -workers 16
package main

import (
	"archive/zip"
	"bufio"
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"io"
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

// BaseURL 币安官方历史数据站月度资金费率根地址
const BaseURL = "https://data.binance.vision/data/futures/um/monthly/fundingRate"

// month 表示一个 (币种, 月份) 的下载任务
type month struct {
	symbol string
	ym     string // "2024-01"
	url    string
	size   int64 // HEAD 得到的目标大小，用于断点校验
}

// binanceExchangeInfo 币安期货 exchangeInfo 响应结构（仅解析所需字段）
type binanceExchangeInfo struct {
	Symbols []struct {
		Symbol       string `json:"symbol"`
		ContractType string `json:"contractType"`
		Status       string `json:"status"`
	} `json:"symbols"`
}

// fetchAllSymbols 从币安主网拉取全部 TRADING 状态的永续合约交易对
// 参数:
//   - client: 已配置代理的 HTTP 客户端
//
// 返回:
//   - []string: 交易对列表（如 ["BTCUSDT", ...]）
//   - error: 请求或解析失败时返回错误
func fetchAllSymbols(client *http.Client) ([]string, error) {
	resp, err := client.Get("https://fapi.binance.com/fapi/v1/exchangeInfo")
	if err != nil {
		return nil, fmt.Errorf("拉取 exchangeInfo 失败: %w", err)
	}
	defer resp.Body.Close()

	var info binanceExchangeInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return nil, fmt.Errorf("解析 exchangeInfo 失败: %w", err)
	}

	symbols := make([]string, 0, len(info.Symbols))
	for _, s := range info.Symbols {
		if s.ContractType == "PERPETUAL" && s.Status == "TRADING" {
			symbols = append(symbols, s.Symbol)
		}
	}
	sort.Strings(symbols)
	return symbols, nil
}

// symbolsFromKlines 从 K 线 CSV 数据目录推导币种列表（去 .csv 后缀后排序）
// 与回测加载的数据集严格一致，确保资金费率与 K 线币种对齐。
// 参数:
//   - dir: K 线数据目录（如 ../data）
//
// 返回:
//   - []string: 币种列表
//   - error: 目录读取失败时返回错误
func symbolsFromKlines(dir string) ([]string, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.csv"))
	if err != nil {
		return nil, err
	}
	symbols := make([]string, 0, len(files))
	for _, f := range files {
		symbols = append(symbols, strings.TrimSuffix(filepath.Base(f), ".csv"))
	}
	sort.Strings(symbols)
	return symbols, nil
}

// buildMonths 为每个币种生成指定月份范围内的下载任务列表
// 参数:
//   - symbols: 币种列表
//   - start: 起始月份 "2024-01"
//   - end: 结束月份 "2026-07"
//
// 返回:
//   - []month: 全部 (币种, 月份) 任务
//   - error: 月份解析失败时返回错误
func buildMonths(symbols []string, start, end string) ([]month, error) {
	st, err := time.Parse("2006-01", start)
	if err != nil {
		return nil, fmt.Errorf("起始月份格式错误 %q: %w", start, err)
	}
	en, err := time.Parse("2006-01", end)
	if err != nil {
		return nil, fmt.Errorf("结束月份格式错误 %q: %w", end, err)
	}
	if en.Before(st) {
		return nil, fmt.Errorf("结束月份 %s 早于起始月份 %s", end, start)
	}

	var tasks []month
	for _, sym := range symbols {
		for m := st; !m.After(en); m = m.AddDate(0, 1, 0) {
			ym := m.Format("2006-01")
			tasks = append(tasks, month{
				symbol: sym,
				ym:     ym,
				url:    fmt.Sprintf("%s/%s/%s-fundingRate-%s.zip", BaseURL, sym, sym, ym),
			})
		}
	}
	return tasks, nil
}

// headSize 通过 HEAD 请求获取远程文件大小，用于断点续传校验
// 参数:
//   - client: HTTP 客户端
//   - url: 远程文件地址
//
// 返回:
//   - int64: 文件大小（字节），-1 表示获取失败或不存在
func headSize(client *http.Client, url string) int64 {
	req, err := http.NewRequest(http.MethodHead, url, nil)
	if err != nil {
		return -1
	}
	resp, err := client.Do(req)
	if err != nil {
		return -1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return -1
	}
	return resp.ContentLength
}

// downloadOne 下载单个月度 zip 到本地 raw 目录
// 已存在且大小与远程一致时跳过（断点续传）；返回 false 表示已跳过
// 参数:
//   - client: HTTP 客户端
//   - rawDir: 本地 raw 目录
//   - task: 下载任务
//
// 返回:
//   - bool: true=本次实际下载，false=已存在跳过或失败
//   - error: 下载失败时返回错误
func downloadOne(client *http.Client, rawDir string, task month) (bool, error) {
	dest := filepath.Join(rawDir, task.symbol, task.ym+".zip")
	if fi, err := os.Stat(dest); err == nil {
		if task.size < 0 {
			task.size = headSize(client, task.url)
		}
		if task.size >= 0 && fi.Size() == task.size {
			return false, nil // 已下载完整，跳过
		}
	}

	resp, err := client.Get(task.url)
	if err != nil {
		return false, fmt.Errorf("%s %s 下载失败: %w", task.symbol, task.ym, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("%s %s 响应码 %d", task.symbol, task.ym, resp.StatusCode)
	}

	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return false, err
	}
	tmp := dest + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return false, err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		return false, err
	}
	f.Close()
	if err := os.Rename(tmp, dest); err != nil {
		return false, err
	}
	return true, nil
}

// mergeSymbol 将某币种的全部月度 zip 解压并按 calc_time 升序合并为单文件 CSV
// 每行 3 列: calc_time, funding_interval_hours, last_funding_rate
// 跳过表头行与非数值行；按时间戳去重。
// 参数:
//   - rawDir: raw zip 目录
//   - dataDir: 合并 CSV 输出目录
//   - symbol: 币种
//   - months: 该币种已下载完成的月份列表（升序）
//
// 返回:
//   - int: 合并写入的资金费率记录数
//   - error: 解压或写入失败时返回错误
func mergeSymbol(rawDir, dataDir, symbol string, months []string) (int, error) {
	outPath := filepath.Join(dataDir, symbol+".csv")
	tmp := outPath + ".tmp"

	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return 0, err
	}

	of, err := os.Create(tmp)
	if err != nil {
		return 0, err
	}
	w := csv.NewWriter(of)

	lastTime := int64(-1)
	total := 0
	for _, ym := range months {
		zipPath := filepath.Join(rawDir, symbol, ym+".zip")
		zr, err := zip.OpenReader(zipPath)
		if err != nil {
			of.Close()
			return total, fmt.Errorf("%s %s 打开 zip 失败: %w", symbol, ym, err)
		}
		for _, zf := range zr.File {
			if !strings.HasSuffix(zf.Name, ".csv") {
				continue
			}
			rc, err := zf.Open()
			if err != nil {
				zr.Close()
				of.Close()
				return total, err
			}
			br := bufio.NewReaderSize(rc, 1<<20)
			cr := csv.NewReader(br)
			cr.FieldsPerRecord = -1
			for {
				rec, err := cr.Read()
				if err == io.EOF {
					break
				}
				if err != nil {
					rc.Close()
					zr.Close()
					of.Close()
					return total, err
				}
				if len(rec) < 3 {
					continue
				}
				ts, err := strconv.ParseInt(rec[0], 10, 64)
				if err != nil {
					continue // 表头或脏行，跳过
				}
				if ts <= lastTime {
					continue // 跨月边界重复或乱序行，去重
				}
				if err := w.Write(rec); err != nil {
					rc.Close()
					zr.Close()
					of.Close()
					return total, err
				}
				lastTime = ts
				total++
			}
			rc.Close()
		}
		zr.Close()
	}
	w.Flush()
	if err := w.Error(); err != nil {
		of.Close()
		return total, err
	}
	of.Close()
	if err := os.Rename(tmp, outPath); err != nil {
		return total, err
	}
	return total, nil
}

// main 资金费率下载器入口
// 参数由命令行 flag 提供（-klines-dir / -symbols / -all / -start / -end / -workers / -dir）
func main() {
	klinesDir := flag.String("klines-dir", "data", "K 线数据目录（用于推导默认币种列表，保证两套数据对齐）")
	symbolsFlag := flag.String("symbols", "", "币种列表，逗号分隔（如 BTCUSDT,ETHUSDT）")
	allFlag := flag.Bool("all", false, "下载币安全部永续合约币种（从 exchangeInfo 拉取）")
	startFlag := flag.String("start", "2024-01", "起始月份 YYYY-MM")
	endFlag := flag.String("end", "2026-07", "结束月份 YYYY-MM")
	workersFlag := flag.Int("workers", 16, "并发下载数")
	dirFlag := flag.String("dir", "data_funding", "数据输出目录")
	proxyFlag := flag.String("proxy", "", "HTTP 代理地址（如 http://127.0.0.1:7897）")
	flag.Parse()

	fmt.Printf("环境代理: http_proxy=%q https_proxy=%q 指定代理: %q\n", os.Getenv("http_proxy"), os.Getenv("https_proxy"), *proxyFlag)

	transport := &http.Transport{}
	if *proxyFlag != "" {
		proxyURL, err := url.Parse(*proxyFlag)
		if err != nil {
			fmt.Printf("[错误] 代理地址无效: %v\n", err)
			os.Exit(1)
		}
		transport.Proxy = http.ProxyURL(proxyURL)
	}
	client := &http.Client{Transport: transport, Timeout: 90 * time.Second}

	// 1. 确定币种列表
	var symbols []string
	switch {
	case *allFlag:
		all, err := fetchAllSymbols(client)
		if err != nil {
			fmt.Printf("[错误] 拉取全量币种失败: %v\n", err)
			os.Exit(1)
		}
		symbols = all
		fmt.Printf("全量币种 %d 个\n", len(symbols))
	case *symbolsFlag != "":
		symbols = strings.Split(*symbolsFlag, ",")
		fmt.Printf("指定币种 %d 个: %v\n", len(symbols), symbols)
	default:
		syms, err := symbolsFromKlines(*klinesDir)
		if err != nil || len(syms) == 0 {
			fmt.Printf("[警告] 从 %s 推导币种失败（%v），使用指定列表或 -all\n", *klinesDir, err)
			os.Exit(1)
		}
		symbols = syms
		fmt.Printf("从 K 线目录推导币种 %d 个\n", len(symbols))
	}

	// 2. 生成任务并预检远程大小
	tasks, err := buildMonths(symbols, *startFlag, *endFlag)
	if err != nil {
		fmt.Printf("[错误] %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("任务总数 %d（%d 币 × %s~%s）\n", len(tasks), len(symbols), *startFlag, *endFlag)

	// 预检每个任务远程大小（并行 HEAD）
	var wg sync.WaitGroup
	sem := make(chan struct{}, *workersFlag)
	for i := range tasks {
		wg.Add(1)
		go func(t *month) {
			defer wg.Done()
			sem <- struct{}{}
			t.size = headSize(client, t.url)
			<-sem
		}(&tasks[i])
	}
	wg.Wait()
	missing := 0
	for _, t := range tasks {
		if t.size < 0 {
			missing++
		}
	}
	if missing > 0 {
		fmt.Printf("[警告] %d 个任务远程文件不存在（多为该币种尚未上市），将跳过\n", missing)
	}

	// 3. 并发下载
	rawDir := filepath.Join(*dirFlag, "raw")
	dataDir := filepath.Join(*dirFlag)
	var mu sync.Mutex
	done, skipped, failed := 0, 0, 0
	var failures []string
	jobs := make(chan month)
	var dwg sync.WaitGroup
	for i := 0; i < *workersFlag; i++ {
		dwg.Add(1)
		go func() {
			defer dwg.Done()
			for t := range jobs {
				if t.size < 0 {
					mu.Lock()
					skipped++
					mu.Unlock()
					continue
				}
				ok, err := downloadOne(client, rawDir, t)
				mu.Lock()
				if err != nil {
					failed++
					failures = append(failures, fmt.Sprintf("%s %s: %v", t.symbol, t.ym, err))
				} else if ok {
					done++
				} else {
					skipped++
				}
				mu.Unlock()
			}
		}()
	}
	start := time.Now()
	for _, t := range tasks {
		jobs <- t
	}
	close(jobs)
	dwg.Wait()

	fmt.Printf("下载完成: 新下载 %d, 跳过 %d, 失败 %d, 耗时 %.1f 分钟\n", done, skipped, failed, time.Since(start).Minutes())
	for _, f := range failures {
		fmt.Println("  失败:", f)
	}

	// 4. 顺序合并（避免并发写同一文件）
	bySymbol := map[string][]string{}
	for _, t := range tasks {
		if t.size < 0 {
			continue
		}
		bySymbol[t.symbol] = append(bySymbol[t.symbol], t.ym)
	}
	totalRows := 0
	for _, sym := range symbols {
		months := bySymbol[sym]
		if len(months) == 0 {
			continue
		}
		n, err := mergeSymbol(rawDir, dataDir, sym, months)
		if err != nil {
			fmt.Printf("  合并 %s 失败: %v\n", sym, err)
			continue
		}
		totalRows += n
		fmt.Printf("  合并 %s: %d 条资金费率 (%s)\n", sym, n, sym+".csv")
	}
	fmt.Printf("合并完成，共 %d 条资金费率记录，用时 %.1f 分钟\n", totalRows, time.Since(start).Minutes())
}
