<script setup lang="ts">
/**
 * 每日策略总结（自动生成）
 * 数据源：market_metrics analyze（每小时自动运行）写入 daily_summaries type=strategy
 * 板块：① 策略口径当日模拟 ② 三桶分析（爆拉/中间/温和）
 */
import { ref, computed, onMounted, onUnmounted } from "vue";
import { QuantService } from "../../../bindings/quant-desktop/internal/bindings";
import { callService } from "../../utils/service";

defineOptions({ name: "StrategySummary" });

const loading = ref(false);
const rows = ref<any[]>([]);
const updatedAt = ref<string>("");
let timer: ReturnType<typeof setInterval> | null = null;

const latest = computed(() => {
  const rec = rows.value.find((r: any) => r.featureJson) || rows.value[0];
  if (!rec) return null;
  try {
    return { date: rec.summaryDate, meta: JSON.parse(rec.featureJson || "{}"), updatedAt: rec.updatedAt };
  } catch {
    return { date: rec.summaryDate, meta: {}, updatedAt: rec.updatedAt };
  }
});

const sim = computed(() => latest.value?.meta.sim || null);
const buckets = computed<any[]>(() => latest.value?.meta.buckets || []);

/** 策略模拟指标行 */
const simRows = computed<any[]>(() => {
  const s = sim.value;
  if (!s) return [];
  return [
    { k: "15m 信号", v: s.signals, d: "收盘 vs 15m 周期开盘 ≥3%" },
    { k: "开仓次数", v: `${s.opens}（追单 ${s.addons}）`, d: "含冷却过滤，同币最多 1+2 仓" },
    { k: "已平仓", v: s.closed, d: "当日按规则平出的仓" },
    { k: "胜率", v: `${Number(s.winRate).toFixed(1)}%`, d: "盈利笔数 / 平仓笔数" },
    { k: "盈亏", v: `${fmtNum(s.pnl, true)} U`, d: "按 100U 名义/仓 × 桶倍数" },
    { k: "平均每笔", v: `${fmtNum(s.avg, true)} U`, d: "" },
    { k: "止损 / 跟踪 / 超时", v: `${s.stop} / ${s.trail} / ${s.maxhold}`, d: "平仓原因分布" },
    { k: "追单平仓", v: s.addonClosed, d: "追单独立离场笔数" }
  ];
});

function fmtNum(v: number, signed = false): string {
  if (v === null || v === undefined || isNaN(v)) return "--";
  const a = Math.abs(v).toFixed(2);
  if (signed && v > 0) return "+" + a;
  return v < 0 ? "-" + a : a;
}

function chgClass(v: number): string {
  if (v > 0) return "text-green";
  if (v < 0) return "text-red";
  return "text-neutral";
}

async function fetchData() {
  const res = await callService(() => QuantService.GetDailySummaries("", "", "strategy"), { silent: true });
  if (res && res.ok) {
    rows.value = [...(res.list ?? [])].sort((a, b) => (a.summaryDate < b.summaryDate ? 1 : -1));
    if (rows.value[0]?.updatedAt) {
      updatedAt.value = new Date(rows.value[0].updatedAt).toLocaleString("zh-CN", { hour12: false });
    }
  }
}

onMounted(async () => {
  await fetchData();
  timer = setInterval(fetchData, 60000);
});

onUnmounted(() => {
  if (timer) clearInterval(timer);
});
</script>

<template>
  <div class="summary-container">
    <div class="quant-card">
      <div class="card-header">
        <h2 class="card-title">每日策略总结（自动生成）</h2>
        <span class="summary-hint">每小时自动更新 · {{ updatedAt || "等待数据" }}</span>
      </div>

      <!-- 板块一：策略口径当日模拟 -->
      <div class="summary-section">
        <div class="section-title">
          策略口径当日模拟{{ latest?.date ? "（" + latest.date + "）" : "" }}
        </div>
        <div class="rule-note">
          规则：15m≥3% 入仓 → 涨 2% 激活移动止盈（回调 3%）→ 激活后再次信号追单（同币最多 1+2 仓）→ 止损 -3% / 跟踪 / 180 分钟超时 / 分原因冷却
        </div>
        <el-table v-if="simRows.length" :data="simRows" size="small" stripe v-loading="loading">
          <el-table-column prop="k" label="指标" min-width="150" />
          <el-table-column prop="v" label="数值" min-width="160" align="right" />
          <el-table-column prop="d" label="说明" min-width="280" />
        </el-table>
        <div v-else class="empty-state">策略模拟每小时自动生成，暂无数据不影响策略运行</div>
      </div>

      <!-- 板块二：三桶分析 -->
      <div class="summary-section">
        <div class="section-title">三桶分析（按开仓时 5m 爆拉分档）</div>
        <div class="rule-note">
          可开仓机会 = 策略规则下能开的动作数（含追单、含全局 10 仓上限）；实际开仓 = 客户端库当天真实开仓；少做 = 可开 − 实际。
          仓位倍数：爆拉桶（5m 单根 ≥2.5%）×1.5 / 中间桶 ×1.0 / 温和桶 ×0.7
        </div>
        <el-table v-if="buckets.length" :data="buckets" size="small" stripe>
          <el-table-column prop="bucket" label="桶" min-width="90" />
          <el-table-column prop="opportunity" label="可开仓机会" min-width="100" align="right" />
          <el-table-column prop="actual" label="实际开仓" min-width="90" align="right" />
          <el-table-column label="实际盈亏(U)" min-width="100" align="right">
            <template #default="{ row }">
              <span :class="chgClass(row.actualPnl)">{{ fmtNum(row.actualPnl, true) }}</span>
            </template>
          </el-table-column>
          <el-table-column prop="missed" label="少做" min-width="70" align="right" />
          <el-table-column label="转化率" min-width="90" align="right">
            <template #default="{ row }">
              <span>{{ Number(row.conversion).toFixed(1) }}%</span>
            </template>
          </el-table-column>
        </el-table>
        <div v-else class="empty-state">三桶分析每小时自动生成，暂无数据不影响策略运行</div>
      </div>

      <div class="rule-note footnote">
        口径：按 5m 收盘/高低价近似回放，未含手续费与滑点，用于复盘当日策略环境，非实盘对账。
      </div>
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
  padding: 20px;
}
.card-header {
  display: flex;
  align-items: center;
  justify-content: space-between;
  margin-bottom: 16px;
}
.card-title {
  margin: 0;
  color: var(--quant-text, #e0e0e0);
  font-size: 18px;
}
.summary-hint {
  font-size: 12px;
  color: var(--quant-text-secondary, #9ca3af);
}
.summary-section {
  margin-bottom: 20px;
}
.section-title {
  font-size: 14px;
  font-weight: 600;
  color: var(--quant-text, #e0e0e0);
  margin-bottom: 8px;
}
.rule-note {
  font-size: 12px;
  color: var(--quant-text-secondary, #9ca3af);
  margin-bottom: 8px;
  line-height: 1.6;
}
.footnote {
  margin-top: 8px;
  border-top: 1px dashed rgba(255, 255, 255, 0.1);
  padding-top: 8px;
}
.empty-state {
  padding: 24px;
  text-align: center;
  color: var(--quant-text-secondary, #9ca3af);
  font-size: 13px;
  border: 1px dashed rgba(255, 255, 255, 0.1);
  border-radius: 8px;
}
</style>
