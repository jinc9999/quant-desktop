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
const rejects = computed<any[]>(() => latest.value?.meta.rejects || []);
/** 逐单归因明细（成交/拦截=该挡/激活错配/执行失败/余额不足/数据缺口/信号未触发/未运行/未归因） */
const details = computed<any[]>(() => latest.value?.meta.details || latest.value?.meta.gap || []);
/** 按币种分组折叠：每币一行（未做成单数/执行损耗/拦截），点击展开明细 */
const detailGroups = computed<any[]>(() => {
  const groups = new Map<string, any>();
  const push = (item: any) => {
    const g = groups.get(item.symbol) || { symbol: item.symbol, lossCount: 0, rejectCount: 0, children: [] };
    g.children.push(item);
    groups.set(item.symbol, g);
  };
  (details.value || []).forEach((i: any) => {
    if (i.cls === "成交") return; // 成交不进"没做成"列表
    const kind = i.cls === "拦截" ? "拦截" : "执行损耗";
    push({ ...i, kind, kindLabel: i.addOn ? "追单" : "首仓" });
  });
  (rejects.value || []).forEach((i: any) => {
    push({ ...i, kind: "拦截", kindLabel: reasonLabel(i.reason) });
  });
  return [...groups.values()]
    .map((g) => {
      g.lossCount = g.children.filter((c: any) => c.kind === "执行损耗").length;
      g.rejectCount = g.children.filter((c: any) => c.kind === "拦截").length;
      g.total = g.children.length;
      return g;
    })
    .sort((a, b) => b.total - a.total);
});
const reasonLabel = (r: string) =>
  ({
    maxpos: "全局10仓上限", cooldown: "冷却期内", no_active: "持仓未激活(无法追单)",
    addon_limit: "追单达上限", new_listing: "新币过滤", volume: "成交额不足",
    rank: "24h涨幅排名未通过", pullback: "山顶过滤器", balance: "余额不足", slots: "槽位已满"
  }[r] || r);

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

      <!-- 板块二：每日可开仓漏斗 -->
      <div class="summary-section">
        <div class="section-title">每日可开仓漏斗（首仓 + 追单，一一对应）</div>
        <div class="rule-note">
          机会 = 15m≥3% 且可开（追单=已激活且未达 3 仓）；成交 = 客户端真实开仓（同币 ±45 分钟匹配）；
          拦截 = 策略规则内未开（该挡）；损耗 = 执行层真实原因（激活错配/失败/余额/数据缺口/采样差）。
        </div>
        <el-table v-if="buckets.length" :data="buckets" size="small" stripe>
          <el-table-column prop="bucket" label="桶" min-width="70" />
          <el-table-column label="首仓机会" width="86" align="right">
            <template #default="{ row }">{{ row.first }}</template>
          </el-table-column>
          <el-table-column label="实际首仓" width="86" align="right">
            <template #default="{ row }">{{ row.actualFirst }}</template>
          </el-table-column>
          <el-table-column label="首仓少做" width="86" align="right">
            <template #default="{ row }">{{ row.first - row.actualFirst }}</template>
          </el-table-column>
          <el-table-column label="追单机会" width="86" align="right">
            <template #default="{ row }">{{ row.addon }}</template>
          </el-table-column>
          <el-table-column label="实际追单" width="86" align="right">
            <template #default="{ row }">{{ row.actualAddon }}</template>
          </el-table-column>
          <el-table-column label="追单少做" width="86" align="right">
            <template #default="{ row }">{{ row.addon - row.actualAddon }}</template>
          </el-table-column>
          <el-table-column label="合计可开" width="86" align="right">
            <template #default="{ row }">{{ row.first + row.addon }}</template>
          </el-table-column>
          <el-table-column label="实际合计" width="86" align="right">
            <template #default="{ row }">{{ row.actualFirst + row.actualAddon }}</template>
          </el-table-column>
          <el-table-column label="合计少做" width="86" align="right">
            <template #default="{ row }">{{ row.first - row.actualFirst + row.addon - row.actualAddon }}</template>
          </el-table-column>
          <el-table-column label="拦截(该挡)" width="90" align="right">
            <template #default="{ row }">{{ row.rule ?? 0 }}</template>
          </el-table-column>
          <el-table-column label="执行损耗" width="90" align="right">
            <template #default="{ row }">{{ row.loss ?? 0 }}</template>
          </el-table-column>
        </el-table>
        <div v-else class="empty-state">漏斗统计每小时自动生成</div>
      </div>

      <!-- 板块三：三桶分析 -->
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

      <!-- 板块四：'有机会但没做成'的单 -->
      <div class="summary-section">
        <div class="section-title">"有机会但没做成"的单（{{ detailGroups.length }} 个币 · 点击展开）</div>
        <div class="rule-note">
          拦截 = 策略规则内未开（该挡）；执行损耗 = 模拟规则可开但实际未成交，已按真实原因细分：
          激活错配 / 交易所失败（错误码）/ 余额不足 / 数据缺口 / 信号未触发（tick 采样差）/ 客户端未运行。
          完整明细每小时自动更新。
        </div>
        <el-table v-if="detailGroups.length" :data="detailGroups" size="small" stripe>
          <el-table-column type="expand">
            <template #default="{ row }">
              <el-table :data="row.children" size="small">
                <el-table-column prop="time" label="时间" width="80" />
                <el-table-column prop="bucket" label="桶" width="90" />
                <el-table-column label="类型" width="100">
                  <template #default="{ row: c }">{{ c.kindLabel }}</template>
                </el-table-column>
                <el-table-column label="分类" width="90">
                  <template #default="{ row: c }">{{ c.kind }}</template>
                </el-table-column>
                <el-table-column label="说明" min-width="200">
                  <template #default="{ row: c }">
                    {{ c.why || reasonLabel(c.reason) }}
                  </template>
                </el-table-column>
              </el-table>
            </template>
          </el-table-column>
          <el-table-column prop="symbol" label="币种" min-width="110" />
          <el-table-column prop="total" label="未做成单数" width="100" align="right" />
          <el-table-column prop="lossCount" label="执行损耗" width="90" align="right" />
          <el-table-column prop="rejectCount" label="拦截" width="80" align="right" />
        </el-table>
        <div v-if="!detailGroups.length" class="empty-state">逐单明细每小时自动生成</div>
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
.sub-title {
  margin: 12px 0 8px;
  font-size: 13px;
  font-weight: 600;
  color: var(--quant-text-secondary, #9ca3af);
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
