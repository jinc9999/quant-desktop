<script setup lang="ts">
/**
 * 每日总结 - 系统自动生成（市场概况 + 双模式交易总结 + 改进建议）
 * 手动备注为可选补充，保存后进入历史记录；支持模拟盘/实盘自动切换
 */
import { ref, reactive, computed, watch, onMounted, onUnmounted } from "vue";
import * as echarts from "echarts";
import { QuantService } from "../../../bindings/quant-desktop/internal/bindings";
import { callService } from "../../utils/service";
import { ElMessage, ElMessageBox } from "element-plus";

defineOptions({ name: "Summary" });

const mode = ref("SIMULATION");
const activeMode = ref("SIMULATION");
const loading = ref(false);
const saving = ref(false);
const list = ref<any[]>([]);
const auto = ref<any>({ market: {}, modes: {}, currentMode: "SIMULATION" });

/** 当前展示模式的自动总结 */
const modeSummary = computed(() => {
  const modes = auto.value.modes || {};
  return modes[activeMode.value] || { trades: {}, suggestions: [] };
});

/** 可选补充备注（非必填） */
const notes = reactive({
  summaryDate: new Date().toISOString().slice(0, 10),
  summaryType: "daily",
  marketNotes: "",
  coinAnalysis: "",
  suggestions: "",
  todayPnl: 0,
  winRate: 0,
  tradeCount: 0,
  rating: 0,
  featureJson: "{}"
});

const draftKey = computed(() => `daily_summary_draft_${mode.value}`);
const typeLabel = (t: string) => (t === "weekly" ? "周结" : "每日");
let saveTimer: ReturnType<typeof setTimeout> | null = null;
let listTimer: ReturnType<typeof setInterval> | null = null;
let chart: echarts.ECharts | null = null;

async function loadMode() {
  const res = await callService(() => QuantService.GetMode(), { silent: true });
  if (res) mode.value = res;
}

function saveDraft() {
  try {
    localStorage.setItem(draftKey.value, JSON.stringify(notes));
  } catch {}
}

function restoreDraft() {
  try {
    const raw = localStorage.getItem(draftKey.value);
    if (raw) Object.assign(notes, JSON.parse(raw));
  } catch {}
}

watch(
  notes,
  () => {
    if (saveTimer) clearTimeout(saveTimer);
    saveTimer = setTimeout(saveDraft, 1500);
  },
  { deep: true }
);

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
    list.value = res.list ?? [];
    renderChart();
  }
}

function renderChart() {
  const el = document.getElementById("pnl-chart");
  if (!el) return;
  if (!chart) chart = echarts.init(el);
  const rows = [...list.value].reverse();
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

/** 保存可选备注（写入当前模式库） */
async function saveNotes() {
  if (!notes.summaryDate) {
    ElMessage.warning("请选择日期");
    return;
  }
  saving.value = true;
  const res = await callService(() => QuantService.SaveDailySummary({ ...notes }), {
    silent: false
  });
  saving.value = false;
  if (res && res.ok) {
    ElMessage.success(res.message || "备注已保存");
    localStorage.removeItem(draftKey.value);
    await fetchList();
  } else {
    ElMessage.error(res?.message || "保存失败");
  }
}

async function edit(row: any) {
  const res = await callService(() => QuantService.GetDailySummaryByID(row.id), {
    silent: true
  });
  if (res && res.ok && res.item) {
    const it = res.item;
    Object.assign(notes, {
      summaryDate: it.summaryDate,
      summaryType: it.summaryType || "daily",
      marketNotes: it.marketNotes || "",
      coinAnalysis: it.coinAnalysis || "",
      suggestions: it.suggestions || "",
      todayPnl: it.todayPnl ?? 0,
      winRate: it.winRate ?? 0,
      tradeCount: it.tradeCount ?? 0,
      rating: it.rating ?? 0,
      featureJson: it.featureJson || "{}"
    });
    ElMessage.success("已载入编辑，保存后覆盖该日期记录");
  }
}

async function remove(row: any) {
  try {
    await ElMessageBox.confirm(`确定删除 ${row.summaryDate} 的备注？`, "删除确认", {
      type: "warning",
      confirmButtonText: "删除",
      cancelButtonText: "取消"
    });
  } catch {
    return;
  }
  const res = await callService(() => QuantService.DeleteDailySummary(row.id), {
    silent: false
  });
  if (res && res.ok) {
    ElMessage.success("已删除");
    fetchList();
  }
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

onMounted(async () => {
  await loadMode();
  restoreDraft();
  await fetchAuto();
  await fetchList();
  listTimer = setInterval(() => {
    fetchAuto();
    fetchList();
  }, 60000);
  window.addEventListener("resize", () => chart && chart.resize());
});

onUnmounted(() => {
  if (listTimer) clearInterval(listTimer);
  if (saveTimer) clearTimeout(saveTimer);
  window.removeEventListener("resize", () => chart && chart.resize());
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

      <!-- 市场概况（全局） -->
      <div class="summary-section">
        <div class="section-title">市场概况（24h，两模式共用）</div>
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

      <!-- 当前模式交易总结 -->
      <div class="summary-section">
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

      <!-- 改进建议（自动） -->
      <div class="summary-section" v-if="modeSummary.suggestions.length">
        <div class="section-title">改进建议（自动生成）</div>
        <ul class="suggest-list">
          <li v-for="(s, i) in modeSummary.suggestions" :key="i">{{ s }}</li>
        </ul>
      </div>
    </div>

    <!-- 可选补充备注 + 历史 -->
    <div class="quant-card notes-card">
      <details>
        <summary class="notes-summary">补充备注（可选）— 手动记录市场观察，保存后进入历史</summary>
        <div class="notes-body">
          <div class="notes-grid">
            <el-date-picker
              v-model="notes.summaryDate"
              type="date"
              value-format="YYYY-MM-DD"
              placeholder="选择日期"
              style="width: 160px"
            />
            <el-radio-group v-model="notes.summaryType">
              <el-radio-button value="daily">每日</el-radio-button>
              <el-radio-button value="weekly">周结</el-radio-button>
            </el-radio-group>
            <el-input-number v-model="notes.todayPnl" :precision="2" placeholder="盈亏(U)" />
            <el-input-number v-model="notes.winRate" :min="0" :max="100" :precision="1" placeholder="胜率%" />
            <el-input-number v-model="notes.tradeCount" :min="0" placeholder="交易次数" />
            <el-rate v-model="notes.rating" :max="10" />
          </div>
          <el-input
            v-model="notes.marketNotes"
            type="textarea"
            :rows="2"
            placeholder="市场观察备注（可选）"
            class="notes-input"
          />
          <el-input
            v-model="notes.coinAnalysis"
            type="textarea"
            :rows="2"
            placeholder="单币分析备注（可选）"
            class="notes-input"
          />
          <el-input
            v-model="notes.suggestions"
            type="textarea"
            :rows="2"
            placeholder="建议备注（可选）"
            class="notes-input"
          />
          <div class="notes-actions">
            <el-button type="primary" size="small" :loading="saving" @click="saveNotes">
              保存备注
            </el-button>
            <span class="draft-hint">✎ 自动存草稿到本机</span>
          </div>
        </div>
      </details>
    </div>

    <!-- 历史 + 趋势图 -->
    <div class="quant-card">
      <div class="card-header">
        <h2 class="card-title">备注历史与盈亏趋势（{{ list.length }}）</h2>
      </div>
      <div id="pnl-chart" class="chart-box"></div>
      <el-table :data="list" v-loading="loading" size="small" stripe class="history-table">
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
        <el-table-column prop="rating" label="评分" min-width="70" align="center" />
        <el-table-column label="操作" width="130" align="center" fixed="right">
          <template #default="{ row }">
            <el-button link type="primary" size="small" @click="edit(row)">编辑</el-button>
            <el-button link type="danger" size="small" @click="remove(row)">删除</el-button>
          </template>
        </el-table-column>
        <template #empty>
          <div class="empty-state">暂无备注记录</div>
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
</style>
