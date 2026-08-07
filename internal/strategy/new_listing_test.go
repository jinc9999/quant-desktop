// Package strategy 新币过滤纯函数测试
package strategy

import (
	"testing"

	"quant-desktop/internal/binance"
)

// TestListingDays 验证上市天数计算：自然日向下取整；未知/异常返回 -1
func TestListingDays(t *testing.T) {
	dayMs := int64(24 * 3600 * 1000)
	now := int64(1753000000000) // 固定基准，避免时间相关
	cases := []struct {
		name    string
		onboard int64
		want    int
	}{
		{"上市当天", now, 0},
		{"上市30天", now - 30*dayMs, 30},
		{"上市60天整", now - 60*dayMs, 60},
		{"上市61天", now - 61*dayMs, 61},
		{"上市1年", now - 365*dayMs, 365},
		{"onboardDate为0(未知)", 0, -1},
		{"未来时间(异常)", now + dayMs, -1},
	}
	for _, c := range cases {
		if got := ListingDays(c.onboard, now); got != c.want {
			t.Errorf("%s: ListingDays = %d, 期望 %d", c.name, got, c.want)
		}
	}
}

// TestFilterTickers 验证筛选层剔除被拦截合约：拦截集合为空时原样返回
func TestFilterTickers(t *testing.T) {
	tickers := []binance.Ticker{
		{Symbol: "NEWUSDT"}, {Symbol: "OLDUSDT"},
	}
	if got := filterTickers(tickers, nil); len(got) != 2 {
		t.Errorf("无拦截时数量 = %d, 期望 2", len(got))
	}
	if got := filterTickers(tickers, map[string]bool{}); len(got) != 2 {
		t.Errorf("空拦截集合时数量 = %d, 期望 2", len(got))
	}
	got := filterTickers(tickers, map[string]bool{"NEWUSDT": true})
	if len(got) != 1 || got[0].Symbol != "OLDUSDT" {
		t.Errorf("拦截 NEWUSDT 后 = %+v, 期望仅 OLDUSDT", got)
	}
	if got := filterTickers(tickers, map[string]bool{"NEWUSDT": true, "OLDUSDT": true}); len(got) != 0 {
		t.Errorf("全拦截后数量 = %d, 期望 0", len(got))
	}
}
