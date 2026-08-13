<script setup lang="ts">
/**
 * 插针预防模块
 * 1) 综合解析：插针类型与对应解决方案（基于回测验证）
 * 2) 数据监控：今日插针拦截情况（按小时可视化）
 * 3) 方案效果追踪：主动/被动预防方案的实施结果
 * 4) 建议收集与处理：权威建议库 + 提交/处理流程
 */
import { ref, computed, onMounted, onUnmounted } from "vue";
import * as echarts from "echarts";
import { ElMessage } from "element-plus";
import { QuantService } from "../../../bindings/quant-desktop/internal/bindings";
import { callService } from "../../utils/service";

defineOptions({ name: "WickPrevention" });

const loading = ref(false);
const curMode = ref("");
const today = ref<any>({ markDevBlocks: 0, wickGuardBlocks: 0, closedTotal: 0, stopLossTotal: 0, hourly: [] });
const suggestions = ref<any[]>([]);
const newSuggestion = ref("");
const submitting = ref(false);
let chart: echarts.ECharts | null = null;
let timer: ReturnType<typeof setInterval> | null = null;

/* ========== 综合解析：插针类型 × 方案 ========== */
const wickTypes = [
  {
    type: "成交价插针（单笔极端成交污染价格）",
    desc: "一笔/几笔异常成交把最新价或收盘价打飞，与真实市场价（标记价）明显偏离。",
    sol: "标记价可信度过滤：开仓前核对「收盘价 vs 标记价收盘价」，偏差 >1% 判定可疑不开仓。回测：误杀 0.18%、利润 -0.7%、最大回撤 -13.4%、净利/回撤 +15%。",
    defense: "主动"
  },
  {
    type: "盘中插针触发止损（止损被假跌破打掉）",
    desc: "价格盘中插到止损线，单子被打掉后价格拉回。",
    sol: "收盘判定出场（路1）：3% 止损/跟踪只在 5m 收盘后判定，盘中插针不触发。回测：真实盘中规则=破产，收盘判定+8%兜底=全周期 +37,954U。",
    defense: "主动"
  },
  {
    type: "瞬间插针击穿又弹回（假击穿）",
    desc: "价格瞬间击穿关键位又弹回，纯插针。",
    sol: "插针守卫：窗口内最新价+标记价都击穿且持续足够样本（引擎 45s/2 样本）才确认，本地兜底前不抢跑。",
    defense: "主动"
  },
  {
    type: "假突破（冲高不站稳）",
    desc: "盘中冲 3% 追进去，收盘前回落。",
    sol: "5m 收盘确认开仓：盘中实时只当闹钟，开仓等已收盘 5m 收盘价确认（与回测口径一致）。",
    defense: "主动"
  },
  {
    type: "真崩盘/跳空（硬止损兜底）",
    desc: "价格一根 K 线直接砸穿，不是插针是真跌。",
    sol: "8% 灾难硬止损（交易所市价条件单）：保证成交，认亏 8% 走人。回测兜底 1,402 笔真崩单，回撤 5.6%→7.5%（保险费约 1 万U）。",
    defense: "被动"
  },
  {
    type: "量能异常（低量插针）——已回测否定",
    desc: "直觉认为「量特别小=插针不可信」。",
    sol: "全周期回测显示低量信号根反而更赚钱（单笔 +1.0~1.3U vs 平均 +0.8U），过滤只减利润、PF 下降、回撤不降——不采用（案底 2026-08-14）。",
    defense: "已否决"
  }
];

/* ========== 方案效果追踪 ========== */
const effects = computed(() => [
  { cat: "主动", name: "5m 收盘确认开仓", today: "—（开仓路径）", verdict: "与回测口径一致，避免盘中追尖", cost: "0" },
  { cat: "主动", name: "标记价可信度过滤（1%）", today: `${today.value.markDevBlocks} 次`, verdict: "误杀0.18%、利润-0.7%、回撤-13.4%、净利/回撤+15%", cost: "利润 -0.7%" },
  { cat: "主动", name: "插针守卫（多源+持续确认）", today: `${today.value.wickGuardBlocks} 次`, verdict: "本地兜底前验证，防假击穿抢跑", cost: "0" },
  { cat: "被动", name: "收盘判定出场（路1）", today: "—（全部平仓路径）", verdict: "全周期 +37,954U（vs 盘中触发=破产）", cost: "0" },
  { cat: "被动", name: "8% 灾难硬止损", today: `≈${today.value.stopLossTotal} 次止损`, verdict: "兜底1,402笔真崩，回撤5.6%→7.5%", cost: "保险费约1万U" }
]);

/* ========== 权威建议库（已整理） ========== */
const authoritySuggestions = [
  { title: "多源价格验证", desc: "最新价=摊位喊价、标记价=官方公告牌，两者一致才动作——币安自身防插针逻辑。", src: "用户思路 + 币安机制" },
  { title: "持续时间确认", desc: "不因瞬间击穿动作，需持续足够样本（引擎 45s/2 样本）才确认。", src: "用户伪代码 verify_breakdown" },
  { title: "收盘判定出场（路1）", desc: "盘中插针不触发止损，只在 5m 收盘后判定；回测全周期 +37,954U。", src: "全周期回测（2026-08-14）" },
  { title: "限价止损适用场景", desc: "Stop-Limit 可避免砸坑，但真崩时可能不成交；8% 灾难单必须市价保证成交。", src: "专家评估" },
  { title: "量能验证不采用", desc: "低量信号根回测反而更赚，过滤只减利润——已记录案底，不写入代码。", src: "全周期回测（2026-08-14）" },
  { title: "开仓挂限价捡漏", desc: "插针时挂低于现价限价单可捡便宜——与追涨策略冲突且有挂单限频风险，未采用。", src: "用户思路，专家评估" }
];

const statusMeta: Record<string, { label: string; type: "primary" | "success" | "danger" }> = {
  pending: { label: "待处理", type: "primary" },
  adopted: { label: "已采纳", type: "success" },
  rejected: { label: "已否决", type: "danger" }
};

async function fetchData() {
  const res = await callService(() => QuantService.GetWickPreventionData(), { silent: true });
  if (res && res.ok) {
    curMode.value = res.mode || "";
    today.value = res.today || { markDevBlocks: 0, wickGuardBlocks: 0, closedTotal: 0, stopLossTotal: 0, hourly: [] };
    suggestions.value = res.suggestions || [];
    renderChart();
  }
}

function renderChart() {
  const el = document.getElementById("wick-hourly-chart");
  if (!el) return;
  if (!chart) chart = echarts.init(el);
  const hourly = today.value.hourly || [];
  chart.setOption({
    tooltip: { trigger: "axis" },
    legend: { data: ["标记价过滤拦截", "插针守卫拦下"] },
    grid: { left: 40, right: 16, top: 32, bottom: 24 },
    xAxis: { type: "category", data: hourly.map((h: any) => `${h.hour}:00`) },
    yAxis: { type: "value", minInterval: 1 },
    series: [
      { name: "标记价过滤拦截", type: "bar", data: hourly.map((h: any) => h.markDev), itemStyle: { color: "#f56c6c" } },
      { name: "插针守卫拦下", type: "bar", data: hourly.map((h: any) => h.wickGuard), itemStyle: { color: "#e6a23c" } }
    ]
  });
}

async function submitSuggestion() {
  const text = newSuggestion.value.trim();
  if (!text) {
    ElMessage.warning("请先填写建议内容");
    return;
  }
  submitting.value = true;
  try {
    const res = await callService(() => QuantService.SubmitWickSuggestion(text), { context: "提交建议" });
    if (res && res.ok) {
      ElMessage.success("建议已提交，等待处理");
      newSuggestion.value = "";
      await fetchData();
    }
  } finally {
    submitting.value = false;
  }
}

async function updateStatus(row: any, status: string) {
  const res = await callService(() => QuantService.UpdateWickSuggestion(row.id, status, row.note || ""), { context: "更新建议状态" });
  if (res && res.ok) {
    ElMessage.success("已更新");
    await fetchData();
  }
}

onMounted(async () => {
  await fetchData();
  timer = setInterval(fetchData, 60000);
  window.addEventListener("resize", resizeChart);
});
onUnmounted(() => {
  if (timer) clearInterval(timer);
  window.removeEventListener("resize", resizeChart);
  if (chart) {
    chart.dispose();
    chart = null;
  }
});
function resizeChart() {
  chart?.resize();
}
</script>

<template>
  <div class="wick-container" v-loading="loading">
    <div class="quant-card">
      <div class="card-header">
        <h2 class="card-title">插针预防</h2>
        <el-tag size="small" :type="curMode === 'LIVE' ? 'danger' : 'warning'">{{ curMode === 'LIVE' ? '实盘' : '模拟盘' }}</el-tag>
        <span class="summary-hint">数据每分钟自动刷新 · 回测结论基于 2024-01~2026-08 全周期验证</span>
      </div>
    </div>

    <!-- 1. 综合解析 -->
    <div class="quant-card">
      <div class="card-header"><span class="card-title">① 综合解析：插针类型 × 解决方案</span></div>
      <el-table :data="wickTypes" size="small" stripe>
        <el-table-column prop="type" label="插针类型" min-width="210" />
        <el-table-column prop="desc" label="现象" min-width="230" />
        <el-table-column prop="sol" label="解决方案（含回测依据）" min-width="360" />
        <el-table-column prop="defense" label="分类" width="80" align="center">
          <template #default="{ row }">
            <el-tag size="small" :type="row.defense === '主动' ? 'success' : row.defense === '被动' ? 'warning' : 'info'">{{ row.defense }}</el-tag>
          </template>
        </el-table-column>
      </el-table>
    </div>

    <!-- 2. 数据监控 -->
    <div class="quant-card">
      <div class="card-header"><span class="card-title">② 今日插针监控（真实落库记录）</span></div>
      <div class="stat-row">
        <div class="stat-card">
          <div class="stat-label">标记价过滤拦截</div>
          <div class="stat-value text-red">{{ today.markDevBlocks }} <span class="stat-unit">次</span></div>
          <div class="stat-sub">收盘价与标记价偏差 >1% 未开仓</div>
        </div>
        <div class="stat-card">
          <div class="stat-label">插针守卫拦下</div>
          <div class="stat-value text-orange">{{ today.wickGuardBlocks }} <span class="stat-unit">次</span></div>
          <div class="stat-sub">疑似插针，本地兜底前未抢跑</div>
        </div>
        <div class="stat-card">
          <div class="stat-label">今日平仓</div>
          <div class="stat-value">{{ today.closedTotal }} <span class="stat-unit">笔</span></div>
          <div class="stat-sub">其中止损 {{ today.stopLossTotal }} 笔（含8%灾难兜底）</div>
        </div>
      </div>
      <div id="wick-hourly-chart" style="height: 240px; margin-top: 12px"></div>
    </div>

    <!-- 3. 方案效果追踪 -->
    <div class="quant-card">
      <div class="card-header"><span class="card-title">③ 方案效果追踪（主动 vs 被动）</span></div>
      <el-table :data="effects" size="small" stripe>
        <el-table-column prop="cat" label="分类" width="80" align="center">
          <template #default="{ row }">
            <el-tag size="small" :type="row.cat === '主动' ? 'success' : 'warning'">{{ row.cat }}</el-tag>
          </template>
        </el-table-column>
        <el-table-column prop="name" label="方案" min-width="200" />
        <el-table-column prop="today" label="当日触发" width="130" align="right" />
        <el-table-column prop="verdict" label="有效性（回测/口径）" min-width="320" />
        <el-table-column prop="cost" label="实施成本" width="130" align="right" />
      </el-table>
    </div>

    <!-- 4. 建议收集与处理 -->
    <div class="quant-card">
      <div class="card-header"><span class="card-title">④ 建议收集与处理</span></div>
      <div class="section-title">权威建议库（已整理验证）</div>
      <el-table :data="authoritySuggestions" size="small" stripe>
        <el-table-column prop="title" label="建议" width="180" />
        <el-table-column prop="desc" label="说明" min-width="380" />
        <el-table-column prop="src" label="来源" width="180" />
      </el-table>

      <div class="section-title" style="margin-top: 16px">提交你的建议</div>
      <div class="suggest-form">
        <el-input v-model="newSuggestion" type="textarea" :rows="2" placeholder="例如：建议把标记价偏差阈值从1.0%调到0.8%……" />
        <el-button type="primary" :loading="submitting" @click="submitSuggestion">提交建议</el-button>
      </div>

      <div class="section-title" style="margin-top: 16px">建议处理清单</div>
      <el-table :data="suggestions" size="small" stripe v-if="suggestions.length">
        <el-table-column prop="content" label="建议内容" min-width="300" />
        <el-table-column label="状态" width="90" align="center">
          <template #default="{ row }">
            <el-tag size="small" :type="statusMeta[row.status]?.type || 'primary'">{{ statusMeta[row.status]?.label || row.status }}</el-tag>
          </template>
        </el-table-column>
        <el-table-column label="处理备注" min-width="160">
          <template #default="{ row }">
            <el-input v-model="row.note" size="small" placeholder="处理备注" @change="() => updateStatus(row, row.status)" />
          </template>
        </el-table-column>
        <el-table-column label="操作" width="170">
          <template #default="{ row }">
            <el-button size="small" type="success" plain @click="updateStatus(row, 'adopted')">采纳</el-button>
            <el-button size="small" type="danger" plain @click="updateStatus(row, 'rejected')">否决</el-button>
          </template>
        </el-table-column>
      </el-table>
      <div v-else class="empty-state">暂无待处理建议</div>
    </div>
  </div>
</template>

<style scoped>
.wick-container { padding: 16px; display: flex; flex-direction: column; gap: 16px; }
.card-header { display: flex; align-items: center; gap: 10px; }
.stat-row { display: flex; gap: 16px; }
.stat-card { flex: 1; border: 1px solid var(--el-border-color-light); border-radius: 8px; padding: 14px; }
.stat-label { font-size: 13px; color: var(--el-text-color-secondary); }
.stat-value { font-size: 26px; font-weight: 600; margin: 6px 0; }
.stat-unit { font-size: 13px; font-weight: 400; color: var(--el-text-color-secondary); }
.stat-sub { font-size: 12px; color: var(--el-text-color-secondary); }
.text-orange { color: #e6a23c; }
.text-red { color: #f56c6c; }
.suggest-form { display: flex; gap: 12px; align-items: flex-start; }
.suggest-form .el-button { margin-top: 2px; }
.section-title { font-weight: 600; margin-bottom: 8px; }
.empty-state { color: var(--el-text-color-secondary); padding: 12px 0; }
</style>
