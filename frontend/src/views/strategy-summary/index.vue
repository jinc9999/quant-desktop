<script setup lang="ts">
/**
 * 每日策略总结（自动生成）——大白话 + 颜色分区
 * 蓝=市场给了什么；绿/红=我们做得怎么样；琥珀=漏掉的肉；紫=规则挡下的风险单。
 * 每块都配一句「该怎么改」。数据每小时自动更新。
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
const details = computed<any[]>(() => latest.value?.meta.details || latest.value?.meta.gap || []);
const interceptHealth = computed<any>(() => latest.value?.meta.interceptHealth || null);
const oppValue = computed<any>(() => latest.value?.meta.opportunityValue || null);

/* ========== 大白话标签与改进建议 ========== */
const lossTips: Record<string, { label: string; tip: string }> = {
  激活错配: { label: "首仓开了，但追单信号没接住", tip: "已加 K 线高点补判，观察是否减少" },
  执行失败: { label: "下单被交易所拒绝了", tip: "多为瞬时问题，已加自动重试；连续出现要看错误码" },
  余额不足: { label: "账户余额不够开仓", tip: "给账户补充保证金" },
  数据缺口: { label: "行情数据没拉全，错过判定", tip: "网络波动导致，已记录；频繁出现要检查网络" },
  信号未触发: { label: "机会来了，但没及时接住", tip: "已加 5m 收盘通道，观察是否减少" },
  客户端未运行: { label: "策略没在运行的时段", tip: "保持客户端和策略全天在线" },
  未归因: { label: "旧版本没记录，暂时判断不了", tip: "新版本跑几天后自动消失" },
  拦截: { label: "被风控规则挡住了（该挡）", tip: "看下方紫色板块：挡对率越高越合理" },
};
const reasonLabel = (r: string) =>
  ({
    maxpos: "全局10仓上限", cooldown: "冷却期内", no_active: "持仓未激活(无法追单)",
    addon_limit: "追单达上限", new_listing: "新币过滤", volume: "成交额不足",
    rank: "24h涨幅排名未通过", pullback: "山顶过滤器", balance: "余额不足", slots: "槽位已满"
  }[r] || r);

/* ========== 市场 vs 实际（机会价值） ========== */
const ovRows = computed<any[]>(() => {
  const b = oppValue.value?.buckets;
  if (!b) return [];
  return ["爆拉桶", "中间桶", "温和桶", "合计"]
    .filter((k) => b[k])
    .map((k) => ({ bucket: k, ...b[k] }));
});
const ovTotal = computed<any>(() => oppValue.value?.buckets?.["合计"] || null);
const marketValue = computed(() => ovTotal.value?.profitVal || 0); // 市场赚钱机会最多能赚
const marketCount = computed(() => ovTotal.value?.profitCnt || 0); // 赚钱机会数
const actualVal = computed(() => ovTotal.value?.actVal || 0); // 实际落袋（净）
const actualProfit = computed(() => ovTotal.value?.actProfit || 0); // 实际赚到的正收益
const eatRate = computed(() => ovTotal.value?.profitCap || 0); // 吃肉率
const marketTotal = computed(() => ovTotal.value?.virtualVal || 0); // 理论池整体
const actCnt = computed(() => ovTotal.value?.actCnt || 0);

const lossRows = computed<any[]>(() => {
  const l = oppValue.value?.loss;
  if (!l) return [];
  return Object.entries(l)
    .map(([k, v]: any) => ({ cls: k, label: lossTips[k]?.label || k, tip: lossTips[k]?.tip || "", ...v }))
    .sort((a, b) => Math.abs(b.miss || 0) - Math.abs(a.miss || 0));
});

/* ========== 规则拦截（该挡验证） ========== */
const interceptRows = computed<any[]>(() => {
  const m = interceptHealth;
  if (!m) return [];
  return Object.entries(m)
    .map(([k, v]: any) => ({ reason: reasonLabel(k), ...v }))
    .sort((a, b) => b.count - a.count);
});
const interceptTotal = computed<any>(() => {
  const rs = interceptRows.value;
  if (!rs.length) return null;
  let count = 0, wClosed = 0, wLoss = 0, wWin = 0, pnl = 0;
  rs.forEach((r) => {
    count += r.count;
    pnl += r.pnl || 0;
    if (!r.closed) return;
    const blockN = Math.round(((r.blockCorrect || 0) / 100) * r.closed);
    wClosed += r.closed;
    wLoss += blockN;
    wWin += r.closed - blockN;
  });
  return {
    count, closed: wClosed, pnl,
    blockRate: wClosed ? (wLoss / wClosed) * 100 : 0,
    missRate: wClosed ? (wWin / wClosed) * 100 : 0
  };
});

/* ========== 一句话总结 ========== */
const headline = computed(() => {
  const t = ovTotal.value;
  if (!t) return { main: "今天还没有总结数据，稍后自动生成", sub: "每小时自动更新，数据积累后这里会给你一句话结论", tone: "neutral" };
  const meat = marketValue.value;
  const eat = Number(eatRate.value).toFixed(1);
  const top = lossRows.value[0];
  let main = "";
  let tone = "neutral";
  if (meat > 0) {
    main = `市场今天给了 ${marketCount.value} 个赚钱机会（最多能赚 +${fmtNum(meat)}U），我们实际吃到 ${eat}%。`;
    tone = eatRate.value >= 60 ? "good" : eatRate.value >= 30 ? "warn" : "bad";
  } else {
    main = `今天市场偏坑（机会整体会亏 ${fmtNum(Math.abs(meat), true)}U），我们靠规则躲掉不少坑，实际 ${fmtNum(actualVal.value, true)}U。`;
    tone = actualVal.value >= 0 ? "good" : "warn";
  }
  let sub = `实际开仓 ${actCnt.value} 次，落袋 ${fmtNum(actualVal.value, true)}U`;
  if (top) sub += `。最该改进：${top.label}——${top.tip}`;
  return { main, sub, tone };
});

/* ========== 进阶数据（原表格保留） ========== */
const detailGroups = computed<any[]>(() => {
  const groups = new Map<string, any>();
  const push = (item: any) => {
    const g = groups.get(item.symbol) || { symbol: item.symbol, lossCount: 0, rejectCount: 0, children: [] };
    g.children.push(item);
    groups.set(item.symbol, g);
  };
  (details.value || []).forEach((i: any) => {
    if (i.cls === "成交") return;
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
        <h2 class="card-title">每日策略总结</h2>
        <span class="summary-hint">每小时自动更新 · {{ updatedAt || "等待数据" }}</span>
      </div>

      <!-- 一句话总结 -->
      <div class="mk-summary" :class="`tone-${headline.tone}`">
        <div class="mk-main">{{ headline.main }}</div>
        <div class="mk-sub">{{ headline.sub }}</div>
      </div>

      <!-- 三大数字卡：市场 / 实际 / 吃肉率 -->
      <div class="stat-row">
        <div class="stat-card mk-blue">
          <div class="stat-label">市场给的肉（蓝）</div>
          <div class="stat-value text-blue">+{{ fmtNum(marketValue) }}U</div>
          <div class="stat-sub">{{ marketCount }} 个赚钱机会 · 理论池整体 {{ fmtNum(marketTotal, true) }}U</div>
        </div>
        <div class="stat-card" :class="actualVal >= 0 ? 'mk-green' : 'mk-red'">
          <div class="stat-label">我们实际赚（{{ actualVal >= 0 ? "绿" : "红" }}）</div>
          <div class="stat-value" :class="chgClass(actualVal)">{{ fmtNum(actualVal, true) }}U</div>
          <div class="stat-sub">实际开仓 {{ actCnt }} 次 · 其中正收益 +{{ fmtNum(actualProfit) }}U</div>
        </div>
        <div class="stat-card mk-violet">
          <div class="stat-label">吃肉率（紫）</div>
          <div class="stat-value text-violet">{{ Number(eatRate).toFixed(1) }}%</div>
          <el-progress :percentage="Math.min(100, Math.max(0, Number(eatRate)))" :stroke-width="10" :text-inside="true" />
          <div class="stat-sub">市场给的赚钱机会，我们吃到的比例</div>
        </div>
      </div>

      <!-- 板块A：市场给了什么（蓝） -->
      <div class="mk-section mk-blue">
        <div class="section-title">市场给了什么（蓝）</div>
        <div class="rule-note">
          用同一套规则回放今天所有可开机会：哪些机会本来能赚钱、最多能赚多少。
          这是「理论天花板」，不代表一定能赚到（还有滑点和手续费）。
        </div>
        <el-table v-if="ovRows.length" :data="ovRows" size="small" stripe>
          <el-table-column prop="bucket" label="类型" min-width="80" />
          <el-table-column prop="opp" label="机会数" width="80" align="right" />
          <el-table-column prop="profitCnt" label="赚钱机会" width="90" align="right" />
          <el-table-column label="最多能赚(U)" width="110" align="right">
            <template #default="{ row }">
              <span :class="chgClass(row.profitVal)">{{ fmtNum(row.profitVal, true) }}</span>
            </template>
          </el-table-column>
          <el-table-column prop="hitProfit" label="盘中到过赚钱线" width="120" align="right" />
          <el-table-column label="整体回放(U)" width="110" align="right">
            <template #default="{ row }">
              <span :class="chgClass(row.virtualVal)">{{ fmtNum(row.virtualVal, true) }}</span>
            </template>
          </el-table-column>
        </el-table>
        <div v-else class="empty-state">市场数据每小时自动生成</div>
      </div>

      <!-- 板块B：我们做得怎么样（绿/红） -->
      <div class="mk-section" :class="actualVal >= 0 ? 'mk-green' : 'mk-red'">
        <div class="section-title">我们做得怎么样（{{ actualVal >= 0 ? "绿" : "红" }}）</div>
        <div class="rule-note">
          实际开仓、实际落袋的盈亏。对照蓝色板块的「最多能赚」，就能看出市场给的肉吃到几成。
        </div>
        <el-table v-if="ovRows.length" :data="ovRows" size="small" stripe>
          <el-table-column prop="bucket" label="类型" min-width="80" />
          <el-table-column prop="actCnt" label="实际开仓" width="90" align="right" />
          <el-table-column prop="actClosed" label="已平仓" width="80" align="right" />
          <el-table-column label="实际盈亏(U)" width="110" align="right">
            <template #default="{ row }">
              <span :class="chgClass(row.actVal)">{{ fmtNum(row.actVal, true) }}</span>
            </template>
          </el-table-column>
          <el-table-column label="吃了市场几成肉" width="130" align="right">
            <template #default="{ row }">
              {{ row.profitVal > 0 ? Number(row.profitCap).toFixed(1) + "%" : "--" }}
            </template>
          </el-table-column>
        </el-table>
        <div v-else class="empty-state">实际数据每小时自动生成</div>
      </div>

      <!-- 板块C：漏掉的肉（琥珀） -->
      <div class="mk-section mk-amber">
        <div class="section-title">漏掉的肉（琥珀）——市场给了但没吃到，按原因拆开看</div>
        <div class="rule-note">漏掉盈利 = 本来能赚的钱没赚到；躲过亏损 = 本来会亏的钱没亏（这类是好事）。</div>
        <el-table v-if="lossRows.length" :data="lossRows" size="small" stripe>
          <el-table-column label="原因" min-width="180">
            <template #default="{ row }">{{ row.label }}</template>
          </el-table-column>
          <el-table-column prop="cnt" label="次数" width="70" align="right" />
          <el-table-column label="漏掉盈利(U)" width="110" align="right">
            <template #default="{ row }">
              <span class="text-red">{{ fmtNum(row.miss, true) }}</span>
            </template>
          </el-table-column>
          <el-table-column label="躲过亏损(U)" width="110" align="right">
            <template #default="{ row }">
              <span class="text-green">{{ fmtNum(row.dodge, true) }}</span>
            </template>
          </el-table-column>
          <el-table-column label="怎么改" min-width="220">
            <template #default="{ row }">{{ row.tip }}</template>
          </el-table-column>
        </el-table>
        <div v-else class="empty-state">漏肉分析每小时自动生成</div>
      </div>

      <!-- 板块D：规则挡下的风险单（紫） -->
      <div class="mk-section mk-violet">
        <div class="section-title">规则挡下的风险单（紫）——挡对了还是挡错了</div>
        <div class="rule-note" v-if="interceptTotal">
          共挡 {{ interceptTotal.count }} 次：挡对率 {{ Number(interceptTotal.blockRate).toFixed(1) }}%（躲过亏损，好事），
          误杀率 {{ Number(interceptTotal.missRate).toFixed(1) }}%（错过赚钱，观察是否该松）。
          这些挡单若硬开，合计会 {{ interceptTotal.pnl >= 0 ? "多赚" : "多亏" }} {{ fmtNum(Math.abs(interceptTotal.pnl), true) }}U。
        </div>
        <el-table v-if="interceptRows.length" :data="interceptRows" size="small" stripe>
          <el-table-column prop="reason" label="规则" min-width="150" />
          <el-table-column prop="count" label="次数" width="70" align="right" />
          <el-table-column prop="closed" label="已走完" width="80" align="right" />
          <el-table-column label="虚拟盈亏(U)" width="110" align="right">
            <template #default="{ row }">
              <span :class="chgClass(row.pnl)">{{ fmtNum(row.pnl, true) }}</span>
            </template>
          </el-table-column>
          <el-table-column label="挡对率" width="90" align="right">
            <template #default="{ row }">{{ Number(row.blockCorrect).toFixed(1) }}%</template>
          </el-table-column>
          <el-table-column label="误杀率" width="90" align="right">
            <template #default="{ row }">{{ Number(row.missProfit).toFixed(1) }}%</template>
          </el-table-column>
        </el-table>
        <div v-else class="empty-state">拦截分析每小时自动生成</div>
      </div>

      <!-- 进阶数据（折叠） -->
      <el-collapse class="mk-adv">
        <el-collapse-item title="进阶数据（研究人员看：完整漏斗/模拟回放/逐单明细）">
          <div class="summary-section">
            <div class="section-title">策略口径当日模拟</div>
            <el-table v-if="simRows.length" :data="simRows" size="small" stripe v-loading="loading">
              <el-table-column prop="k" label="指标" min-width="150" />
              <el-table-column prop="v" label="数值" min-width="160" align="right" />
              <el-table-column prop="d" label="说明" min-width="280" />
            </el-table>
          </div>
          <div class="summary-section">
            <div class="section-title">每日可开仓漏斗（首仓 + 追单）</div>
            <el-table v-if="buckets.length" :data="buckets" size="small" stripe>
              <el-table-column prop="bucket" label="桶" min-width="70" />
              <el-table-column label="首仓机会" width="86" align="right"><template #default="{ row }">{{ row.first }}</template></el-table-column>
              <el-table-column label="实际首仓" width="86" align="right"><template #default="{ row }">{{ row.actualFirst }}</template></el-table-column>
              <el-table-column label="追单机会" width="86" align="right"><template #default="{ row }">{{ row.addon }}</template></el-table-column>
              <el-table-column label="实际追单" width="86" align="right"><template #default="{ row }">{{ row.actualAddon }}</template></el-table-column>
              <el-table-column label="拦截(该挡)" width="90" align="right"><template #default="{ row }">{{ row.rule ?? 0 }}</template></el-table-column>
              <el-table-column label="执行损耗" width="90" align="right"><template #default="{ row }">{{ row.loss ?? 0 }}</template></el-table-column>
              <el-table-column label="转化率" width="90" align="right"><template #default="{ row }">{{ Number(row.conversion).toFixed(1) }}%</template></el-table-column>
            </el-table>
          </div>
          <div class="summary-section">
            <div class="section-title">"有机会但没做成"的单（{{ detailGroups.length }} 个币 · 点击展开）</div>
            <el-table v-if="detailGroups.length" :data="detailGroups" size="small" stripe>
              <el-table-column type="expand">
                <template #default="{ row }">
                  <el-table :data="row.children" size="small">
                    <el-table-column prop="time" label="时间" width="80" />
                    <el-table-column prop="bucket" label="桶" width="90" />
                    <el-table-column label="类型" width="100"><template #default="{ row: c }">{{ c.kindLabel }}</template></el-table-column>
                    <el-table-column label="分类" width="90"><template #default="{ row: c }">{{ c.kind }}</template></el-table-column>
                    <el-table-column label="说明" min-width="200"><template #default="{ row: c }">{{ c.why || reasonLabel(c.reason) }}</template></el-table-column>
                  </el-table>
                </template>
              </el-table-column>
              <el-table-column prop="symbol" label="币种" min-width="110" />
              <el-table-column prop="total" label="未做成单数" width="100" align="right" />
              <el-table-column prop="lossCount" label="执行损耗" width="90" align="right" />
              <el-table-column prop="rejectCount" label="拦截" width="80" align="right" />
            </el-table>
          </div>
        </el-collapse-item>
      </el-collapse>

      <div class="rule-note footnote">
        口径：回放按 5m 收盘/高低价近似，未含手续费与滑点；机会数是每次拉取行情时的快照。
        市场与实际的差距 = 理论天花板 vs 真实执行，趋势比单日数字更重要。
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

/* 一句话总结 */
.mk-summary {
  border-radius: 10px;
  padding: 14px 16px;
  margin-bottom: 16px;
  border: 1px solid var(--quant-border, #2c2f3a);
}
.mk-summary.tone-good { background: rgba(34, 197, 94, 0.1); border-color: #22c55e; }
.mk-summary.tone-warn { background: rgba(245, 158, 11, 0.1); border-color: #f59e0b; }
.mk-summary.tone-bad { background: rgba(239, 68, 68, 0.1); border-color: #ef4444; }
.mk-summary.tone-neutral { background: rgba(148, 163, 184, 0.08); }
.mk-main { font-size: 15px; font-weight: 600; color: var(--quant-text, #e0e0e0); line-height: 1.6; }
.mk-sub { font-size: 12px; color: var(--quant-text-secondary, #9ca3af); margin-top: 6px; line-height: 1.6; }

/* 数字卡 */
.stat-row {
  display: grid;
  grid-template-columns: repeat(3, 1fr);
  gap: 12px;
  margin-bottom: 16px;
}
@media (max-width: 900px) {
  .stat-row { grid-template-columns: 1fr; }
}
.stat-card {
  border-radius: 10px;
  padding: 14px 16px;
  border: 1px solid var(--quant-border, #2c2f3a);
  background: var(--quant-card, #1d1f27);
}
.stat-label { font-size: 12px; color: var(--quant-text-secondary, #9ca3af); }
.stat-value { font-size: 24px; font-weight: 700; margin: 6px 0; }
.stat-sub { font-size: 11px; color: var(--quant-text-secondary, #9ca3af); line-height: 1.5; }
.text-blue { color: #60a5fa; }
.text-violet { color: #a78bfa; }

/* 板块配色 */
.mk-section {
  border-radius: 10px;
  padding: 14px 16px;
  margin-bottom: 16px;
  border: 1px solid var(--quant-border, #2c2f3a);
  background: var(--quant-card, #1d1f27);
}
.mk-blue { border-left: 4px solid #3b82f6; background: rgba(59, 130, 246, 0.05); }
.mk-green { border-left: 4px solid #22c55e; background: rgba(34, 197, 94, 0.05); }
.mk-red { border-left: 4px solid #ef4444; background: rgba(239, 68, 68, 0.05); }
.mk-amber { border-left: 4px solid #f59e0b; background: rgba(245, 158, 11, 0.05); }
.mk-violet { border-left: 4px solid #8b5cf6; background: rgba(139, 92, 246, 0.05); }
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
.sub-title {
  margin: 12px 0 8px;
  font-size: 13px;
  font-weight: 600;
  color: var(--quant-text-secondary, #9ca3af);
}
.empty-state {
  padding: 24px;
  text-align: center;
  color: var(--quant-text-secondary, #9ca3af);
  font-size: 13px;
  border: 1px dashed rgba(255, 255, 255, 0.1);
  border-radius: 8px;
}
.mk-adv {
  margin-top: 4px;
}
.summary-section {
  margin-bottom: 16px;
}
</style>
