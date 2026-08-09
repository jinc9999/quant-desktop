// 回测结果可视化：生成 SVG 格式的资金曲线与回撤曲线图。
package main

import (
	"fmt"
	"math"
	"os"
	"strings"
	"time"
)

// chartConfig SVG 图表布局配置
const (
	svgWidth  = 1200
	svgHeight = 520
	plotLeft  = 70
	plotRight = 1170
	eqTop     = 30   // 资金曲线面板顶部
	eqBottom  = 270  // 资金曲线面板底部
	ddTop     = 320  // 回撤面板顶部
	ddBottom  = 470  // 回撤面板底部
	maxPoints = 2000 // 图表最大描点数（抽样）
)

// downsample 对序列等间隔抽样，控制描点数量
// 参数:
//   - n: 原始点数
//
// 返回:
//   - int: 抽样步长（>=1）
func downsample(n int) int {
	if n <= maxPoints {
		return 1
	}
	return int(math.Ceil(float64(n) / float64(maxPoints)))
}

// fmtPx 格式化坐标为字符串
// 参数:
//   - v: 坐标值
//
// 返回:
//   - string: 保留 1 位小数的坐标文本
func fmtPx(v float64) string {
	return fmt.Sprintf("%.1f", v)
}

// polyline 构建 SVG polyline 点串
// 参数:
//   - pts: 坐标点数组
//
// 返回:
//   - string: "x,y x,y ..."
func polyline(pts [][2]float64) string {
	var sb strings.Builder
	for _, p := range pts {
		sb.WriteString(fmt.Sprintf("%.1f,%.1f ", p[0], p[1]))
	}
	return strings.TrimSpace(sb.String())
}

// writeReportSVG 生成资金曲线 + 回撤曲线 SVG 图
// 参数:
//   - path: 输出文件路径
//   - curve: 权益曲线（逐片）
//   - initial: 初始权益
//
// 返回:
//   - error: 写入失败时返回错误
func writeReportSVG(path string, curve []EquityPoint, initial float64) error {
	if len(curve) == 0 {
		return fmt.Errorf("权益曲线为空")
	}

	step := downsample(len(curve))
	n := (len(curve)-1)/step + 1

	// 抽样权益点
	eqPts := make([][2]float64, 0, n)
	minEq, maxEq := math.Inf(1), math.Inf(-1)
	for i := 0; i < len(curve); i += step {
		p := curve[i]
		eqPts = append(eqPts, [2]float64{float64(i), p.Equity})
		if p.Equity < minEq {
			minEq = p.Equity
		}
		if p.Equity > maxEq {
			maxEq = p.Equity
		}
	}
	// 最后一个点必含
	if curve[len(curve)-1].TS != curve[len(eqPts)-1*step].TS {
		last := curve[len(curve)-1]
		eqPts = append(eqPts, [2]float64{float64(len(curve) - 1), last.Equity})
		if last.Equity < minEq {
			minEq = last.Equity
		}
		if last.Equity > maxEq {
			maxEq = last.Equity
		}
	}
	if maxEq == minEq {
		maxEq = minEq + 1
	}

	// 计算回撤序列（抽样）
	ddPts := make([][2]float64, 0, n)
	peak := initial
	maxDD := 0.0
	for i := 0; i < len(curve); i++ {
		p := curve[i]
		if p.Equity > peak {
			peak = p.Equity
		}
		dd := (peak - p.Equity) / peak * 100
		if dd > maxDD {
			maxDD = dd
		}
		if i%step == 0 {
			ddPts = append(ddPts, [2]float64{float64(i), dd})
		}
	}

	total := float64(len(curve) - 1)
	plotW := float64(plotRight - plotLeft)

	// 坐标换算
	eqXY := make([][2]float64, len(eqPts))
	for i, p := range eqPts {
		x := plotLeft + p[0]/total*plotW
		y := eqBottom - (p[1]-minEq)/(maxEq-minEq)*(eqBottom-eqTop)
		eqXY[i] = [2]float64{x, y}
	}
	ddXY := make([][2]float64, len(ddPts))
	for i, p := range ddPts {
		x := plotLeft + p[0]/total*plotW
		y := ddBottom - p[1]/maxDD*(ddBottom-ddTop)
		ddXY[i] = [2]float64{x, y}
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, `<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="%d" viewBox="0 0 %d %d">
<style>text{font-family:system-ui,-apple-system,sans-serif;font-size:12px;fill:#666} .title{font-size:15px;font-weight:bold;fill:#222}</style>`, svgWidth, svgHeight, svgWidth, svgHeight)

	// 标题
	fmt.Fprintf(&sb, `<text class="title" x="%d" y="20">资金曲线（初始 %sU → 期末 %sU）</text>`, plotLeft, fmtPx(initial), fmtPx(curve[len(curve)-1].Equity))
	fmt.Fprintf(&sb, `<text class="title" x="%d" y="310">回撤曲线（最大回撤 %s%%）</text>`, plotLeft, fmtPx(maxDD))

	// 资金曲线面板
	fmt.Fprintf(&sb, `<rect x="%d" y="%d" width="%v" height="%d" fill="#fafbfc" stroke="#ddd"/>`, plotLeft, eqTop, plotW, eqBottom-eqTop)
	fmt.Fprintf(&sb, `<polyline points="%s" fill="none" stroke="#1a73e8" stroke-width="1.5"/>`, polyline(eqXY))
	// 初始权益参考线
	yInit := eqBottom - (initial-minEq)/(maxEq-minEq)*(eqBottom-eqTop)
	fmt.Fprintf(&sb, `<line x1="%d" y1="%s" x2="%d" y2="%s" stroke="#999" stroke-dasharray="4 4"/>`, plotLeft, fmtPx(yInit), plotRight, fmtPx(yInit))

	// 回撤面板
	fmt.Fprintf(&sb, `<rect x="%d" y="%d" width="%v" height="%d" fill="#fafbfc" stroke="#ddd"/>`, plotLeft, ddTop, plotW, ddBottom-ddTop)
	fmt.Fprintf(&sb, `<polyline points="%s" fill="none" stroke="#d93025" stroke-width="1.5"/>`, polyline(ddXY))

	// 横轴时间刻度（6 个）
	firstTS := curve[0].TS
	lastTS := curve[len(curve)-1].TS
	for i := 0; i <= 5; i++ {
		ts := firstTS + int64(float64(i)/5.0*float64(lastTS-firstTS))
		x := plotLeft + float64(i)/5.0*plotW
		fmt.Fprintf(&sb, `<line x1="%s" y1="%d" x2="%s" y2="%d" stroke="#bbb"/>`, fmtPx(x), eqBottom, fmtPx(x), eqBottom+5)
		fmt.Fprintf(&sb, `<text x="%s" y="%d">%s</text>`, fmtPx(x-22), eqBottom+18, time.UnixMilli(ts).Format("2006-01"))
	}

	// 纵轴权益刻度（5 个）
	for i := 0; i <= 4; i++ {
		v := minEq + float64(i)/4.0*(maxEq-minEq)
		y := eqBottom - float64(i)/4.0*(eqBottom-eqTop)
		fmt.Fprintf(&sb, `<text x="8" y="%s">%s</text>`, fmtPx(y+4), fmtPx(v))
	}
	// 纵轴回撤刻度（3 个）
	for i := 0; i <= 2; i++ {
		v := maxDD * float64(i) / 2.0
		y := ddBottom - float64(i)/2.0*(ddBottom-ddTop)
		fmt.Fprintf(&sb, `<text x="8" y="%s">%s%%</text>`, fmtPx(y+4), fmtPx(v))
	}

	sb.WriteString(`</svg>`)
	return os.WriteFile(path, []byte(sb.String()), 0o644)
}
