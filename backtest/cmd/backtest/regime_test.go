package main

import "testing"

// 市场状态过滤（S01 单因子实验）单元测试

func TestRegimeOK_None(t *testing.T) {
	e := NewEngine(DefaultConfig())
	if !e.regimeOK() {
		t.Fatal("regime 为空/关闭时应恒为 true")
	}
}

func TestRegimeOK_BTCEMA(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Regime = "btcma"
	e := NewEngine(cfg)

	// EMA 未初始化 → false（不盲目放行）
	e.btc = &btcState{ema: 100, close: 101}
	if e.regimeOK() {
		t.Fatal("BTC EMA 未初始化时应为 false")
	}

	// close < EMA → 熊市门控，false
	e.btc = &btcState{ema: 100, emaInit: true, close: 99}
	if e.regimeOK() {
		t.Fatal("BTC close<EMA 时应为 false")
	}

	// close >= EMA → true
	e.btc.close = 101
	if !e.regimeOK() {
		t.Fatal("BTC close>=EMA 时应为 true")
	}
}

func TestRegimeOK_BTC24h(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Regime = "btc24h"
	cfg.RegimeParam = 0 // BTC 24h 涨幅 >= 0% 才允许开仓
	e := NewEngine(cfg)

	e.btc = &btcState{chg24: 2, chg24Ready: false}
	if e.regimeOK() {
		t.Fatal("24h 窗口未就绪时应为 false")
	}

	e.btc = &btcState{chg24: -1.5, chg24Ready: true}
	if e.regimeOK() {
		t.Fatal("BTC 24h -1.5% < 0% 时应为 false")
	}

	e.btc.chg24 = 0.5
	if !e.regimeOK() {
		t.Fatal("BTC 24h +0.5% >= 0% 时应为 true")
	}
}

func TestRegimeOK_Breadth(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Regime = "breadth"
	cfg.RegimeParam = 0.5 // 50% 以上币 24h 上涨才允许开仓
	e := NewEngine(cfg)

	// 构造 3 个 24h 窗口已满的币: 2 涨 1 跌
	for _, v := range []struct {
		sym     string
		old, cur float64
	}{
		{"AAAUSDT", 100, 110},
		{"BBBUSDT", 100, 105},
		{"CCCUSDT", 100, 95},
	} {
		st := &symbolState{filled: WindowBars, idx: 2}
		st.closes[1] = v.cur // 当前收盘
		st.closes[2] = v.old // 24h 前收盘（idx 所指槽位）
		e.states[v.sym] = st
	}

	// 2/3 = 66.7% >= 50% → 通过
	if !e.regimeOK() {
		t.Fatal("上涨占比 66.7% >= 50% 时应通过")
	}

	// 调高阈值到 70% → 过滤
	e.cfg.RegimeParam = 0.7
	if e.regimeOK() {
		t.Fatal("上涨占比 66.7% < 70% 时应被过滤")
	}
}
