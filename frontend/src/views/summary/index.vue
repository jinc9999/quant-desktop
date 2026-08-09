<script setup lang="ts">
/**
 * 每日总结 - 手动录入/编辑/提交 + 本地草稿 + 历史列表 + 盈亏趋势图
 * 数据按模式（模拟盘/实盘）物理分库隔离，提交走后端 CRUD + 审计日志
 */
import { ref, reactive, computed, watch, onMounted, onUnmounted } from "vue";
import * as echarts from "echarts";
import { QuantService } from "../../../bindings/quant-desktop/internal/bindings";
import { callService } from "../../utils/service";
import { ElMessage, ElMessageBox } from "element-plus";

defineOptions({ name: "Summary" });

const mode = ref("SIMULATION");
const loading = ref(false);
const saving = ref(false);
const list = ref<any[]>([]);

const form = reactive({
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
    localStorage.setItem(draftKey.value, JSON.stringify(form));
  } catch {}
}

function restoreDraft() {
  try {
    const raw = localStorage.getItem(draftKey.value);
    if (raw) Object.assign(form, JSON.parse(raw));
  } catch {}
}

watch(
  form,
  () => {
    if (saveTimer) clearTimeout(saveTimer);
    saveTimer = setTimeout(saveDraft, 1500);
  },
  { deep: true }
);

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

async function fetchList() {
  const res = await callService(() => QuantService.GetDailySummaries("", "", "all"), {
    silent: true
  });
  if (res && res.ok) {
    list.value = res.list ?? [];
    renderChart();
  }
}

async function save() {
  if (!form.summaryDate) {
    ElMessage.warning("请选择日期");
    return;
  }
  saving.value = true;
  const res = await callService(() => QuantService.SaveDailySummary({ ...form }), {
    silent: false
  });
  saving.value = false;
  if (res && res.ok) {
    ElMessage.success(res.message || "已保存");
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
    Object.assign(form, {
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
  } else {
    ElMessage.error(res?.message || "载入失败");
  }
}

async function remove(row: any) {
  try {
    await ElMessageBox.confirm(`确定删除 ${row.summaryDate} 的总结？`, "删除确认", {
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
  } else {
    ElMessage.error(res?.message || "删除失败");
  }
}

function fmtNum(v: number, signed = false): string {
  if (v === null || v === undefined || isNaN(v)) return "--";
  const s = Math.abs(v).toFixed(2);
  if (signed && v > 0) return "+" + s;
  return v < 0 ? "-" + s : s;
}

function chgClass(v: number): string {
  if (v > 0) return "text-green";
  if (v < 0) return "text-red";
  return "text-neutral";
}

onMounted(async () => {
  await loadMode();
  restoreDraft();
  await fetchList();
  listTimer = setInterval(fetchList, 60000);
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
    <div class="summary-layout">
      <!-- 左侧：录入表单 -->
      <div class="quant-card form-card">
        <div class="card-header">
          <h2 class="card-title">每日总结录入</h2>
          <el-tag size="small" :type="mode === 'LIVE' ? 'danger' : 'info'">
            {{ mode === "LIVE" ? "实盘" : "模拟盘" }}
          </el-tag>
        </div>
        <div class="form-grid">
          <el-form label-width="80px" label-position="left">
            <el-form-item label="日期">
              <el-date-picker
                v-model="form.summaryDate"
                type="date"
                value-format="YYYY-MM-DD"
                placeholder="选择日期"
                style="width: 100%"
              />
            </el-form-item>
            <el-form-item label="类型">
              <el-radio-group v-model="form.summaryType">
                <el-radio-button value="daily">每日</el-radio-button>
                <el-radio-button value="weekly">周结</el-radio-button>
              </el-radio-group>
            </el-form-item>
            <el-form-item label="今日盈亏">
              <el-input-number v-model="form.todayPnl" :precision="2" :step="1" style="width: 100%" />
            </el-form-item>
            <el-form-item label="胜率 %">
              <el-input-number v-model="form.winRate" :min="0" :max="100" :precision="1" :step="1" style="width: 100%" />
            </el-form-item>
            <el-form-item label="交易次数">
              <el-input-number v-model="form.tradeCount" :min="0" :step="1" style="width: 100%" />
            </el-form-item>
            <el-form-item label="体验评分">
              <el-rate v-model="form.rating" :max="10" show-score />
            </el-form-item>
          </el-form>
        </div>

        <div class="text-fields">
          <div class="field-block">
            <div class="field-label">市场概况</div>
            <el-input
              v-model="form.marketNotes"
              type="textarea"
              :rows="4"
              placeholder="大盘趋势、涨跌结构、热点板块…"
            />
          </div>
          <div class="field-block">
            <div class="field-label">单币盈亏分析</div>
            <el-input
              v-model="form.coinAnalysis"
              type="textarea"
              :rows="4"
              placeholder="哪些币贡献/拖累收益、进场与出场质量…"
            />
          </div>
          <div class="field-block">
            <div class="field-label">改进建议</div>
            <el-input
              v-model="form.suggestions"
              type="textarea"
              :rows="3"
              placeholder="策略/风控/执行的改进方向…"
            />
          </div>
          <details class="feature-field">
            <summary>ML 特征扩展字段（featureJson，可选）</summary>
            <el-input
              v-model="form.featureJson"
              type="textarea"
              :rows="2"
              placeholder='{"breadth": 0.58, "volatility": 1.2}'
            />
          </details>
        </div>

        <div class="form-actions">
          <el-button type="primary" :loading="saving" @click="save">保存总结</el-button>
          <span class="draft-hint">✎ 输入内容每 1.5 秒自动保存草稿到本机</span>
        </div>
      </div>

      <!-- 右侧：历史列表 + 趋势图 -->
      <div class="right-col">
        <div class="quant-card">
          <div class="card-header">
            <h2 class="card-title">盈亏趋势</h2>
          </div>
          <div id="pnl-chart" class="chart-box"></div>
        </div>
        <div class="quant-card">
          <div class="card-header">
            <h2 class="card-title">历史总结（{{ list.length }}）</h2>
          </div>
          <el-table :data="list" v-loading="loading" size="small" stripe height="360">
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
              <div class="empty-state">暂无总结，保存一条试试</div>
            </template>
          </el-table>
        </div>
      </div>
    </div>
  </div>
</template>

<style scoped>
.summary-container {
  padding: 16px;
}

.summary-layout {
  display: grid;
  grid-template-columns: minmax(360px, 480px) 1fr;
  gap: 16px;
  align-items: start;
}

@media (max-width: 1100px) {
  .summary-layout {
    grid-template-columns: 1fr;
  }
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

.right-col {
  display: flex;
  flex-direction: column;
  gap: 16px;
}

.chart-box {
  height: 200px;
  width: 100%;
}

.text-fields {
  display: flex;
  flex-direction: column;
  gap: 12px;
  margin-top: 12px;
}

.field-label {
  font-size: 12px;
  color: var(--quant-text-secondary, #8b8fa3);
  margin-bottom: 6px;
}

.feature-field {
  font-size: 12px;
  color: var(--quant-text-secondary, #8b8fa3);
}

.form-actions {
  display: flex;
  align-items: center;
  gap: 12px;
  margin-top: 16px;
}

.draft-hint {
  font-size: 12px;
  color: var(--quant-text-secondary, #8b8fa3);
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
