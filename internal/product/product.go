// Package product 提供产品变体信息（A/B/C 构建期注入）。
// C 版“超能战士”通过 -ldflags -X 注入以下变量：
//
//	-X quant-desktop/internal/product.Variant=C
//	-X quant-desktop/internal/product.ProductName=超能战士
//	-X quant-desktop/internal/product.LicenseServerURL=https://license.example.com
package product

// 构建期注入变量（默认值对应 A 版）
var (
	// Variant 产品变体：A（默认） / B / C
	Variant = "A"
	// ProductName 产品展示名（C 版=超能战士；A/B 版留空时前端用默认名）
	ProductName = ""
	// LicenseServerURL 授权服务器地址（仅 C 版使用；A/B 版忽略）
	LicenseServerURL = "http://127.0.0.1:8081"
)

// Info 返回给前端的产品信息（用于 UI 变体切换与主题色）
type Info struct {
	Variant     string `json:"variant"`
	ProductName string `json:"productName"`
}

// IsC 是否 C 版（超能战士）
func IsC() bool {
	return Variant == "C"
}

// GetInfo 获取产品信息
func GetInfo() Info {
	name := ProductName
	if name == "" {
		switch Variant {
		case "C":
			name = "超能战士"
		case "B":
			name = "币安-魔力稳健B策略"
		default:
			name = "币安-魔力进攻A策略"
		}
	}
	return Info{
		Variant:     Variant,
		ProductName: name,
	}
}
