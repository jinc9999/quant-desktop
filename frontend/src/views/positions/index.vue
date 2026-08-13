<script setup lang="ts">
/**
 * 实时持仓面板
 * 展示当前所有持仓的完整表格：交易对/方向/入场价/入场时间/数量/保证金/标记价格/盈亏/回报率/止损价/委托状态
 * 支持列排序、条件筛选、行展开（关联委托）、分页加载、固定表头 + 双向滚动
 * 通过 Wails 绑定调用 Go 后端 QuantService.GetPositionDetails（后端已聚合标记价格/保证金/盈亏/回报率/委托状态）
 */
import { ref, shallowRef, reactive, computed, watch, onMounted, onUnmounted } from "vue";
import { showError } from "../../utils/error-handler";
import { callService } from "../../utils/service";
import { QuantService } from "../../../bindings/quant-desktop/internal/bindings";
import type {
  PositionDetail,
  OrderBrief
} from "../../../bindings/quant-desktop/internal/bindings";

defineOptions({ name: "Positions" });

/** 委托状态展示元信息（标签文案 + el-tag 类型 + 视觉样式） */
interface StatusMeta {
  label: string;
  type: "primary" | "warning" | "success" | "info" | "danger";
  effect: "dark" | "light" | "plain";
}

/** 聚合委托状态 → 展示元信息映射（与后端 deriveOrderStatus 返回值对应） */
const statusMeta: Record<string, StatusMeta> = {
  NEW: { label: "未成交", type: "primary", effect: "light" },
  PARTIALLY_FILLED: { label: "部分成交", type: "warning", effect: "light" },
  FILLED: { label: "已成交", type: "success", effect: "light" },
  CANCELED: { label: "已取消", type: "info", effect: "light" },
  TRAILING: { label: "跟踪止损", type: "warning", effect: "plain" },
  NONE: { label: "无委托", type: "info", effect: "plain" }
};

/** 委托状态筛选下拉选项 */
const statusOptions = [
  { label: "未成交", value: "NEW" },
  { label: "部分成交", value: "PARTIALLY_FILLED" },
  { label: "已成交", value: "FILLED" },
  { label: "已取消", value: "CANCELED" },
  { label: "跟踪止损", value: "TRAILING" },
  { label: "无委托", value: "NONE" }
];

const WEEKDAYS = ["周日", "周一", "周二", "周三", "周四", "周五", "周六"];

// ==================== 响应式状态 ====================

/** 持仓数据（shallowRef：整体替换数组，避免大列表深度响应开销） */
const positions = shallowRef<PositionDetail[]>([]);
const loading = ref(false);
/** 当前运行模式（模拟盘/实盘，用于明确数据性质） */
const modeLabel = ref("");
/** 自动刷新开关 */
const autoRefresh = ref(true);
/** 自动刷新定时器 */
let refreshTimer: ReturnType<typeof setInterval> | null = null;
/** 上次数据快照（JSON），轮询无变化时跳过赋值，避免无效重渲染 */
let lastSnapshot = "";

/** 筛选条件（交易对 / 方向 / 委托状态，空字符串表示不筛选） */
const filters = reactive({ symbol: "", side: "", orderStatus: "" });

/** 排序状态（prop 为列字段，order 为升/降序，null 表示不排序） */
const sortState = ref<{ prop: string; order: "ascending" | "descending" | null }>({
  prop: "openedAt",
  order: "descending"
});

/** 分页状态 */
const currentPage = ref(1);
const pageSize = ref(20);

/** 表格可视区高度（固定表头 + 内容垂直滚动，随窗口尺寸自适应） */
const tableHeight = ref(420);

// ==================== 格式化工具 ====================

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
 * 生成完整时间戳描述（悬停提示用）：精确到毫秒 + 星期 + 原始时间戳
 * @param ts 毫秒时间戳
 * @returns 完整时间戳字符串
 */
function fullTimestamp(ts: number): string {
  const d = new Date(ts);
  return `${formatDateTime(ts)}.${String(d.getMilliseconds()).padStart(3, "0")}（${WEEKDAYS[d.getDay()]}） · 时间戳 ${ts}`;
}

/**
 * 价格格式化：按量级自适应小数位（≥1000 保留 2 位并千分位，≥1 保留 4 位，其余 6 位）
 * @param v 价格数值，为空或非正数时返回 "-"
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
 * 盈亏格式化：正数带 "+" 前缀，固定 2 位小数
 * @param v 盈亏数值
 * @returns 带符号的盈亏字符串
 */
function formatPnl(v: number): string {
  const s = v.toLocaleString("en-US", { minimumFractionDigits: 2, maximumFractionDigits: 2 });
  return v > 0 ? "+" + s : s;
}

/**
 * 回报率格式化：百分比 + 符号，固定 2 位小数
 * @param v 回报率数值（已含 ×100）
 * @returns 带符号的百分比字符串
 */
function formatRoi(v: number): string {
  const s = Math.abs(v).toFixed(2) + "%";
  return v > 0 ? "+" + s : v < 0 ? "-" + s : s;
}

/**
 * 根据盈亏正负返回颜色样式类（正=绿/负=红/零=灰）
 * @param v 盈亏或回报率数值
 * @returns CSS 类名
 */
function pnlClass(v: number): string {
  return v > 0 ? "pnl-pos" : v < 0 ? "pnl-neg" : "pnl-zero";
}

/**
 * 拆分交易对为基础币与计价币（如 BTCUSDT → BTC + USDT）
 * @param symbol 交易对原始名称
 * @returns base 基础币、quote 计价币（无法识别时 quote 为空串）
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
 * 获取委托类型中文名称（展开行关联委托列表用）
 * @param type 委托类型（STOP_MARKET / TRAILING_STOP_MARKET）
 * @returns 中文名称
 */
function orderTypeLabel(type: string): string {
  switch (type) {
    case "STOP_MARKET":
      return "止损";
    case "TRAILING_STOP_MARKET":
      return "跟踪止损";
    default:
      return type;
  }
}

/**
 * 获取委托状态的展示元信息（未匹配时兜底为灰色 info 标签）
 * @param status 聚合委托状态码
 * @returns 状态展示元信息
 */
function getStatusMeta(status: string): StatusMeta {
  return statusMeta[status] ?? { label: status, type: "info", effect: "light" };
}

// ==================== 计算属性（筛选 → 排序 → 分页） ====================

/** 交易对筛选选项（从当前持仓数据去重生成） */
const symbolOptions = computed(() => {
  const set = new Set<string>();
  for (const p of positions.value) set.add(p.symbol);
  return Array.from(set).sort();
});

/** 各列排序比较器映射（数值列按差值、文本列按 localeCompare） */
const comparators: Record<string, (a: PositionDetail, b: PositionDetail) => number> = {
  symbol: (a, b) => a.symbol.localeCompare(b.symbol),
  side: (a, b) => a.side.localeCompare(b.side),
  entryPrice: (a, b) => a.entryPrice - b.entryPrice,
  openedAt: (a, b) => a.openedAt - b.openedAt,
  amount: (a, b) => a.amount - b.amount,
  margin: (a, b) => a.margin - b.margin,
  markPrice: (a, b) => a.markPrice - b.markPrice,
  unrealizedPnl: (a, b) => a.unrealizedPnl - b.unrealizedPnl,
  roiPct: (a, b) => a.roiPct - b.roiPct,
  currentStopPrice: (a, b) => a.currentStopPrice - b.currentStopPrice,
  orderStatus: (a, b) => a.orderStatus.localeCompare(b.orderStatus)
};

/** 筛选后的持仓列表（交易对 / 方向 / 委托状态三重过滤） */
const filteredRows = computed(() => {
  let rows = positions.value;
  if (filters.symbol) rows = rows.filter(r => r.symbol === filters.symbol);
  if (filters.side) rows = rows.filter(r => r.side === filters.side);
  if (filters.orderStatus) rows = rows.filter(r => r.orderStatus === filters.orderStatus);
  return rows;
});

/** 排序后的持仓列表（对全量筛选结果排序，保证跨页排序正确） */
const sortedRows = computed(() => {
  const { prop, order } = sortState.value;
  if (!prop || !order) return filteredRows.value;
  const cmp = comparators[prop];
  if (!cmp) return filteredRows.value;
  const rows = [...filteredRows.value];
  rows.sort((a, b) => (order === "ascending" ? cmp(a, b) : cmp(b, a)));
  return rows;
});

/** 当前页数据（分页切片，限制 DOM 行数保证大数据量滚动流畅） */
const pagedRows = computed(() => {
  const start = (currentPage.value - 1) * pageSize.value;
  return sortedRows.value.slice(start, start + pageSize.value);
});

/** 筛选结果汇总（合计行用：总保证金 / 总未实现盈亏 / 是否有行情） */
const totals = computed(() => {
  let margin = 0;
  let pnl = 0;
  let hasPrice = false;
  for (const r of filteredRows.value) {
    margin += r.margin;
    pnl += r.unrealizedPnl;
    if (r.markPrice > 0) hasPrice = true;
  }
  return { margin, pnl, hasPrice, count: filteredRows.value.length };
});

// ==================== 表格事件 ====================

/**
 * 表头排序变更回调（sortable="custom" 模式，自行维护排序状态）
 * @param payload Element Plus sort-change 事件参数（prop/order）
 */
function handleSortChange(payload: { prop: string | null; order: "ascending" | "descending" | null }) {
  sortState.value = { prop: payload.prop ?? "", order: payload.order };
}

/**
 * 自定义合计行：基于全量筛选结果计算总保证金与总盈亏（而非当前页）
 * @param param Element Plus summary-method 参数（columns 为当前列定义）
 * @returns 各列合计文本数组
 */
function getSummaries(param: { columns: any[] }): string[] {
  return param.columns.map((col, idx) => {
    if (idx === 0) return `合计（${totals.value.count} 笔）`;
    if (col.property === "margin") return formatUsd(totals.value.margin);
    if (col.property === "unrealizedPnl") {
      return totals.value.hasPrice ? formatPnl(totals.value.pnl) : "-";
    }
    return "";
  });
}

/**
 * 重置全部筛选条件并回到第一页
 */
function resetFilters() {
  filters.symbol = "";
  filters.side = "";
  filters.orderStatus = "";
  currentPage.value = 1;
}

// 筛选条件变化时回到第一页
watch(filters, () => {
  currentPage.value = 1;
});

// 数据量变化时校正页码（如平仓导致行数减少，避免停留空页）
watch(
  () => sortedRows.value.length,
  len => {
    const maxPage = Math.max(1, Math.ceil(len / pageSize.value));
    if (currentPage.value > maxPage) currentPage.value = maxPage;
  }
);

// ==================== 数据加载 ====================

/**
 * 刷新持仓数据：调用 Go 绑定获取持仓明细列表
 * 轮询时做 JSON 快照对比，数据无变化则跳过赋值以减少重渲染
 * @param showError 失败时是否弹出错误提示（轮询调用传 false 避免刷屏）
 * @param showLoading 是否显示全屏加载遮罩（手动刷新/首次加载传 true）
 */
async function refreshPositions(showErrorFlag = false, showLoading = false) {
  if (showLoading || positions.value.length === 0) loading.value = true;
  const list = await callService(
    () => QuantService.GetPositionDetails(),
    { silent: !showErrorFlag, context: "获取持仓" }
  );
  if (list !== null) {
    const rows = (list || []) as PositionDetail[];
    const snapshot = JSON.stringify(rows);
    if (snapshot !== lastSnapshot) {
      lastSnapshot = snapshot;
      positions.value = rows;
    }
  }
  loading.value = false;
}

/**
 * 手动点击刷新按钮（显示加载遮罩，失败时提示用户）
 */
function handleManualRefresh() {
  refreshPositions(true, true);
}

/**
 * 切换自动刷新开关
 * @param val 开关新状态
 */
function toggleAutoRefresh(val: boolean | string | number) {
  if (val) {
    refreshTimer = setInterval(() => refreshPositions(false), 3000);
  } else if (refreshTimer) {
    clearInterval(refreshTimer);
    refreshTimer = null;
  }
}

/**
 * 计算表格可视区高度：窗口高度减去顶部导航、标题、筛选栏、分页等固定区域
 */
function calcTableHeight() {
  tableHeight.value = Math.max(300, window.innerHeight - 330);
}

onMounted(() => {
  calcTableHeight();
  window.addEventListener("resize", calcTableHeight);
  refreshPositions();
  callService(() => QuantService.GetMode(), { silent: true }).then((r) => {
    if (r) modeLabel.value = r;
  });
  // 每 3 秒自动刷新一次持仓
  refreshTimer = setInterval(() => refreshPositions(false), 3000);
});

onUnmounted(() => {
  window.removeEventListener("resize", calcTableHeight);
  // 清除轮询定时器，避免内存泄漏
  if (refreshTimer) {
    clearInterval(refreshTimer);
    refreshTimer = null;
  }
});
</script>

<template>
  <div class="positions-panel">
    <div class="panel-header">
      <h2>实时持仓</h2>
      <el-tag size="small" :type="modeLabel === 'LIVE' ? 'danger' : 'warning'">{{ modeLabel === 'LIVE' ? '实盘' : '模拟盘' }}</el-tag>
      <div class="header-actions">
        <el-switch
          v-model="autoRefresh"
          size="small"
          active-text="自动刷新"
          @change="toggleAutoRefresh"
        />
        <el-button size="small" @click="handleManualRefresh" :loading="loading">
          刷新
        </el-button>
      </div>
    </div>

    <div class="quant-card">
      <!-- 筛选工具栏 -->
      <div class="toolbar">
        <div class="filters">
          <el-select
            v-model="filters.symbol"
            placeholder="全部交易对"
            clearable
            filterable
            size="small"
            class="filter-select"
          >
            <el-option v-for="s in symbolOptions" :key="s" :label="s" :value="s" />
          </el-select>
          <el-select
            v-model="filters.side"
            placeholder="全部方向"
            clearable
            size="small"
            class="filter-select filter-select--narrow"
          >
            <el-option label="多" value="LONG" />
            <el-option label="空" value="SHORT" />
          </el-select>
          <el-select
            v-model="filters.orderStatus"
            placeholder="全部委托状态"
            clearable
            size="small"
            class="filter-select"
          >
            <el-option
              v-for="opt in statusOptions"
              :key="opt.value"
              :label="opt.label"
              :value="opt.value"
            />
          </el-select>
          <el-button size="small" text class="reset-btn" @click="resetFilters">重置</el-button>
        </div>
        <span class="total-hint">共 {{ sortedRows.length }} 个持仓</span>
      </div>

      <!-- 持仓表格：固定表头 + 垂直滚动 + 水平滚动 + 奇偶行 + 悬停高亮 + 合计行 -->
      <el-table
        :data="pagedRows"
        v-loading="loading"
        stripe
        row-key="id"
        :height="tableHeight"
        :default-sort="{ prop: 'openedAt', order: 'descending' }"
        show-summary
        :summary-method="getSummaries"
        style="width: 100%"
        @sort-change="handleSortChange"
      >
        <!-- 展开行：持仓详细信息 + 关联委托列表 -->
        <el-table-column type="expand" width="48">
          <template #default="{ row }">
            <div class="expand-detail">
              <div class="detail-grid">
                <div class="detail-item">
                  <span class="detail-label">持仓 ID</span>
                  <span class="detail-value">#{{ row.id }}</span>
                </div>
                <div class="detail-item">
                  <span class="detail-label">杠杆</span>
                  <span class="detail-value">{{ row.leverage }}x</span>
                </div>
                <div class="detail-item">
                  <span class="detail-label">持仓最高价</span>
                  <span class="detail-value num">{{ formatPrice(row.highestPrice) }}</span>
                </div>
                <div class="detail-item">
                  <span class="detail-label">跟踪止损</span>
                  <span class="detail-value">
                    {{ row.trailingActive ? "已激活" : "未激活" }}
                  </span>
                </div>
                <div class="detail-item">
                  <span class="detail-label">保证金</span>
                  <span class="detail-value num">{{ formatUsd(row.margin) }} USDT</span>
                </div>
                <div class="detail-item">
                  <span class="detail-label">完整开仓时间</span>
                  <span class="detail-value">{{ fullTimestamp(row.openedAt) }}</span>
                </div>
              </div>

              <div class="orders-title">关联委托（{{ (row.orders as OrderBrief[]).length }}）</div>
              <el-table
                v-if="(row.orders as OrderBrief[]).length > 0"
                :data="row.orders"
                size="small"
                stripe
              >
                <el-table-column label="类型" width="110">
                  <template #default="{ row: od }">{{ orderTypeLabel(od.orderType) }}</template>
                </el-table-column>
                <el-table-column prop="side" label="方向" width="80" />
                <el-table-column label="状态" width="110">
                  <template #default="{ row: od }">
                    <el-tag
                      :type="getStatusMeta(od.status).type"
                      size="small"
                    >{{ od.status }}</el-tag>
                  </template>
                </el-table-column>
                <el-table-column label="触发价" width="130">
                  <template #default="{ row: od }">
                    <span class="num">{{ formatPrice(od.stopPrice) }}</span>
                  </template>
                </el-table-column>
                <el-table-column label="激活价" width="130">
                  <template #default="{ row: od }">
                    <span class="num">{{ formatPrice(od.activationPrice) }}</span>
                  </template>
                </el-table-column>
                <el-table-column label="回调率" width="90">
                  <template #default="{ row: od }">
                    {{ od.callbackRate != null ? od.callbackRate + "%" : "-" }}
                  </template>
                </el-table-column>
                <el-table-column label="成交价" width="130">
                  <template #default="{ row: od }">
                    <span class="num">{{ formatPrice(od.filledPrice) }}</span>
                  </template>
                </el-table-column>
                <el-table-column label="创建时间" min-width="160">
                  <template #default="{ row: od }">{{ formatDateTime(od.createdAt) }}</template>
                </el-table-column>
              </el-table>
              <div v-else class="orders-empty">暂无关联委托</div>
            </div>
          </template>
        </el-table-column>

        <!-- 交易对（左侧固定，横向滚动时保持可见） -->
        <el-table-column prop="symbol" label="交易对" width="140" fixed="left" sortable="custom">
          <template #default="{ row }">
            <span class="symbol-base">{{ splitSymbol(row.symbol).base }}</span>
            <span class="symbol-quote">/{{ splitSymbol(row.symbol).quote }}</span>
          </template>
        </el-table-column>

        <!-- 方向：多=绿 / 空=红 -->
        <el-table-column prop="side" label="方向" width="90" sortable="custom">
          <template #default="{ row }">
            <span :class="row.side === 'LONG' ? 'side-long' : 'side-short'">
              {{ row.side === "LONG" ? "▲ 多" : "▼ 空" }}
            </span>
          </template>
        </el-table-column>

        <el-table-column prop="margin" label="保证金" min-width="120" align="right" sortable="custom">
          <template #default="{ row }">
            <span class="num">{{ formatUsd(row.margin) }}</span>
          </template>
        </el-table-column>

        <!-- 标记价格：无行情时显示 "-" -->
        <el-table-column prop="markPrice" label="标记价格" min-width="120" align="right" sortable="custom">
          <template #default="{ row }">
            <span class="num">{{ row.markPrice > 0 ? formatPrice(row.markPrice) : "-" }}</span>
          </template>
        </el-table-column>

        <!-- 盈亏：正=绿 / 负=红 -->
        <el-table-column prop="unrealizedPnl" label="盈亏" min-width="120" align="right" sortable="custom">
          <template #default="{ row }">
            <span class="num pnl-cell" :class="pnlClass(row.unrealizedPnl)">
              {{ row.markPrice > 0 ? formatPnl(row.unrealizedPnl) : "-" }}
            </span>
          </template>
        </el-table-column>

        <!-- 回报率：百分比，正=绿 / 负=红 -->
        <el-table-column prop="roiPct" label="回报率" min-width="100" align="right" sortable="custom">
          <template #default="{ row }">
            <span class="num pnl-cell" :class="pnlClass(row.roiPct)">
              {{ row.markPrice > 0 ? formatRoi(row.roiPct) : "-" }}
            </span>
          </template>
        </el-table-column>

        <el-table-column
          prop="currentStopPrice"
          label="止损价"
          min-width="120"
          align="right"
          sortable="custom"
        >
          <template #default="{ row }">
            <span class="num stop-price">{{ formatPrice(row.currentStopPrice) }}</span>
          </template>
        </el-table-column>

        <!-- 委托状态：状态标签区分 -->
        <el-table-column prop="orderStatus" label="委托状态" min-width="120" sortable="custom">
          <template #default="{ row }">
            <el-tag
              :type="getStatusMeta(row.orderStatus).type"
              :effect="getStatusMeta(row.orderStatus).effect"
              size="small"
            >{{ getStatusMeta(row.orderStatus).label }}</el-tag>
          </template>
        </el-table-column>

        <!-- 入场价 -->
        <el-table-column prop="entryPrice" label="入场价" min-width="120" align="right" sortable="custom">
          <template #default="{ row }">
            <span class="num">{{ formatPrice(row.entryPrice) }}</span>
          </template>
        </el-table-column>

        <!-- 入场时间：悬停显示完整时间戳 -->
        <el-table-column prop="openedAt" label="入场时间" min-width="160" sortable="custom">
          <template #default="{ row }">
            <el-tooltip :content="fullTimestamp(row.openedAt)" placement="top">
              <span class="cell-time">{{ formatDateTime(row.openedAt) }}</span>
            </el-tooltip>
          </template>
        </el-table-column>

        <el-table-column prop="amount" label="数量" min-width="110" align="right" sortable="custom">
          <template #default="{ row }">
            <span class="num">{{ formatAmount(row.amount) }}</span>
          </template>
        </el-table-column>

        <!-- 空状态 -->
        <template #empty>
          <div class="empty-state">暂无持仓</div>
        </template>
      </el-table>

      <!-- 分页 -->
      <div class="pagination-wrap">
        <el-pagination
          v-model:current-page="currentPage"
          v-model:page-size="pageSize"
          :page-sizes="[10, 20, 50, 100]"
          :total="sortedRows.length"
          layout="total, sizes, prev, pager, next, jumper"
          background
          small
        />
      </div>
    </div>
  </div>
</template>

<style scoped>
.positions-panel {
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
.header-actions {
  display: flex;
  align-items: center;
  gap: 12px;
}
.quant-card {
  background: var(--quant-card, #1d1f27);
  border: 1px solid var(--quant-border, #2c2f3a);
  border-radius: 8px;
  padding: 16px;
}

/* ========== 筛选工具栏 ========== */
.toolbar {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  margin-bottom: 14px;
  flex-wrap: wrap;
}
.filters {
  display: flex;
  align-items: center;
  gap: 10px;
  flex-wrap: wrap;
}
.filter-select {
  width: 160px;
}
.filter-select--narrow {
  width: 110px;
}
.reset-btn {
  color: var(--quant-text-secondary, #8b8fa3);
}
.total-hint {
  font-size: 13px;
  color: var(--quant-text-secondary, #8b8fa3);
}

/* ========== 单元格样式 ========== */
.num {
  font-variant-numeric: tabular-nums;
  font-family: "SF Mono", "JetBrains Mono", Consolas, monospace;
}
.symbol-base {
  font-weight: 700;
  color: var(--quant-text, #e0e0e0);
}
.symbol-quote {
  font-size: 12px;
  color: var(--quant-text-secondary, #8b8fa3);
}
.side-long {
  color: #22c55e;
  font-weight: 600;
}
.side-short {
  color: #ef4444;
  font-weight: 600;
}
.pnl-pos {
  color: #22c55e;
}
.pnl-neg {
  color: #ef4444;
}
.pnl-zero {
  color: var(--quant-text-secondary, #8b8fa3);
}
.pnl-cell {
  font-weight: 600;
}
.stop-price {
  color: #f59e0b;
}
.cell-time {
  cursor: default;
  border-bottom: 1px dashed transparent;
  transition: border-color 0.15s ease;
}
.cell-time:hover {
  border-bottom-color: var(--quant-text-secondary, #8b8fa3);
}

/* ========== 展开行详情 ========== */
.expand-detail {
  padding: 12px 20px 16px 48px;
  border-left: 3px solid var(--quant-border, #2c2f3a);
  margin: 4px 12px;
  background: rgba(255, 255, 255, 0.015);
  border-radius: 0 6px 6px 0;
}
.detail-grid {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(220px, 1fr));
  gap: 10px 24px;
  margin-bottom: 14px;
}
.detail-item {
  display: flex;
  align-items: baseline;
  gap: 8px;
  font-size: 13px;
}
.detail-label {
  color: var(--quant-text-secondary, #8b8fa3);
  flex-shrink: 0;
}
.detail-label::after {
  content: "：";
}
.detail-value {
  color: var(--quant-text, #e0e0e0);
}
.orders-title {
  font-size: 13px;
  font-weight: 600;
  color: var(--quant-text-secondary, #8b8fa3);
  margin-bottom: 8px;
}
.orders-empty {
  font-size: 13px;
  color: var(--quant-text-secondary, #8b8fa3);
  padding: 8px 0;
}

/* ========== 分页与空状态 ========== */
.pagination-wrap {
  display: flex;
  justify-content: flex-end;
  margin-top: 14px;
}
.empty-state {
  text-align: center;
  padding: 40px;
  color: var(--quant-text-secondary, #8b8fa3);
}

/* ========== 表格主题微调 ========== */
:deep(.el-table) {
  font-size: 13px;
}
:deep(.el-table th.el-table__cell) {
  font-weight: 600;
}
:deep(.el-table__row) {
  transition: background-color 0.15s ease;
}
:deep(.el-table__footer .cell) {
  font-weight: 600;
  color: var(--quant-text, #e0e0e0);
}
</style>
