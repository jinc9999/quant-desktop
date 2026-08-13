<script setup lang="ts">
/**
 * 每日总结 - 系统自动生成（市场概况 + 双模式交易总结 + 改进建议）
 * 手动备注为可选补充，保存后进入历史记录；支持模拟盘/实盘自动切换
 */
import { ref, computed, onMounted, onUnmounted } from "vue";
import * as echarts from "echarts";
import { QuantService } from "../../../bindings/quant-desktop/internal/bindings";
import { callService } from "../../utils/service";
import { ElMessage } from "element-plus";

defineOptions({ name: "Summary" });

const mode = ref("SIMULATION");
const activeMode = ref("SIMULATION");
const loading = ref(false);
const list = ref<any[]>([]);
const recon = ref<any>(null);
/** 周期筛选：day=日结记录 / week=周结 / month=月结 / year=年结 */
const period = ref("day");

/** 最新一条带市场解读的记录：跟随周期 Tab（day→日结/晨间/自动；week/month/year→对应周期总结） */
const latestReport = computed(() => {
  const allow = periodTypes(period.value);
  return (
    list.value.find((r: any) => allow.includes(r.summaryType) && r.marketNotes && r.marketNotes.trim()) ??
    null
  );
});
function periodTypes(p: string): string[] {
  const m: Record<string, string[]> = {
    day: ["daily", "morning", "auto"],
    week: ["weekly"],
    month: ["monthly"],
    year: ["yearly"]
  };
  return m[p] || m.day;
}
/** 结构化元数据（feature_json）：市场指标表格 + 大白话归因 */
const reportMeta = computed(() => {
  try {
    return JSON.parse(latestReport.value?.featureJson || "{}") || {};
  } catch {
    return {};
  }
});
/** 策略环境指标：day=当日明细；week/month/year=区间聚合（跨天合计/均值） */
const envMetrics = computed<any[]>(() => {
  const ms = (reportMeta.value.metrics as any[]) || [];
  if (!ms.length) return [];
  const agg = ms.reduce(
    (a, m) => {
      a.pool += m.poolWidth;
      a.oppCoins += m.opportunityCount;
      a.oppTotal += m.opportunityTotal;
      a.burst += m.burstTotal;
      a.fake += m.fakeBreakoutRate;
      a.atr += m.btcATRPct;
      a.up = Math.max(a.up, m.max15mUp);
      a.down = Math.min(a.down, m.max15mDown);
      return a;
    },
    { pool: 0, oppCoins: 0, oppTotal: 0, burst: 0, fake: 0, atr: 0, up: -999, down: 999, n: ms.length }
  );
  const n = Math.max(agg.n, 1);
  const m = ms[ms.length - 1];
  if (period.value === "day") {
    return [
      { k: "机会池宽度", v: `${m.poolWidth} 个币`, d: "24h成交额≥2000万 的合约数" },
      { k: "异动机会", v: `${m.opportunityCount} 币 / ${m.opportunityTotal} 次`, d: "15m单根涨≥3%（策略的肉）" },
      { k: "5m爆拉", v: `${m.burstTotal} 次`, d: "5m单根涨≥2.5%（智慧版1.5倍机会）" },
      { k: "假突破率", v: `${Number(m.fakeBreakoutRate).toFixed(1)}%`, d: "冲3%未站稳占比（高=止损会多）" },
      { k: "最大15m涨/跌", v: `+${Number(m.max15mUp).toFixed(1)}% / ${Number(m.max15mDown).toFixed(1)}%`, d: "当日最猛的单根异动" },
      { k: "BTC波动", v: `ATR ${Number(m.btcATRPct).toFixed(1)}%`, d: "BTC 24h 波动率（高=肉多滑点狠）" }
    ];
  }
  return [
    { k: "累计异动机会", v: `${agg.oppCoins} 币 / ${agg.oppTotal} 次`, d: `${n} 天合计（日均 ${(agg.oppTotal / n).toFixed(1)} 次）` },
    { k: "累计5m爆拉", v: `${agg.burst} 次`, d: `日均 ${(agg.burst / n).toFixed(1)} 次` },
    { k: "平均假突破率", v: `${(agg.fake / n).toFixed(1)}%`, d: "区间平均（高=止损会多）" },
    { k: "平均机会池", v: `${Math.round(agg.pool / n)} 个币`, d: "日均成交额≥2000万 合约数" },
    { k: "最大15m涨/跌", v: `+${agg.up.toFixed(1)}% / ${agg.down.toFixed(1)}%`, d: "区间内最猛单根异动" },
    { k: "平均BTC波动", v: `ATR ${(agg.atr / n).toFixed(1)}%`, d: "区间平均波动率" }
  ];
});
/** 大白话归因 */
const attribution = computed(() => reportMeta.value.attribution || "");
/** 按周期过滤后的历史记录 */
const filteredList = computed(() => {
  const allow = periodTypes(period.value);
  return list.value.filter((r) => allow.includes(r.summaryType));
});
const auto = ref<any>({ market: {}, modes: {}, currentMode: "SIMULATION" });

/** 当前展示模式的自动总结 */
const modeSummary = computed(() => {
  const modes = auto.value.modes || {};
  return modes[activeMode.value] || { trades: {}, suggestions: [] };
});

const typeLabel = (t: string) =>
  t === "weekly" ? "周结" : t === "monthly" ? "月结" : t === "yearly" ? "年结" : t === "auto" ? "自动" : t === "morning" ? "晨间" : "每日";
let listTimer: ReturnType<typeof setInterval> | null = null;
let chart: echarts.ECharts | null = null;

async function loadMode() {
  const res = await callService(() => QuantService.GetMode(), { silent: true });
  if (res) mode.value = res;
}

/** 拉取系统自动总结（市场全局 + 模拟盘/实盘双模式） */
async function fetchAuto() {
  const res = await callService(() => QuantService.GetDailySummary(), { silent: true });
  if (res) {
    auto.value = res;
    if (res.currentMode) activeMode.value = res.currentMode;
  }
}

/** 拉取历史备注列表（用于趋势图） */
async function fetchList() {
  const res = await callService(() => QuantService.GetDailySummaries("", "", "all"), {
    silent: true
  });
  if (res && res.ok) {
    // 日记录按天去重（优先 每日>晨间>自动）；周/月/年总结独立保留（不受按天去重影响）
    const rank: Record<string, number> = { daily: 3, morning: 2, auto: 1 };
    const byDate = new Map<string, any>();
    for (const r of res.list ?? []) {
      if (r.summaryType === "weekly" || r.summaryType === "monthly" || r.summaryType === "yearly") {
        continue; // 周期总结在后面单独并入
      }
      const cur = byDate.get(r.summaryDate);
      if (!cur || (rank[r.summaryType] ?? 0) > (rank[cur.summaryType] ?? 0)) {
        byDate.set(r.summaryDate, r);
      }
    }
    const periodRows = (res.list ?? []).filter((r: any) =>
      ["weekly", "monthly", "yearly"].includes(r.summaryType)
    );
    list.value = [...byDate.values(), ...periodRows].sort((a, b) =>
      a.summaryDate < b.summaryDate ? 1 : -1
    );
    renderChart();
  }
}

/** 拉取账户对账（权益 vs 本地统计），一眼看出账目差额 */
async function fetchRecon() {
  const res = await callService(() => QuantService.GetAccountReconciliation(), { silent: true });
  if (res) recon.value = res;
}

function renderChart() {
  const el = document.getElementById("pnl-chart");
  if (!el) return;
  if (!chart) chart = echarts.init(el);
  const rows = [...filteredList.value].reverse();
  chart.setOption(
    {
      tooltip: { trigger: "axis" },
      grid: { left: 70, right: 24, top: 34, bottom: 34 },
      xAxis: { type: "category", data: rows.map(r => r.summaryDate) },
      yAxis: { type: "value", name: "盈亏(U)" },
      series: [
        {
          name: "今日盈亏",
          type: "line",
          data: rows.map(r => r.todayPnl ?? 0),
          smooth: true,
          areaStyle: { opacity: 0.15 }
        }
      ]
    },
    true
  );
}

function fmtNum(v: number, signed = false): string {
  if (v === null || v === undefined || isNaN(v)) return "--";
  const s = Math.abs(v).toFixed(2);
  if (signed && v > 0) return "+" + s;
  return v < 0 ? "-" + s : s;
}

function fmtChg(v: number): string {
  if (v === undefined || v === null) return "--";
  return (v > 0 ? "+" : "") + Number(v).toFixed(2) + "%";
}

function fmtVolume(v: number): string {
  if (!v || v <= 0) return "--";
  if (v >= 1e8) return (v / 1e8).toFixed(1) + " 亿";
  if (v >= 1e4) return (v / 1e4).toFixed(0) + " 万";
  return v.toFixed(0);
}

function chgClass(v: number): string {
  if (v > 0) return "text-green";
  if (v < 0) return "text-red";
  return "text-neutral";
}

function onResize() {
  if (chart) chart.resize();
}

onMounted(async () => {
  await loadMode();
  await fetchAuto();
  await fetchList();
  await fetchRecon();
  listTimer = setInterval(() => {
    fetchAuto();
    fetchList();
    fetchRecon();
  }, 60000);
  window.addEventListener("resize", onResize);
});

onUnmounted(() => {
  if (listTimer) clearInterval(listTimer);
  window.removeEventListener("resize", onResize);
  if (chart) {
    chart.dispose();
    chart = null;
  }
});
</script>

<template>
  <div class="summary-container">
    <div class="quant-card">
      <div class="card-header">
        <h2 class="card-title">每日总结（系统自动生成）</h2>
        <div class="summary-tools">
          <el-radio-group v-model="activeMode" size="small">
            <el-radio-button value="SIMULATION">模拟盘</el-radio-button>
            <el-radio-button value="LIVE">实盘</el-radio-button>
          </el-radio-group>
          <span class="summary-hint">每 60 秒自动刷新</span>
        </div>
      </div>

      <!-- 概览条 -->
      <div class="overview-grid">
        <div class="overview-item">
          <span class="ov-label">今日盈亏</span>
          <span class="ov-value" :class="chgClass(modeSummary.trades.todayPnl ?? 0)">
            {{ fmtNum(modeSummary.trades.todayPnl ?? 0, true) }} U
          </span>
        </div>
        <div class="overview-item">
          <span class="ov-label">胜率</span>
          <span class="ov-value">{{ (modeSummary.trades.winRate ?? 0).toFixed(1) }}%</span>
        </div>
        <div class="overview-item">
          <span class="ov-label">交易数</span>
          <span class="ov-value">{{ modeSummary.trades.closedCount ?? 0 }}</span>
        </div>
        <div class="overview-item ov-wide">
          <span class="ov-label">市场一句话</span>
          <span class="ov-value">{{ attribution || (latestReport ? "已生成市场解读" : "等待数据采集") }}</span>
        </div>
      </div>

      <!-- 归因与结论（大白话） -->
      <div class="summary-section" v-if="attribution">
        <div class="section-title">
          归因与结论（{{ latestReport?.summaryDate }} · {{ typeLabel(latestReport?.summaryType) }}）
        </div>
        <div class="attribution-box">{{ attribution }}</div>
      </div>

      <!-- 账户对账（权益 vs 本地统计） -->
      <div class="summary-section" v-if="recon && recon.ok">
        <div class="section-title">账户对账（权益 vs 本地统计）</div>
        <div class="summary-grid">
          <div class="summary-item">
            <span class="sum-label">账户权益</span>
            <span>{{ fmtNum(recon.equity) }} U</span>
          </div>
          <div class="summary-item">
            <span class="sum-label">真实累计盈亏</span>
            <span :class="chgClass(recon.trueNet)">{{ fmtNum(recon.trueNet, true) }} U</span>
          </div>
          <div class="summary-item">
            <span class="sum-label">本地累计盈亏</span>
            <span :class="chgClass(recon.localNet)">{{ fmtNum(recon.localNet, true) }} U</span>
          </div>
          <div class="summary-item">
            <span class="sum-label">账目差额</span>
            <span :class="Math.abs(recon.diff) > 1 ? 'text-red' : 'text-green'">
              {{ fmtNum(recon.diff, true) }} U
            </span>
          </div>
        </div>
        <div class="draft-hint" v-if="Math.abs(recon.diff) > 1">
          本地统计与账户存在差额（多为离线幽灵单/强平清算/成交价差），可用对账回填工具处理。
        </div>
      </div>

      <!-- 市场概况（全局，仅日视图展示当日大盘） -->
      <div class="summary-section" v-if="period === 'day'">
        <div class="section-title">市场环境 · 大盘（24h，两模式共用）</div>
        <div class="summary-grid">
          <div class="summary-item">
            <span class="sum-label">上涨 / 下跌</span>
            <span>
              <b class="text-green">{{ auto.market.up ?? 0 }}</b> /
              <b class="text-red">{{ auto.market.down ?? 0 }}</b>
              <span class="sum-sub">（共 {{ auto.market.total ?? 0 }}）</span>
            </span>
          </div>
          <div class="summary-item">
            <span class="sum-label">中位涨跌</span>
            <span :class="chgClass(auto.market.medianChange ?? 0)">{{ fmtChg(auto.market.medianChange) }}</span>
          </div>
          <div class="summary-item">
            <span class="sum-label">平均涨跌</span>
            <span :class="chgClass(auto.market.avgChange ?? 0)">{{ fmtChg(auto.market.avgChange) }}</span>
          </div>
          <div class="summary-item">
            <span class="sum-label">总成交额</span>
            <span>{{ fmtVolume(auto.market.totalQuoteVolume) }} U</span>
          </div>
        </div>
        <div class="mover-row" v-if="auto.market.topGainers && auto.market.topGainers.length">
          <div class="mover-col">
            <div class="mover-title text-green">领涨</div>
            <div v-for="g in auto.market.topGainers.slice(0, 5)" :key="g.symbol" class="mover-item">
              <span>{{ g.symbol }}</span>
              <span class="text-green">{{ fmtChg(g.change) }}</span>
            </div>
          </div>
          <div class="mover-col">
            <div class="mover-title text-red">领跌</div>
            <div v-for="g in auto.market.topLosers.slice(0, 5)" :key="g.symbol" class="mover-item">
              <span>{{ g.symbol }}</span>
              <span class="text-red">{{ fmtChg(g.change) }}</span>
            </div>
          </div>
        </div>
      </div>

      <!-- 策略环境（今日市场指标，market_metrics 采集） -->
      <div class="summary-section" v-if="envMetrics.length">
        <div class="section-title">市场环境 · 策略视角（今天有没有肉）</div>
        <el-table :data="envMetrics" size="small" stripe>
          <el-table-column prop="k" label="指标" min-width="130" />
          <el-table-column prop="v" label="数值" min-width="170" align="right" />
          <el-table-column prop="d" label="解读" min-width="280" />
          <template #empty>
            <div class="empty-state">今天还没采集市场指标，运行 market_metrics collect 后自动展示</div>
          </template>
        </el-table>
      </div>

      <!-- 当前模式交易总结（仅日视图） -->
      <div class="summary-section" v-if="period === 'day'">
        <div class="section-title">
          今日交易（{{ activeMode === "LIVE" ? "实盘" : "模拟盘" }}）
        </div>
        <div class="summary-grid">
          <div class="summary-item">
            <span class="sum-label">平仓笔数</span>
            <span>{{ modeSummary.trades.closedCount ?? 0 }}</span>
          </div>
          <div class="summary-item">
            <span class="sum-label">今日盈亏</span>
            <span :class="chgClass(modeSummary.trades.todayPnl ?? 0)">
              {{ fmtNum(modeSummary.trades.todayPnl ?? 0, true) }} U
            </span>
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
            <template #default="{ row }"><span class="text-green">{{ fmtChg(row.avgWinPct) }}</span></template>
          </el-table-column>
          <el-table-column prop="avgLossPct" label="均亏%" min-width="80" align="right">
            <template #default="{ row }"><span class="text-red">{{ fmtChg(row.avgLossPct) }}</span></template>
          </el-table-column>
        </el-table>
      </div>

      <!-- 改进建议（自动，仅日视图） -->
      <div class="summary-section" v-if="period === 'day' && modeSummary.suggestions.length">
        <div class="section-title">改进建议（自动生成）</div>
        <ul class="suggest-list">
          <li v-for="(s, i) in modeSummary.suggestions" :key="i">{{ s }}</li>
        </ul>
      </div>
    </div>

    <!-- 历史与趋势 -->
    <div class="quant-card">
      <div class="card-header">
        <h2 class="card-title">历史与趋势（{{ filteredList.length }} 条）</h2>
        <div class="summary-tools">
          <el-radio-group v-model="period" size="small">
            <el-radio-button value="day">日</el-radio-button>
            <el-radio-button value="week">周</el-radio-button>
            <el-radio-button value="month">月</el-radio-button>
            <el-radio-button value="year">年</el-radio-button>
          </el-radio-group>
          <span class="summary-hint">运行中每小时自动更新当天记录</span>
        </div>
      </div>
      <div id="pnl-chart" class="chart-box"></div>
      <el-table :data="filteredList" v-loading="loading" size="small" stripe class="history-table">
        <el-table-column prop="summaryDate" label="日期" min-width="100" />
        <el-table-column label="类型" width="70" align="center">
          <template #default="{ row }">{{ typeLabel(row.summaryType) }}</template>
        </el-table-column>
        <el-table-column prop="todayPnl" label="盈亏(U)" min-width="90" align="right">
          <template #default="{ row }">
            <span :class="chgClass(row.todayPnl)">{{ fmtNum(row.todayPnl, true) }}</span>
          </template>
        </el-table-column>
        <el-table-column prop="winRate" label="胜率%" min-width="80" align="right" />
        <el-table-column prop="tradeCount" label="交易" min-width="70" align="right" />
        <el-table-column label="更新时间" min-width="160">
          <template #default="{ row }">
            {{ new Date(row.updatedAt).toLocaleString("zh-CN", { hour12: false }) }}
          </template>
        </el-table-column>
        <template #empty>
          <div class="empty-state">暂无历史记录，策略运行约 1 小时后自动生成</div>
        </template>
      </el-table>
    </div>
  </div>
</template>

<style scoped>
.summary-container {
  padding: 16px;
  display: flex;
  flex-direction: column;
  gap: 16px;
}

.overview-grid {
  display: grid;
  grid-template-columns: repeat(4, 1fr);
  gap: 12px;
  margin-bottom: 4px;
}
.overview-item {
  display: flex;
  flex-direction: column;
  gap: 4px;
  padding: 12px 14px;
  background: rgba(255, 255, 255, 0.03);
  border: 1px solid var(--quant-border, #2c2f3a);
  border-radius: 8px;
}
.overview-item.ov-wide {
  grid-column: span 2;
}
.ov-label {
  font-size: 12px;
  color: var(--quant-text-secondary, #9ca3af);
}
.ov-value {
  font-size: 20px;
  font-weight: 700;
  line-height: 1.2;
  color: var(--quant-text, #e0e0e0);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.attribution-box {
  padding: 10px 14px;
  background: rgba(255, 255, 255, 0.03);
  border: 1px solid var(--quant-border, #2c2f3a);
  border-radius: 8px;
  font-size: 13px;
  line-height: 1.7;
  color: var(--quant-text, #e0e0e0);
}
.quant-card {
  background: var(--quant-card, #1d1f27);
  border: 1px solid var(--quant-border, #2c2f3a);
  border-radius: 10px;
  padding: 18px 20px;
}

.card-header {
  display: flex;
  align-items: center;
  justify-content: space-between;
  margin-bottom: 14px;
}

.card-title {
  margin: 0;
  font-size: 17px;
  color: var(--quant-text, #e0e0e0);
}

.summary-tools {
  display: flex;
  align-items: center;
  gap: 10px;
}

.summary-hint {
  font-size: 12px;
  color: var(--quant-text-secondary, #8b8fa3);
}

.summary-section {
  margin-top: 14px;
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

.notes-summary {
  font-size: 13px;
  color: var(--quant-text-secondary, #8b8fa3);
  cursor: pointer;
}

.notes-body {
  margin-top: 12px;
}

.notes-grid {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 10px;
  margin-bottom: 10px;
}

.notes-input {
  margin-bottom: 10px;
}

.notes-actions {
  display: flex;
  align-items: center;
  gap: 10px;
}

.draft-hint {
  font-size: 12px;
  color: var(--quant-text-secondary, #8b8fa3);
}

.chart-box {
  height: 200px;
  width: 100%;
  margin-bottom: 12px;
}

.text-green {
  color: #22c55e !important;
}
.text-red {
  color: #ef4444 !important;
}
.text-neutral {
  color: var(--quant-text, #e0e0e0) !important;
}

.market-note {
  margin: 8px 0 0;
  padding: 10px 12px;
  background: rgba(127, 127, 127, 0.08);
  border-radius: 6px;
  white-space: pre-line;
  line-height: 1.7;
  font-size: 13px;
  font-family: inherit;
}
</style>
