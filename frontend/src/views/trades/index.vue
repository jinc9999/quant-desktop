<script setup lang="ts">
/**
 * 已完成交易明细面板
 * 展示所有已平仓持仓记录，通过 Wails 绑定调用 Go 后端 QuantService.GetClosedTradeDetails。
 * 包含 10 列：交易对、方向、入场价、出场价、数量、净盈亏、盈利%、手续费、平仓时间、平仓原因。
 * 支持列排序、条件筛选、分页、固定表头与奇偶行斑马纹。
 */
import { ref, computed, onMounted, onUnmounted } from "vue";
import { callService } from "../../utils/service";
import { QuantService } from "../../../bindings/quant-desktop/internal/bindings";

defineOptions({ name: "Trades" });

/** 已完成交易行数据（与 Go 后端 ClosedTradeDetail 视图模型字段一致） */
interface ClosedTradeRow {
  id: number;
  symbol: string;
  side: string;
  entryPrice: number;
  exitPrice: number | null;
  amount: number;
  leverage: number;
  realizedPnl: number;
  profitPct: number;
  fee: number;
  closedAt: number | null;
  closeReason: string | null;
}

// 已完成交易数据（shallowRef：整体替换数组，避免大列表深度响应开销）
const trades = ref<ClosedTradeRow[]>([]);
const loading = ref(false);
// 自动刷新定时器
let refreshTimer: ReturnType<typeof setInterval> | null = null;

// 筛选条件
const filters = ref<{ symbol: string; side: string; closeReason: string }>({
  symbol: "",
  side: "",
  closeReason: "",
});

// 排序状态（el-table sort-change 事件驱动）
const sortState = ref<{ prop: string; order: string }>({ prop: "closedAt", order: "descending" });

// 分页
const currentPage = ref(1);
const pageSize = ref(20);

/** 交易对选项（从数据中去重提取） */
const symbolOptions = computed(() => {
  const set = new Set<string>();
  trades.value.forEach(t => set.add(t.symbol));
  return Array.from(set).sort();
});

/** 筛选后的行 */
const filteredRows = computed(() => {
  let rows = trades.value;
  if (filters.value.symbol) rows = rows.filter(r => r.symbol === filters.value.symbol);
  if (filters.value.side) rows = rows.filter(r => r.side === filters.value.side);
  if (filters.value.closeReason) rows = rows.filter(r => r.closeReason === filters.value.closeReason);
  return rows;
});

/** 各列排序比较器（按字段类型定制） */
const comparators: Record<string, (a: ClosedTradeRow, b: ClosedTradeRow) => number> = {
  symbol: (a, b) => a.symbol.localeCompare(b.symbol),
  entryPrice: (a, b) => a.entryPrice - b.entryPrice,
  exitPrice: (a, b) => (a.exitPrice ?? 0) - (b.exitPrice ?? 0),
  amount: (a, b) => a.amount - b.amount,
  realizedPnl: (a, b) => a.realizedPnl - b.realizedPnl,
  profitPct: (a, b) => a.profitPct - b.profitPct,
  fee: (a, b) => a.fee - b.fee,
  closedAt: (a, b) => (a.closedAt ?? 0) - (b.closedAt ?? 0),
};

/** 排序后的行 */
const sortedRows = computed(() => {
  const { prop, order } = sortState.value;
  const cmp = comparators[prop];
  if (!cmp || !order) return filteredRows.value;
  const rows = [...filteredRows.value];
  rows.sort((a, b) => (order === "ascending" ? cmp(a, b) : cmp(b, a)));
  return rows;
});

/** 当前页数据 */
const pagedRows = computed(() => {
  const start = (currentPage.value - 1) * pageSize.value;
  return sortedRows.value.slice(start, start + pageSize.value);
});

// ===== 摘要统计 =====
// 全量统计来自后端 GetTradeStats（数据库全部已平仓记录聚合），
// 不再用最近 200 条列表在端上计算，避免"总"数字随列表截断失真。
interface TradeStats {
  count: number;
  netPnl: number;
  wins: number;
  losses: number;
  zeros: number;
  totalFee: number;
  winRate: number;
}

const tradeStats = ref<TradeStats>({ count: 0, netPnl: 0, wins: 0, losses: 0, zeros: 0, totalFee: 0, winRate: 0 });

/** 总平仓笔数（全量） */
const totalCount = computed(() => tradeStats.value.count);
/** 总净盈亏（全量） */
const totalPnl = computed(() => tradeStats.value.netPnl);
/** 总手续费（全量） */
const totalFee = computed(() => tradeStats.value.totalFee);
/** 盈利笔数（全量） */
const profitCount = computed(() => tradeStats.value.wins);
/** 亏损笔数（全量） */
const lossCount = computed(() => tradeStats.value.losses);
/** 胜率：盈利 /（盈利 + 亏损），零盈亏（多为对账幽灵单）不计入分母 */
const winRate = computed(() => tradeStats.value.winRate);

// ===== 格式化工具 =====
/**
 * 将毫秒时间戳格式化为 YYYY-MM-DD HH:mm:ss
 * @param ts 毫秒时间戳，为空或无效时返回 "-"
 * @returns 格式化后的时间字符串
 */
function formatDateTime(ts: number | null | undefined): string {
  if (!ts) return "-";
  const d = new Date(ts);
  const pad = (n: number) => String(n).padStart(2, "0");
  return (
    `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ` +
    `${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`
  );
}

/**
 * 价格格式化：按量级保留不同小数位（≥1000 千分位2位，≥1 保留4位，其余6位）
 * @param v 价格数值，为空或非正时返回 "-"
 * @returns 格式化后的价格字符串
 */
function formatPrice(v: number | null | undefined): string {
  if (v == null || v <= 0) return "-";
  if (v >= 1000) {
    return v.toLocaleString("en-US", { minimumFractionDigits: 2, maximumFractionDigits: 2 });
  }
  if (v >= 1) return v.toFixed(4);
  return v.toFixed(6);
}

/**
 * 数量格式化：千分位 + 最多 4 位小数
 * @param v 数量数值，为空时返回 "-"
 * @returns 格式化后的数量字符串
 */
function formatAmount(v: number | null | undefined): string {
  if (v == null) return "-";
  return v.toLocaleString("en-US", { maximumFractionDigits: 4 });
}

/**
 * USDT 金额格式化：千分位 + 固定 2 位小数
 * @param v 金额数值，为空时返回 "-"
 * @returns 格式化后的金额字符串
 */
function formatUsd(v: number | null | undefined): string {
  if (v == null) return "-";
  return v.toLocaleString("en-US", { minimumFractionDigits: 2, maximumFractionDigits: 2 });
}

/**
 * 带正负号的盈亏格式化（USDT，2 位小数）
 * @param v 盈亏数值
 * @returns 带符号的金额字符串
 */
function formatSignedUsd(v: number | null | undefined): string {
  if (v == null) return "-";
  const prefix = v > 0 ? "+" : "";
  return prefix + formatUsd(v);
}

/**
 * 带正负号的百分比格式化（2 位小数）
 * @param v 百分比数值
 * @returns 带符号的百分比字符串
 */
function formatPct(v: number | null | undefined): string {
  if (v == null) return "-";
  const prefix = v > 0 ? "+" : "";
  return prefix + v.toFixed(2) + "%";
}

/**
 * 拆分交易对为基础币与计价币（如 BTCUSDT → {base:BTC, quote:USDT}）
 * @param symbol 交易对字符串
 * @returns 基础币与计价币对象
 */
function splitSymbol(symbol: string): { base: string; quote: string } {
  const quotes = ["USDT", "USDC", "FDUSD", "BUSD", "BTC", "ETH", "BNB"];
  for (const q of quotes) {
    if (symbol.endsWith(q) && symbol.length > q.length) {
      return { base: symbol.slice(0, -q.length), quote: q };
    }
  }
  return { base: symbol, quote: "" };
}

/**
 * 方向中文标签
 * @param side 方向枚举（LONG / SHORT）
 * @returns 中文标签
 */
function sideLabel(side: string): string {
  return side === "LONG" ? "多" : side === "SHORT" ? "空" : side;
}

/**
 * 方向颜色（多绿空红）
 * @param side 方向枚举
 * @returns CSS 颜色值
 */
function sideColor(side: string): string {
  return side === "LONG" ? "#22c55e" : side === "SHORT" ? "#ef4444" : "var(--quant-text, #e0e0e0)";
}

/**
 * 盈亏/盈利%颜色（正绿负红）
 * @param v 数值
 * @returns CSS 颜色值
 */
function pnlColor(v: number): string {
  return v > 0 ? "#22c55e" : v < 0 ? "#ef4444" : "var(--quant-text, #e0e0e0)";
}

/**
 * 平仓原因中文标签
 * @param reason 平仓原因枚举值
 * @returns 中文标签
 */
function closeReasonLabel(reason: string | null): string {
  switch (reason) {
    case "STOP_LOSS":
      return "止损";
    case "TRAILING_STOP":
      return "跟踪止损";
    case "TAKE_PROFIT":
      return "固定止盈";
    case "LIMIT_STOP":
      return "限价平仓";
    case "MAX_HOLD":
      return "超时平仓";
    case "ROLLBACK":
      return "回滚";
    case "GHOST":
      return "状态同步";
    default:
      return reason || "-";
  }
}

/**
 * 平仓原因颜色
 * @param reason 平仓原因枚举值
 * @returns CSS 颜色值
 */
function closeReasonColor(reason: string | null): string {
  switch (reason) {
    case "STOP_LOSS":
      return "#ef4444";
    case "TRAILING_STOP":
      return "#f59e0b";
    case "ROLLBACK":
      return "#8b8fa3";
    default:
      return "var(--quant-text, #e0e0e0)";
  }
}

// ===== 事件处理 =====
/**
 * 表头排序变更回调
 * @param param el-table sort-change 事件参数（prop 列字段，order 排序方向）
 */
function handleSortChange({ prop, order }: { prop: string; order: string }) {
  sortState.value = { prop: prop || "", order: order || "" };
}

/** 筛选条件变化时重置到第一页 */
function resetPage() {
  currentPage.value = 1;
}

/**
 * 刷新已完成交易数据：调用 Go 绑定获取明细列表
 * @param showErrorFlag 失败时是否弹出错误提示（轮询调用传 false 避免刷屏）
 */
async function refreshTrades(showErrorFlag = false) {
  loading.value = true;
  const list = await callService(
    () => QuantService.GetClosedTradeDetails(200),
    { silent: !showErrorFlag, context: "获取已完成交易" }
  );
  if (list !== null) {
    trades.value = (list || []) as ClosedTradeRow[];
  }
  // 顶部卡片用全量统计（与列表分开获取，避免 200 条窗口导致"总"数字失真）
  const stats = await callService(() => QuantService.GetTradeStats(), { silent: true });
  if (stats) {
    tradeStats.value = {
      count: stats.count ?? 0,
      netPnl: stats.netPnl ?? 0,
      wins: stats.wins ?? 0,
      losses: stats.losses ?? 0,
      zeros: stats.zeros ?? 0,
      totalFee: stats.totalFee ?? 0,
      winRate: stats.winRate ?? 0
    };
  }
  loading.value = false;
}

/** 手动点击刷新按钮（失败时提示用户） */
function handleManualRefresh() {
  refreshTrades(true);
}

onMounted(() => {
  refreshTrades();
  // 每 5 秒自动刷新一次已完成交易
  refreshTimer = setInterval(() => refreshTrades(false), 5000);
});

onUnmounted(() => {
  // 清除轮询定时器，避免内存泄漏
  if (refreshTimer) {
    clearInterval(refreshTimer);
    refreshTimer = null;
  }
});
</script>

<template>
  <div class="trades-panel">
    <div class="panel-header">
      <h2>已完成交易</h2>
      <el-button size="small" @click="handleManualRefresh" :loading="loading">
        刷新
      </el-button>
    </div>

    <!-- 摘要卡片 -->
    <div class="summary-cards">
      <div class="summary-card">
        <div class="card-value" style="color: #4068c9">{{ totalCount }}</div>
        <div class="card-label">总平仓笔数</div>
      </div>
      <div class="summary-card">
        <div class="card-value" :style="{ color: pnlColor(totalPnl) }">
          {{ formatSignedUsd(totalPnl) }}
        </div>
        <div class="card-label">总净盈亏（USDT）</div>
      </div>
      <div class="summary-card">
        <div class="card-value" style="color: #22c55e">{{ profitCount }}</div>
        <div class="card-label">盈利笔数</div>
      </div>
      <div class="summary-card">
        <div class="card-value" style="color: #ef4444">{{ lossCount }}</div>
        <div class="card-label">亏损笔数</div>
      </div>
      <div class="summary-card">
        <div class="card-value" style="color: #4068c9">{{ winRate.toFixed(1) }}%</div>
        <div class="card-label">胜率</div>
      </div>
      <div class="summary-card">
        <div class="card-value" style="color: #f59e0b">{{ formatUsd(totalFee) }}</div>
        <div class="card-label">总手续费（USDT）</div>
      </div>
    </div>

    <!-- 筛选栏 -->
    <div class="filter-bar">
      <el-select
        v-model="filters.symbol"
        placeholder="交易对"
        clearable
        size="small"
        style="width: 150px"
        @change="resetPage"
      >
        <el-option v-for="s in symbolOptions" :key="s" :label="s" :value="s" />
      </el-select>
      <el-select
        v-model="filters.side"
        placeholder="方向"
        clearable
        size="small"
        style="width: 110px"
        @change="resetPage"
      >
        <el-option label="多" value="LONG" />
        <el-option label="空" value="SHORT" />
      </el-select>
      <el-select
        v-model="filters.closeReason"
        placeholder="平仓原因"
        clearable
        size="small"
        style="width: 130px"
        @change="resetPage"
      >
        <el-option label="止损" value="STOP_LOSS" />
        <el-option label="跟踪止损" value="TRAILING_STOP" />
        <el-option label="固定止盈" value="TAKE_PROFIT" />
        <el-option label="限价平仓" value="LIMIT_STOP" />
        <el-option label="超时平仓" value="MAX_HOLD" />
        <el-option label="回滚" value="ROLLBACK" />
        <el-option label="状态同步" value="GHOST" />
      </el-select>
      <span class="filter-count">共 {{ sortedRows.length }} 条</span>
    </div>

    <!-- 已完成交易明细表格 -->
    <div class="quant-card">
      <el-table
        :data="pagedRows"
        v-loading="loading"
        stripe
        row-key="id"
        height="560"
        :default-sort="{ prop: 'closedAt', order: 'descending' }"
        style="width: 100%"
        @sort-change="handleSortChange"
      >
        <!-- 交易对 -->
        <el-table-column prop="symbol" label="交易对" width="130" fixed="left" sortable="custom">
          <template #default="{ row }">
            <span class="symbol-base">{{ splitSymbol(row.symbol).base }}</span>
            <span class="symbol-quote">/{{ splitSymbol(row.symbol).quote }}</span>
          </template>
        </el-table-column>

        <!-- 方向 -->
        <el-table-column prop="side" label="方向" width="80" align="center">
          <template #default="{ row }">
            <span class="side-tag" :style="{ color: sideColor(row.side), borderColor: sideColor(row.side) }">
              {{ sideLabel(row.side) }}
            </span>
          </template>
        </el-table-column>

        <!-- 入场价 -->
        <el-table-column prop="entryPrice" label="入场价" width="120" align="right" sortable="custom">
          <template #default="{ row }">{{ formatPrice(row.entryPrice) }}</template>
        </el-table-column>

        <!-- 平仓原因 -->
        <el-table-column prop="closeReason" label="平仓原因" min-width="110" align="center">
          <template #default="{ row }">
            <span :style="{ color: closeReasonColor(row.closeReason) }">
              {{ closeReasonLabel(row.closeReason) }}
            </span>
          </template>
        </el-table-column>

        <!-- 净盈亏 -->
        <el-table-column prop="realizedPnl" label="净盈亏" min-width="120" align="right" sortable="custom">
          <template #default="{ row }">
            <span :style="{ color: pnlColor(row.realizedPnl) }">
              {{ formatSignedUsd(row.realizedPnl) }}
            </span>
          </template>
        </el-table-column>

        <!-- 盈利% -->
        <el-table-column prop="profitPct" label="盈利%" min-width="100" align="right" sortable="custom">
          <template #default="{ row }">
            <span :style="{ color: pnlColor(row.profitPct) }">
              {{ formatPct(row.profitPct) }}
            </span>
          </template>
        </el-table-column>

        <!-- 出场价 -->
        <el-table-column prop="exitPrice" label="出场价" min-width="120" align="right" sortable="custom">
          <template #default="{ row }">{{ formatPrice(row.exitPrice) }}</template>
        </el-table-column>

        <!-- 数量 -->
        <el-table-column prop="amount" label="数量" min-width="110" align="right" sortable="custom">
          <template #default="{ row }">{{ formatAmount(row.amount) }}</template>
        </el-table-column>

        <!-- 手续费 -->
        <el-table-column prop="fee" label="手续费" min-width="100" align="right" sortable="custom">
          <template #default="{ row }">{{ formatUsd(row.fee) }}</template>
        </el-table-column>

        <!-- 平仓时间 -->
        <el-table-column prop="closedAt" label="平仓时间" min-width="170" sortable="custom">
          <template #default="{ row }">{{ formatDateTime(row.closedAt) }}</template>
        </el-table-column>

        <template #empty>
          <div class="empty-state">暂无已完成交易</div>
        </template>
      </el-table>

      <!-- 分页 -->
      <div class="pagination-bar">
        <el-pagination
          v-model:current-page="currentPage"
          v-model:page-size="pageSize"
          :total="sortedRows.length"
          :page-sizes="[20, 50, 100]"
          layout="total, sizes, prev, pager, next"
          background
          small
        />
      </div>
    </div>
  </div>
</template>

<style scoped>
.trades-panel {
  padding: 20px;
}
.panel-header {
  display: flex;
  align-items: center;
  justify-content: space-between;
  margin-bottom: 20px;
}
.panel-header h2 {
  margin: 0;
  color: var(--quant-text, #e0e0e0);
  font-size: 20px;
}
/* 摘要卡片区域 */
.summary-cards {
  display: flex;
  gap: 16px;
  margin-bottom: 20px;
  flex-wrap: wrap;
}
.summary-card {
  flex: 1;
  min-width: 140px;
  background: var(--quant-card, #1d1f27);
  border: 1px solid var(--quant-border, #2c2f3a);
  border-radius: 8px;
  padding: 16px 20px;
  text-align: center;
}
.card-value {
  font-size: 26px;
  font-weight: 700;
  line-height: 1.2;
}
.card-label {
  margin-top: 4px;
  font-size: 13px;
  color: var(--quant-text-secondary, #8b8fa3);
}
/* 筛选栏 */
.filter-bar {
  display: flex;
  align-items: center;
  gap: 12px;
  margin-bottom: 16px;
}
.filter-count {
  margin-left: auto;
  font-size: 13px;
  color: var(--quant-text-secondary, #8b8fa3);
}
/* 表格卡片 */
.quant-card {
  background: var(--quant-card, #1d1f27);
  border: 1px solid var(--quant-border, #2c2f3a);
  border-radius: 8px;
  padding: 16px;
}
.symbol-base {
  color: var(--quant-text, #e0e0e0);
  font-weight: 600;
}
.symbol-quote {
  color: var(--quant-text-secondary, #8b8fa3);
}
.side-tag {
  display: inline-block;
  padding: 1px 8px;
  border: 1px solid;
  border-radius: 4px;
  font-size: 12px;
  font-weight: 600;
}
.pagination-bar {
  display: flex;
  justify-content: flex-end;
  margin-top: 16px;
}
.empty-state {
  text-align: center;
  padding: 40px;
  color: var(--quant-text-secondary, #8b8fa3);
}
</style>
