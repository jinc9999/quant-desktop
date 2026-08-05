<script setup lang="ts">
/**
 * 实时日志面板
 * 展示交易日志列表，支持自动滚动
 * 通过 Wails 绑定轮询 Go 后端 QuantService.GetLogs
 */
import { ref, nextTick, onMounted, onUnmounted } from "vue";
import { QuantService } from "../../../bindings/quant-desktop/internal/bindings";
import { callService } from "../../utils/service";

defineOptions({ name: "Logs" });

/** 日志条目（与 Go 后端 TradeLog 模型字段一致） */
interface LogEntry {
  id: number;
  timestamp: number;
  level: string;
  module: string;
  message: string;
  symbol: string;
  price: number;
  amount: number;
}

// 日志数据
const logs = ref<LogEntry[]>([]);
// 自动滚动开关
const autoScroll = ref(true);
// 日志容器 DOM 引用
const logContainer = ref<HTMLElement | null>(null);
// 轮询定时器
let logTimer: ReturnType<typeof setInterval> | null = null;
// 本地日志上限，超出后裁剪旧条目
const MAX_LOCAL_LOGS = 1000;

/**
 * 将毫秒时间戳格式化为 HH:mm:ss
 * @param ts 毫秒时间戳
 * @returns 格式化后的时间字符串
 */
function formatTime(ts: number): string {
  const d = new Date(ts);
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`;
}

/** 滚动到日志底部（自动滚动开启时） */
function scrollToBottom() {
  if (!autoScroll.value) return;
  nextTick(() => {
    if (logContainer.value) {
      logContainer.value.scrollTop = logContainer.value.scrollHeight;
    }
  });
}

/**
 * 拉取最近日志并按 id 去重合并，按时间正序显示
 */
async function fetchLogs() {
  const list = await callService(() => QuantService.GetLogs(200), { silent: true });
  if (!list || list.length === 0) return;
  const knownIds = new Set(logs.value.map(l => l.id));
  const incoming = (list as LogEntry[]).filter(l => !knownIds.has(l.id));
  if (incoming.length === 0) return;
  const merged = logs.value.concat(incoming);
  merged.sort((a, b) => a.timestamp - b.timestamp || a.id - b.id);
  logs.value =
    merged.length > MAX_LOCAL_LOGS ? merged.slice(-MAX_LOCAL_LOGS) : merged;
  scrollToBottom();
}

/** 清空本地日志显示 */
function clearLogs() {
  logs.value = [];
}

/**
 * 获取日志级别对应颜色
 * @param level 日志级别（error / warn / info 等）
 * @returns 十六进制颜色值
 */
function levelColor(level: string): string {
  switch (level) {
    case "error":
      return "#ef4444";
    case "warn":
      return "#f59e0b";
    default:
      return "#8b8fa3";
  }
}

onMounted(() => {
  fetchLogs();
  // 每 2 秒轮询一次日志
  logTimer = setInterval(fetchLogs, 2000);
});

onUnmounted(() => {
  // 清除轮询定时器，避免内存泄漏
  if (logTimer) {
    clearInterval(logTimer);
    logTimer = null;
  }
});
</script>

<template>
  <div class="logs-panel">
    <div class="panel-header">
      <h2>交易日志</h2>
      <div class="header-actions">
        <el-checkbox v-model="autoScroll" size="small">自动滚动</el-checkbox>
        <el-button size="small" @click="clearLogs">清空</el-button>
      </div>
    </div>

    <div class="quant-card log-container" ref="logContainer">
      <div v-if="logs.length === 0" class="empty-state">
        暂无日志
      </div>
      <div v-for="log in logs" :key="log.id" class="log-entry">
        <span class="log-time">{{ formatTime(log.timestamp) }}</span>
        <span class="log-level" :style="{ color: levelColor(log.level) }">
          [{{ log.level.toUpperCase() }}]
        </span>
        <span class="log-module">{{ log.module }}</span>
        <span v-if="log.symbol" class="log-symbol">{{ log.symbol }}</span>
        <span class="log-message">{{ log.message }}</span>
      </div>
    </div>
  </div>
</template>

<style scoped>
.logs-panel {
  padding: 20px;
  height: calc(100vh - 120px);
  display: flex;
  flex-direction: column;
}
.panel-header {
  display: flex;
  align-items: center;
  justify-content: space-between;
  margin-bottom: 16px;
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
  padding: 12px;
}
.log-container {
  flex: 1;
  overflow-y: auto;
  font-family: "JetBrains Mono", "Fira Code", monospace;
  font-size: 12px;
  line-height: 1.8;
}
.log-entry {
  display: flex;
  gap: 8px;
  padding: 2px 0;
  border-bottom: 1px solid rgba(44, 47, 58, 0.5);
}
.log-time {
  color: #6b7280;
  white-space: nowrap;
}
.log-level {
  font-weight: 600;
  white-space: nowrap;
}
.log-module {
  color: #4068c9;
  white-space: nowrap;
}
.log-symbol {
  color: #22c55e;
  white-space: nowrap;
}
.log-message {
  color: var(--quant-text, #e0e0e0);
  word-break: break-all;
}
.empty-state {
  text-align: center;
  padding: 40px;
  color: var(--quant-text-secondary, #8b8fa3);
}
</style>
