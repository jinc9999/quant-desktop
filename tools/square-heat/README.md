# 币安广场热度采集器（square-heat-collector）

研究用旁路工具：采集币安广场热帖与币种热度，供后续单因子验证。
**不参与任何交易，不修改策略与主程序。**

## 为什么需要浏览器

币安广场没有官方读取 API（官方接口只能发帖）。网页版依赖浏览器真实计算的
指纹请求头（bnc-uuid / csrftoken / device-info 等），普通 HTTP 会被拒绝。
本工具用 Playwright 打开一次广场页面嗅探这些请求头，之后在页面内定时拉取
热帖 feed（浏览器自动携带 Cookie 与系统代理）。

## 安装（一次性）

需要 Node.js 18+。

```powershell
cd quant-desktop\tools\square-heat
npm install
npx playwright install chromium   # 首次下载浏览器内核约 150MB
```

## 运行

```powershell
node collector.mjs            # 常驻，默认每 10 分钟采集一轮
node collector.mjs --once     # 只跑一轮，用于验证
```

可选环境变量：

- `SQ_INTERVAL_SEC`：采集间隔秒数（默认 600）
- `SQ_DATA_DIR`：数据目录（默认本目录下 `data\`）

## 产出数据（均在 data\ 目录）

- `posts.jsonl`：原始帖子（逐条追加，含 id/作者/时间/互动数/提到的币种）
- `heat_snapshots.jsonl`：每轮热度快照（12 小时窗口、时间衰减，Top40 币种）
- `collector.log`：运行日志

## 说明

- 依赖系统代理（如 v2rayN 127.0.0.1:10808）才能访问币安官网；
- 数据留在本机，不提交仓库（已在 .gitignore 排除 data/ 与 node_modules/）；
- 采集满 1-2 个月后，用 `heat_snapshots.jsonl` 与 S01 信号做单因子统计：
  热度分位高的币 vs 低的币，追涨后 15m/1h 收益与胜率是否有显著差异。
