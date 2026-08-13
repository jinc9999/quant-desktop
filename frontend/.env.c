# C 版（超能战士）前端构建环境
# 注意：--mode c 不会加载 .env.production，必须在此补齐生产必需变量（否则路由初始化崩溃）
VITE_PRODUCT_VARIANT=C
VITE_PUBLIC_PATH = /
VITE_ROUTER_HISTORY = "hash"
VITE_CDN = false
VITE_COMPRESSION = "none"
