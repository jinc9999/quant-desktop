<script setup lang="ts">
/**
 * 仪表盘 - 账户状态与核心数据展示
 * 聚合展示账户状态、权益、运行时间、Tick 统计、今日盈利等关键指标
 * 通过 Wails 绑定调用 Go 后端 QuantService.GetDashboardData
 */
import { ref, computed, onMounted, onUnmounted } from "vue";
import { QuantService } from "../../../bindings/quant-desktop/internal/bindings";
import { callService } from "../../utils/service";

defineOptions({ name: "Dashboard" });

/** C 版（超能战士）：隐藏策略参数相关指标卡 */
const isC = import.meta.env.VITE_PRODUCT_VARIANT === "C";

/** 仪表盘数据（与 Go 后端 GetDashboardData 返回字段一致） */
interface DashboardData {
  running: boolean;
  mode: string;
  tickCount: number;
  tickErrorCount: number;
  startTime: number;
  runtimeSeconds: number;
  todayPnl: number;
  todayClosedCount: number;
  openPositionCount: number;
  totalWalletBalance: number;
  totalUnrealizedPnl: number;
  totalMarginBalance: number;
  availableBalance: number;
  scanIntervalSec: number;
  timeframe: string;
  topN: number;
  cooldownMin: number;
  marginMode: string;
  tickerLoadMsg: string;
}

const data = ref<DashboardData>({
  running: false,
  mode: "SIMULATION",
  tickCount: 0,
  tickErrorCount: 0,
  startTime: 0,
  runtimeSeconds: 0,
  todayPnl: 0,
  todayClosedCount: 0,
  openPositionCount: 0,
  totalWalletBalance: 0,
  totalUnrealizedPnl: 0,
  totalMarginBalance: 0,
  availableBalance: 0,
  scanIntervalSec: 30,
  timeframe: "15m",
  topN: 3,
  cooldownMin: 60,
  marginMode: "ISOLATED",
  tickerLoadMsg: ""
});

const loading = ref(true);
/** 每日总结数据 */
const summary = ref<{
  market: any;
  trades: any;
  suggestions: string[];
}>({ market: {}, trades: {}, suggestions: [] });
/** 当前展示的总结模式（默认跟随程序当前模式，可手动切换） */
const activeMode = ref("SIMULATION");
const modeSummary = computed(() => {
  const modes = (summary.value as any).modes || {};
  return modes[activeMode.value] || { trades: {}, suggestions: [] };
});
let timer: ReturnType<typeof setInterval> | null = null;
let summaryTimer: ReturnType<typeof setInterval> | null = null;

/** 格式化运行时间为 HH:MM:SS */
const runtimeText = computed(() => {
  const s = data.value.runtimeSeconds;
  if (s <= 0) return "--";
  const h = Math.floor(s / 3600);
  const m = Math.floor((s % 3600) / 60);
  const sec = s % 60;
  return `${String(h).padStart(2, "0")}:${String(m).padStart(2, "0")}:${String(sec).padStart(2, "0")}`;
});

/** 格式化启动时间 */
const startTimeText = computed(() => {
  if (!data.value.startTime) return "--";
  const d = new Date(data.value.startTime);
  return d.toLocaleString("zh-CN", {
    month: "2-digit",
    day: "2-digit",
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit"
  });
});

/** 运行模式显示文本 */
const modeText = computed(() => {
  const map: Record<string, string> = {
    SIMULATION: "模拟盘",
    LIVE: "实盘"
  };
  return map[data.value.mode] || data.value.mode;
});

/** 运行状态标签类型 */
const statusType = computed(() => (data.value.running ? "success" : "danger"));

/** 行情加载状态颜色 */
const loadStatusClass = computed(() => {
  const m = data.value.tickerLoadMsg || "";
  if (m.includes("⚠")) return "text-orange";
  if (m.includes("加载")) return "text-green";
  return "text-neutral";
});

/** 今日盈亏颜色类 */
const pnlClass = computed(() => {
  if (data.value.todayPnl > 0) return "text-green";
  if (data.value.todayPnl < 0) return "text-red";
  return "text-neutral";
});

/** 未实现盈亏颜色类 */
const upnlClass = computed(() => {
  if (data.value.totalUnrealizedPnl > 0) return "text-green";
  if (data.value.totalUnrealizedPnl < 0) return "text-red";
  return "text-neutral";
});

/** 格式化数值（保留 2 位小数，带正负号） */
function fmtNum(v: number, signed = false): string {
  const s = Math.abs(v).toFixed(2);
  if (signed && v > 0) return "+" + s;
  if (v < 0) return "-" + s;
  return s;
}

/** 拉取仪表盘数据 */
async function fetchData() {
  const res = await callService(() => QuantService.GetDashboardData(), { silent: true });
  if (res) {
    data.value = {
      running: res.running ?? false,
      mode: res.mode ?? "SIMULATION",
      tickCount: res.tickCount ?? 0,
      tickErrorCount: res.tickErrorCount ?? 0,
      startTime: res.startTime ?? 0,
      runtimeSeconds: res.runtimeSeconds ?? 0,
      todayPnl: res.todayPnl ?? 0,
      todayClosedCount: res.todayClosedCount ?? 0,
      openPositionCount: res.openPositionCount ?? 0,
      totalWalletBalance: res.totalWalletBalance ?? 0,
      totalUnrealizedPnl: res.totalUnrealizedPnl ?? 0,
      totalMarginBalance: res.totalMarginBalance ?? 0,
      availableBalance: res.availableBalance ?? 0,
      scanIntervalSec: res.scanIntervalSec ?? 30,
      timeframe: res.timeframe ?? "15m",
      topN: res.topN ?? 3,
      cooldownMin: res.cooldownMin ?? 60,
      marginMode: res.marginMode ?? "ISOLATED",
      tickerLoadMsg: res.tickerLoadMsg ?? ""
    };
  }
  loading.value = false;
}

/** 拉取每日总结 */
async function fetchSummary() {
  const res = await callService(() => QuantService.GetDailySummary(), { silent: true });
  if (res) {
    summary.value = {
      market: res.market ?? {},
      trades: res.trades ?? {},
      suggestions: res.suggestions ?? []
    };
    // 自动跟随当前模式（模拟盘/实盘），同时保留手动切换能力
    if (res.currentMode) activeMode.value = res.currentMode;
  }
}

/** 格式化成交额（亿/万） */
function fmtVolume(v: number): string {
  if (!v || v <= 0) return "--";
  if (v >= 1e8) return (v / 1e8).toFixed(1) + " 亿";
  if (v >= 1e4) return (v / 1e4).toFixed(0) + " 万";
  return v.toFixed(0);
}

/** 涨跌颜色 */
function chgClass(v: number): string {
  if (v > 0) return "text-green";
  if (v < 0) return "text-red";
  return "text-neutral";
}

function fmtChg(v: number): string {
  if (v === undefined || v === null) return "--";
  return (v > 0 ? "+" : "") + v.toFixed(2) + "%";
}

onMounted(() => {
  fetchData();
  fetchSummary();
  timer = setInterval(fetchData, 2000);
  summaryTimer = setInterval(fetchSummary, 60000);
});

onUnmounted(() => {
  if (timer) clearInterval(timer);
  if (summaryTimer) clearInterval(summaryTimer);
});
</script>

<template>
  <div class="dashboard-container" v-loading="loading">
    <!-- 第一行：核心指标卡片 -->
    <div class="card-row">
      <!-- 账户状态 -->
      <div class="quant-card status-card">
        <div class="card-header">
          <span class="card-title">账户状态</span>
          <el-tag :type="statusType" size="small" effect="dark" round>
            {{ data.running ? "运行中" : "已停止" }}
          </el-tag>
        </div>
        <div class="card-body">
          <div class="status-main">
            <span class="status-dot" :class="data.running ? 'dot-active' : 'dot-inactive'" />
            <span class="status-mode">{{ modeText }}</span>
          </div>
          <div class="status-sub">
            <span>持仓 {{ data.openPositionCount }} 个</span>
            <span>今日平仓 {{ data.todayClosedCount }} 次</span>
          </div>
        </div>
      </div>

      <!-- 账户权益 -->
      <div class="quant-card">
        <div class="card-header">
          <span class="card-title">账户权益</span>
          <span class="card-unit">USDT</span>
        </div>
        <div class="card-body">
          <div class="metric-value">{{ fmtNum(data.totalMarginBalance) }}</div>
          <div class="metric-detail">
            <div class="detail-row">
              <span class="detail-label">钱包余额</span>
              <span>{{ fmtNum(data.totalWalletBalance) }}</span>
            </div>
            <div class="detail-row">
              <span class="detail-label">可用余额</span>
              <span>{{ fmtNum(data.availableBalance) }}</span>
            </div>
            <div class="detail-row">
              <span class="detail-label">未实现盈亏</span>
              <span :class="upnlClass">{{ fmtNum(data.totalUnrealizedPnl, true) }}</span>
            </div>
          </div>
        </div>
      </div>

      <!-- 今日盈利 -->
      <div class="quant-card">
        <div class="card-header">
          <span class="card-title">今日盈利</span>
          <span class="card-unit">USDT</span>
        </div>
        <div class="card-body">
          <div class="metric-value" :class="pnlClass">
            {{ fmtNum(data.todayPnl, true) }}
          </div>
          <div class="metric-detail">
            <div class="detail-row">
              <span class="detail-label">平仓次数</span>
              <span>{{ data.todayClosedCount }}</span>
            </div>
            <div class="detail-row">
              <span class="detail-label">当前持仓</span>
              <span>{{ data.openPositionCount }}</span>
            </div>
          </div>
        </div>
      </div>
    </div>

    <!-- 第二行：运行信息卡片 -->
    <div class="card-row">
      <!-- 运行时间 -->
      <div class="quant-card">
        <div class="card-header">
          <span class="card-title">运行时间</span>
        </div>
        <div class="card-body">
          <div class="metric-value mono">{{ runtimeText }}</div>
          <div class="metric-detail">
            <div class="detail-row">
              <span class="detail-label">启动时间</span>
              <span>{{ startTimeText }}</span>
            </div>
          </div>
        </div>
      </div>

      <!-- Tick 统计 -->
      <div class="quant-card">
        <div class="card-header">
          <span class="card-title">Tick 统计</span>
        </div>
        <div class="card-body">
          <div class="metric-value mono">{{ data.tickCount.toLocaleString() }}</div>
          <div class="metric-detail">
            <div class="detail-row">
              <span class="detail-label">错误次数</span>
              <span :class="data.tickErrorCount > 0 ? 'text-red' : 'text-green'">
                {{ data.tickErrorCount }}
              </span>
            </div>
            <div class="detail-row">
              <span class="detail-label">错误率</span>
              <span :class="data.tickCount > 0 && data.tickErrorCount / data.tickCount > 0.05 ? 'text-red' : 'text-neutral'">
                {{ data.tickCount > 0 ? ((data.tickErrorCount / data.tickCount) * 100).toFixed(2) + "%" : "0%" }}
              </span>
            </div>
          </div>
        </div>
      </div>

      <!-- 扫描配置（C 版隐藏：策略参数不对外展示） -->
      <div v-if="!isC" class="quant-card">
        <div class="card-header">
          <span class="card-title">扫描配置</span>
        </div>
        <div class="card-body">
          <div class="metric-value mono">{{ data.scanIntervalSec }}<span class="value-unit">s</span></div>
          <div class="metric-detail">
            <div class="detail-row">
              <span class="detail-label">时间窗口</span>
              <span>{{ data.timeframe }}</span>
            </div>
            <div class="detail-row">
              <span class="detail-label">候选 Top N</span>
              <span>{{ data.topN }}</span>
            </div>
            <div class="detail-row">
              <span class="detail-label">保证金模式</span>
              <span>{{ data.marginMode === "ISOLATED" ? "逐仓" : "全仓" }}</span>
            </div>
            <div class="detail-row">
              <span class="detail-label">数据源</span>
              <span class="text-green">WS 实时流</span>
            </div>
          </div>
        </div>
      </div>
    </div>

    <!-- 行情加载状态 -->
    <div class="quant-card load-status-card">
      <div class="card-header">
        <span class="card-title">行情加载状态</span>
        <span class="summary-hint">启动后自动全量加载，缺币时告警并 REST 补齐</span>
      </div>
      <div class="load-status-body" :class="loadStatusClass">
        {{ data.tickerLoadMsg || "尚未加载全量行情" }}
      </div>
    </div>

    <!-- 每日总结 -->
    <div class="quant-card summary-panel">
      <div class="card-header">
        <span class="card-title">每日总结</span>
        <div class="summary-tools">
          <el-radio-group v-model="activeMode" size="small">
            <el-radio-button value="SIMULATION">模拟盘</el-radio-button>
            <el-radio-button value="LIVE">实盘</el-radio-button>
          </el-radio-group>
          <span class="summary-hint">每 60 秒自动刷新</span>
        </div>
      </div>

      <!-- 市场概况 -->
      <div class="summary-section">
        <div class="section-title">市场概况（24h）</div>
        <div class="summary-grid">
          <div class="summary-item">
            <span class="sum-label">上涨 / 下跌</span>
            <span>
              <b class="text-green">{{ summary.market.up ?? 0 }}</b>
              /
              <b class="text-red">{{ summary.market.down ?? 0 }}</b>
              <span class="sum-sub">（共 {{ summary.market.total ?? 0 }}）</span>
            </span>
          </div>
          <div class="summary-item">
            <span class="sum-label">中位涨跌</span>
            <span :class="chgClass(summary.market.medianChange ?? 0)">{{
              fmtChg(summary.market.medianChange)
            }}</span>
          </div>
          <div class="summary-item">
            <span class="sum-label">平均涨跌</span>
            <span :class="chgClass(summary.market.avgChange ?? 0)">{{
              fmtChg(summary.market.avgChange)
            }}</span>
          </div>
          <div class="summary-item">
            <span class="sum-label">总成交额</span>
            <span>{{ fmtVolume(summary.market.totalQuoteVolume) }} U</span>
          </div>
        </div>
        <div class="mover-row" v-if="summary.market.topGainers && summary.market.topGainers.length">
          <div class="mover-col">
            <div class="mover-title text-green">领涨</div>
            <div v-for="g in summary.market.topGainers.slice(0, 5)" :key="g.symbol" class="mover-item">
              <span>{{ g.symbol }}</span>
              <span class="text-green">{{ fmtChg(g.change) }}</span>
            </div>
          </div>
          <div class="mover-col">
            <div class="mover-title text-red">领跌</div>
            <div v-for="g in summary.market.topLosers.slice(0, 5)" :key="g.symbol" class="mover-item">
              <span>{{ g.symbol }}</span>
              <span class="text-red">{{ fmtChg(g.change) }}</span>
            </div>
          </div>
        </div>
      </div>

      <!-- 今日交易 -->
      <div class="summary-section">
        <div class="section-title">今日交易</div>
        <div class="summary-grid">
          <div class="summary-item">
            <span class="sum-label">平仓笔数</span>
            <span>{{ modeSummary.trades.closedCount ?? 0 }}</span>
          </div>
          <div class="summary-item">
            <span class="sum-label">今日盈亏</span>
            <span :class="chgClass(modeSummary.trades.todayPnl ?? 0)">{{
              fmtNum(modeSummary.trades.todayPnl ?? 0, true)
            }} U</span>
          </div>
          <div class="summary-item">
            <span class="sum-label">胜率</span>
            <span>{{ (modeSummary.trades.winRate ?? 0).toFixed(1) }}%</span>
          </div>
          <div class="summary-item">
            <span class="sum-label">止损 / 跟踪止盈</span>
            <span>{{ modeSummary.trades.stopCount ?? 0 }} / {{ modeSummary.trades.trailCount ?? 0 }}</span>
          </div>
        </div>
        <el-table :data="modeSummary.trades.byCoin ?? []" size="small" stripe class="coin-table">
          <el-table-column prop="symbol" label="币种" min-width="110" />
          <el-table-column prop="trades" label="交易次数" min-width="80" align="right" />
          <el-table-column prop="pnl" label="盈亏(U)" min-width="100" align="right">
            <template #default="{ row }">
              <span :class="chgClass(row.pnl)">{{ fmtNum(row.pnl, true) }}</span>
            </template>
          </el-table-column>
          <el-table-column prop="winRate" label="胜率" min-width="80" align="right">
            <template #default="{ row }">{{ row.winRate.toFixed(1) }}%</template>
          </el-table-column>
          <el-table-column prop="avgHoldMin" label="平均持仓(分)" min-width="100" align="right" />
          <el-table-column prop="avgWinPct" label="均盈%" min-width="80" align="right">
            <template #default="{ row }">
              <span class="text-green">{{ fmtChg(row.avgWinPct) }}</span>
            </template>
          </el-table-column>
          <el-table-column prop="avgLossPct" label="均亏%" min-width="80" align="right">
            <template #default="{ row }">
              <span class="text-red">{{ fmtChg(row.avgLossPct) }}</span>
            </template>
          </el-table-column>
        </el-table>
      </div>

      <!-- 改进建议 -->
      <div class="summary-section" v-if="modeSummary.suggestions.length">
        <div class="section-title">改进建议</div>
        <ul class="suggest-list">
          <li v-for="(s, i) in modeSummary.suggestions" :key="i">{{ s }}</li>
        </ul>
      </div>
    </div>
  </div>
</template>

<style scoped>
.dashboard-container {
  padding: 16px;
  display: flex;
  flex-direction: column;
  gap: 16px;
}

.card-row {
  display: grid;
  grid-template-columns: repeat(3, 1fr);
  gap: 16px;
}

@media (max-width: 1200px) {
  .card-row {
    grid-template-columns: repeat(2, 1fr);
  }
}

@media (max-width: 768px) {
  .card-row {
    grid-template-columns: 1fr;
  }
}

.quant-card {
  background: var(--quant-card, #1d1f27);
  border: 1px solid var(--quant-border, #2c2f3a);
  border-radius: 10px;
  padding: 18px 20px;
  transition: border-color 0.2s;
}

.quant-card:hover {
  border-color: var(--quant-primary, #4068c9);
}

.card-header {
  display: flex;
  align-items: center;
  justify-content: space-between;
  margin-bottom: 14px;
}

.card-title {
  font-size: 13px;
  font-weight: 500;
  color: var(--quant-text-secondary, #8b8fa3);
  letter-spacing: 0.5px;
}

.card-unit {
  font-size: 11px;
  color: var(--quant-text-secondary, #8b8fa3);
  opacity: 0.7;
}

.card-body {
  display: flex;
  flex-direction: column;
  gap: 12px;
}

/* 状态卡片 */
.status-main {
  display: flex;
  align-items: center;
  gap: 10px;
}

.status-dot {
  width: 10px;
  height: 10px;
  border-radius: 50%;
  flex-shrink: 0;
}

.dot-active {
  background: var(--quant-green, #22c55e);
  box-shadow: 0 0 8px rgba(34, 197, 94, 0.5);
  animation: pulse 2s infinite;
}

.dot-inactive {
  background: var(--quant-red, #ef4444);
  box-shadow: 0 0 6px rgba(239, 68, 68, 0.3);
}

@keyframes pulse {
  0%, 100% { opacity: 1; }
  50% { opacity: 0.5; }
}

.status-mode {
  font-size: 22px;
  font-weight: 600;
  color: var(--quant-text, #e0e0e0);
}

.status-sub {
  display: flex;
  gap: 16px;
  font-size: 12px;
  color: var(--quant-text-secondary, #8b8fa3);
}

/* 数值指标 */
.metric-value {
  font-size: 28px;
  font-weight: 700;
  color: var(--quant-text, #e0e0e0);
  line-height: 1.2;
}

.metric-value.mono {
  font-family: "JetBrains Mono", "Fira Code", "SF Mono", monospace;
  font-size: 26px;
}

.value-unit {
  font-size: 14px;
  font-weight: 400;
  color: var(--quant-text-secondary, #8b8fa3);
  margin-left: 2px;
}

.metric-detail {
  display: flex;
  flex-direction: column;
  gap: 6px;
}

.detail-row {
  display: flex;
  justify-content: space-between;
  align-items: center;
  font-size: 12px;
  color: var(--quant-text-secondary, #8b8fa3);
}

.detail-label {
  opacity: 0.8;
}

/* 颜色编码 */
.text-green {
  color: var(--quant-green, #22c55e) !important;
}

.text-red {
  color: var(--quant-red, #ef4444) !important;
}

.text-neutral {
  color: var(--quant-text, #e0e0e0) !important;
}

.text-orange {
  color: #f59e0b !important;
}

/* 行情加载状态 */
.load-status-card .load-status-body {
  font-size: 13px;
  line-height: 1.6;
  word-break: break-all;
}

.summary-hint {
  font-size: 12px;
  color: var(--quant-text-secondary, #8b8fa3);
}

.summary-tools {
  display: flex;
  align-items: center;
  gap: 10px;
}

/* 每日总结 */
.summary-panel {
  width: 100%;
}

.summary-section {
  margin-top: 16px;
}

.section-title {
  font-size: 14px;
  font-weight: 600;
  color: var(--quant-text, #e0e0e0);
  margin-bottom: 10px;
  border-left: 3px solid var(--quant-primary, #4068c9);
  padding-left: 8px;
}

.summary-grid {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(160px, 1fr));
  gap: 10px;
}

.summary-item {
  display: flex;
  flex-direction: column;
  gap: 4px;
  background: var(--quant-card-sub, #171922);
  border-radius: 8px;
  padding: 10px 12px;
  font-size: 14px;
}

.sum-label {
  font-size: 12px;
  color: var(--quant-text-secondary, #8b8fa3);
}

.sum-sub {
  font-size: 12px;
  color: var(--quant-text-secondary, #8b8fa3);
}

.mover-row {
  display: grid;
  grid-template-columns: 1fr 1fr;
  gap: 16px;
  margin-top: 12px;
}

.mover-title {
  font-size: 12px;
  font-weight: 600;
  margin-bottom: 6px;
}

.mover-item {
  display: flex;
  justify-content: space-between;
  font-size: 12px;
  padding: 2px 0;
  color: var(--quant-text, #e0e0e0);
}

.coin-table {
  margin-top: 12px;
}

.suggest-list {
  margin: 0;
  padding-left: 18px;
  color: var(--quant-text, #e0e0e0);
  font-size: 13px;
  line-height: 1.9;
}
</style>
