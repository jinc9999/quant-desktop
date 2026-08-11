<script setup lang="ts">
/**
 * 委托状态面板
 * 展示当前所有委托的表格数据，支持展开查看事件流水
 * 通过 Wails 绑定调用 Go 后端 QuantService.GetOrders / GetOrderEvents / CancelOrder
 */
import { ref, computed, onMounted, onUnmounted } from "vue";
import { ElMessageBox } from "element-plus";
import { showError, showSuccess } from "../../utils/error-handler";
import { callService } from "../../utils/service";
import { QuantService } from "../../../bindings/quant-desktop/internal/bindings";

defineOptions({ name: "Orders" });

/** 委托行数据（与 Go 后端 storage.Order 模型字段一致） */
interface OrderRow {
  id: number;
  positionId: number;
  exchangeOrderId: number;
  symbol: string;
  orderType: string;
  side: string;
  status: string;
  stopPrice: number | null;
  activationPrice: number | null;
  callbackRate: number | null;
  amount: number;
  filledPrice: number | null;
  filledAmount: number | null;
  createdAt: number;
  updatedAt: number;
}

/** 委托事件数据（与 Go 后端 storage.OrderEvent 模型字段一致） */
interface OrderEventRow {
  id: number;
  orderId: number;
  exchangeOrderId: number;
  eventType: string;
  oldStatus: string | null;
  newStatus: string | null;
  price: number | null;
  message: string | null;
  timestamp: number;
}

// 委托数据
const orders = ref<OrderRow[]>([]);
const loading = ref(false);
// 自动刷新开关
const autoRefresh = ref(true);
// 自动刷新定时器
let refreshTimer: ReturnType<typeof setInterval> | null = null;
// 事件缓存（orderId -> events）
const eventsMap = ref<Record<number, OrderEventRow[]>>({});
const eventsLoading = ref<Record<number, boolean>>({});

/** 活跃委托数（NEW / PARTIALLY_FILLED） */
const activeCount = computed(
  () => orders.value.filter(o => o.status === "NEW" || o.status === "PARTIALLY_FILLED").length
);
/** 已成交委托数（FILLED） */
const filledCount = computed(
  () => orders.value.filter(o => o.status === "FILLED").length
);
/** 已取消委托数（CANCELED / EXPIRED / REJECTED） */
const canceledCount = computed(
  () => orders.value.filter(o => ["CANCELED", "EXPIRED", "REJECTED"].includes(o.status)).length
);

// 状态筛选（all / active / filled / canceled），默认全部
const statusFilter = ref<"all" | "active" | "filled" | "canceled">("all");

/** 状态分组映射：筛选键 -> 包含的委托状态集合 */
const statusGroups: Record<string, string[]> = {
  active: ["NEW", "PARTIALLY_FILLED"],
  filled: ["FILLED"],
  canceled: ["CANCELED", "EXPIRED", "REJECTED"],
};

/** 筛选后的委托列表（表格实际渲染数据） */
const filteredOrders = computed(() => {
  if (statusFilter.value === "all") return orders.value;
  const group = statusGroups[statusFilter.value];
  return orders.value.filter(o => group.includes(o.status));
});

/** 分页：表格只渲染当前页，避免 744+ 行委托一次性渲染打满渲染进程（Mac 白屏同款问题） */
const currentPage = ref(1);
const pageSize = ref(50);
const pagedOrders = computed(() => {
  const start = (currentPage.value - 1) * pageSize.value;
  return filteredOrders.value.slice(start, start + pageSize.value);
});

/**
 * 将毫秒时间戳格式化为 HH:mm:ss
 * @param ts 毫秒时间戳，为空或无效时返回 "-"
 * @returns 格式化后的时间字符串
 */
function formatTime(ts: number | null | undefined): string {
  if (!ts) return "-";
  const d = new Date(ts);
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`;
}

/**
 * 获取委托类型中文名称
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
 * 获取委托状态中文名称
 * @param status 委托状态（NEW / PARTIALLY_FILLED / FILLED / CANCELED / EXPIRED / REJECTED）
 * @returns 中文名称，未知状态返回原值
 */
function statusLabel(status: string | null | undefined): string {
  switch (status) {
    case "NEW":
      return "未成交";
    case "PARTIALLY_FILLED":
      return "部分成交";
    case "FILLED":
      return "已成交";
    case "CANCELED":
      return "已取消";
    case "EXPIRED":
      return "已过期";
    case "REJECTED":
      return "已拒绝";
    default:
      return status || "-";
  }
}

/**
 * 获取状态对应的 el-tag type
 * @param status 委托状态
 * @returns el-tag 的 type 值
 */
function statusTagType(status: string): "primary" | "warning" | "success" | "info" | "danger" {
  switch (status) {
    case "NEW":
      return "primary";
    case "PARTIALLY_FILLED":
      return "warning";
    case "FILLED":
      return "success";
    case "CANCELED":
      return "info";
    case "EXPIRED":
      return "info";
    case "REJECTED":
      return "danger";
    default:
      return "info";
  }
}

/**
 * 判断委托是否为活跃状态（可取消）
 * @param status 委托状态
 * @returns 是否活跃
 */
function isActive(status: string): boolean {
  return status === "NEW" || status === "PARTIALLY_FILLED";
}

/**
 * 刷新委托数据：调用 Go 绑定获取委托列表
 * @param showError 失败时是否弹出错误提示（轮询调用传 false 避免刷屏）
 */
async function refreshOrders(showErrorFlag = false) {
  loading.value = true;
  const list = await callService(
    () => QuantService.GetOrders(""),
    { silent: !showErrorFlag, context: "获取委托" }
  );
  if (list !== null) {
    const next = (list || []) as OrderRow[];
    // 数据未变化时跳过赋值，避免整表无谓重渲染（744 行 × 3 秒刷新曾打满渲染进程）
    if (JSON.stringify(next) !== JSON.stringify(orders.value)) {
      orders.value = next;
      // 当前页超出范围（数据变少）时回退到最后一页
      const maxPage = Math.max(1, Math.ceil(filteredOrders.value.length / pageSize.value));
      if (currentPage.value > maxPage) currentPage.value = maxPage;
    }
  }
  loading.value = false;
}

/**
 * 展开行时加载委托事件流水
 * @param row 当前操作的委托行
 * @param expanded 是否展开（Element Plus expand-change 第二参数）
 */
function handleExpandChange(row: any, expanded: any) {
  if (!expanded) return;
  const orderRow = row as OrderRow;
  // 已缓存则不重复请求
  if (eventsMap.value[orderRow.id]) return;
  eventsLoading.value[orderRow.id] = true;
  QuantService
    .GetOrderEvents(orderRow.id, 50)
    .then((events: any) => {
      eventsMap.value[orderRow.id] = (events || []) as OrderEventRow[];
    })
    .catch(() => {
      eventsMap.value[orderRow.id] = [];
    })
    .finally(() => {
      eventsLoading.value[orderRow.id] = false;
    });
}

/**
 * 取消委托：弹出确认框后调用后端 CancelOrder
 * @param row 要取消的委托行（模板插槽传入，类型为 any）
 */
async function handleCancel(row: any) {
  const order = row as OrderRow;
  try {
    await ElMessageBox.confirm(
      `确定要取消委托 ${order.symbol} #${order.id} 吗？`,
      "取消委托",
      { confirmButtonText: "确定", cancelButtonText: "取消", type: "warning" }
    );
  } catch {
    return;
  }
  const result = await callService(
    () => QuantService.CancelOrder(order.id),
    { context: "取消委托" }
  );
  if (result !== null) {
    showSuccess(result || "委托已取消");
    refreshOrders(true);
  }
}

/** 手动点击刷新按钮（失败时提示用户） */
function handleManualRefresh() {
  refreshOrders(true);
}

/**
 * 切换自动刷新开关
 * @param val 开关新状态
 */
function toggleAutoRefresh(val: boolean | string | number) {
  if (val) {
    if (!refreshTimer) refreshTimer = setInterval(() => refreshOrders(false), 3000);
  } else if (refreshTimer) {
    clearInterval(refreshTimer);
    refreshTimer = null;
  }
}

onMounted(() => {
  refreshOrders();
  // 每 3 秒自动刷新一次委托
  refreshTimer = setInterval(() => refreshOrders(false), 3000);
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
  <div class="orders-panel">
    <div class="panel-header">
      <h2>委托状态</h2>
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

    <!-- 摘要卡片 -->
    <div class="summary-cards">
      <div class="summary-card">
        <div class="card-value" style="color: #4068c9">{{ activeCount }}</div>
        <div class="card-label">活跃委托</div>
      </div>
      <div class="summary-card">
        <div class="card-value" style="color: #22c55e">{{ filledCount }}</div>
        <div class="card-label">已成交</div>
      </div>
      <div class="summary-card">
        <div class="card-value" style="color: #6b7280">{{ canceledCount }}</div>
        <div class="card-label">已取消</div>
      </div>
    </div>

    <!-- 状态筛选栏 -->
    <div class="filter-bar">
      <el-radio-group v-model="statusFilter" size="small" @change="currentPage = 1">
        <el-radio-button label="all">全部 {{ orders.length }}</el-radio-button>
        <el-radio-button label="active">活跃 {{ activeCount }}</el-radio-button>
        <el-radio-button label="filled">已成交 {{ filledCount }}</el-radio-button>
        <el-radio-button label="canceled">已取消 {{ canceledCount }}</el-radio-button>
      </el-radio-group>
    </div>

    <!-- 委托列表 -->
    <div class="quant-card">
      <el-table
        :data="pagedOrders"
        v-loading="loading"
        stripe
        row-key="id"
        style="width: 100%"
        @expand-change="handleExpandChange"
      >
        <el-table-column type="expand">
          <template #default="{ row }">
            <div class="events-wrapper" v-loading="eventsLoading[row.id]">
              <div v-if="!eventsMap[row.id] || eventsMap[row.id].length === 0" class="events-empty">
                暂无事件
              </div>
              <el-table
                v-else
                :data="eventsMap[row.id]"
                size="small"
                stripe
              >
                <el-table-column label="时间" width="100">
                  <template #default="{ row: evt }">{{ formatTime(evt.timestamp) }}</template>
                </el-table-column>
                <el-table-column prop="eventType" label="事件类型" width="140" />
                <el-table-column label="状态变更" width="200">
                  <template #default="{ row: evt }">
                    {{ statusLabel(evt.oldStatus) }} → {{ statusLabel(evt.newStatus) }}
                  </template>
                </el-table-column>
                <el-table-column label="价格" width="120">
                  <template #default="{ row: evt }">{{ evt.price ?? "-" }}</template>
                </el-table-column>
                <el-table-column prop="message" label="消息" min-width="200">
                  <template #default="{ row: evt }">{{ evt.message || "-" }}</template>
                </el-table-column>
              </el-table>
            </div>
          </template>
        </el-table-column>
        <el-table-column prop="symbol" label="交易对" width="140" />
        <el-table-column label="类型" width="110">
          <template #default="{ row }">{{ orderTypeLabel(row.orderType) }}</template>
        </el-table-column>
        <el-table-column label="触发价" width="120">
          <template #default="{ row }">{{ row.stopPrice ?? "-" }}</template>
        </el-table-column>
        <el-table-column label="激活价" width="130">
          <template #default="{ row }">
            {{ row.orderType === "TRAILING_STOP_MARKET" && row.activationPrice != null
              ? row.activationPrice.toFixed(6)
              : "-" }}
          </template>
        </el-table-column>
        <el-table-column label="回调率" width="100">
          <template #default="{ row }">
            {{ row.orderType === "TRAILING_STOP_MARKET" && row.callbackRate != null
              ? row.callbackRate + "%"
              : "-" }}
          </template>
        </el-table-column>
        <el-table-column label="成交价" width="130">
          <template #default="{ row }">
            {{ row.status === "FILLED" && row.filledPrice != null
              ? row.filledPrice.toFixed(6)
              : "-" }}
          </template>
        </el-table-column>
        <el-table-column prop="positionId" label="持仓ID" width="90" />
        <el-table-column prop="amount" label="数量" width="120" />
        <el-table-column label="状态" width="140">
          <template #default="{ row }">
            <el-tag :type="statusTagType(row.status)" size="small">{{ statusLabel(row.status) }}</el-tag>
          </template>
        </el-table-column>
        <el-table-column label="创建时间" width="110">
          <template #default="{ row }">{{ formatTime(row.createdAt) }}</template>
        </el-table-column>
        <el-table-column label="操作" min-width="100">
          <template #default="{ row }">
            <el-button
              v-if="isActive(row.status)"
              size="small"
              type="danger"
              @click.stop="handleCancel(row)"
            >
              取消
            </el-button>
            <span v-else class="no-action">-</span>
          </template>
        </el-table-column>
      </el-table>

      <div class="pagination-bar">
        <el-pagination
          v-model:current-page="currentPage"
          v-model:page-size="pageSize"
          :total="filteredOrders.length"
          :page-sizes="[20, 50, 100, 200]"
          layout="total, sizes, prev, pager, next"
          background
          small
        />
      </div>

      <div v-if="filteredOrders.length === 0 && !loading" class="empty-state">
        {{ statusFilter === "all" ? "暂无委托" : "该状态下暂无委托" }}
      </div>
    </div>
  </div>
</template>

<style scoped>
.orders-panel {
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
/* 摘要卡片区域 */
.summary-cards {
  display: flex;
  gap: 16px;
  margin-bottom: 20px;
}
.summary-card {
  flex: 1;
  background: var(--quant-card, #1d1f27);
  border: 1px solid var(--quant-border, #2c2f3a);
  border-radius: 8px;
  padding: 16px 20px;
  text-align: center;
}
.card-value {
  font-size: 28px;
  font-weight: 700;
  line-height: 1.2;
}
.card-label {
  margin-top: 4px;
  font-size: 13px;
  color: var(--quant-text-secondary, #8b8fa3);
}
/* 状态筛选栏 */
.filter-bar {
  display: flex;
  align-items: center;
  margin-bottom: 16px;
}
/* 表格卡片 */
.quant-card {
  background: var(--quant-card, #1d1f27);
  border: 1px solid var(--quant-border, #2c2f3a);
  border-radius: 8px;
  padding: 16px;
}
/* 分页栏 */
.pagination-bar {
  display: flex;
  justify-content: flex-end;
  margin-top: 10px;
}
/* 展开行事件区域 */
.events-wrapper {
  padding: 8px 16px;
}
.events-empty {
  text-align: center;
  padding: 12px;
  color: var(--quant-text-secondary, #8b8fa3);
  font-size: 13px;
}
.no-action {
  color: var(--quant-text-secondary, #8b8fa3);
}
.empty-state {
  text-align: center;
  padding: 40px;
  color: var(--quant-text-secondary, #8b8fa3);
}
</style>
