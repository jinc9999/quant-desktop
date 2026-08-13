<template>
  <div class="service-panel">
    <div class="panel-header">
      <h2>服务状态</h2>
      <el-button type="primary" :loading="refreshing" @click="handleRefresh">
        刷新状态
      </el-button>
    </div>

    <el-alert
      v-if="license.serverUnreachable"
      type="warning"
      :closable="false"
      show-icon
      title="授权服务器连接异常"
      description="当前使用本地缓存继续运行，到期后需联网续期才能恢复"
      class="alert-item"
    />
    <el-alert
      v-else-if="expired"
      type="error"
      :closable="false"
      show-icon
      title="服务已到期"
      description="策略已自动停止，请联系管理员续费，续费成功后功能自动恢复"
      class="alert-item"
    />
    <el-alert
      v-else-if="status && !status.serviceUntilMs"
      type="warning"
      :closable="false"
      show-icon
      title="账号尚未开通服务"
      description="请联系管理员开通服务周期后使用"
      class="alert-item"
    />

    <div class="status-grid">
      <div class="quant-card">
        <div class="metric-label">服务状态</div>
        <div class="metric-value" :class="expired ? 'danger' : 'ok'">
          {{ expired ? "已到期" : status && !status.serviceUntilMs ? "未开通" : "正常" }}
        </div>
      </div>
      <div class="quant-card">
        <div class="metric-label">剩余时间</div>
        <div class="metric-value mono">{{ remainingText }}</div>
      </div>
      <div class="quant-card">
        <div class="metric-label">到期时间</div>
        <div class="metric-value mono">{{ untilText }}</div>
      </div>
      <div class="quant-card">
        <div class="metric-label">当前模式</div>
        <div class="metric-value">{{ profileText }}</div>
      </div>
    </div>

    <div class="quant-card info-card">
      <h3>账号信息</h3>
      <el-descriptions :column="1" border size="small">
        <el-descriptions-item label="手机号">{{ maskedPhone }}</el-descriptions-item>
        <el-descriptions-item label="绑定设备">
          <span class="mono">{{ shortDevice }}</span>
        </el-descriptions-item>
        <el-descriptions-item label="服务周期">一周 / 一月 / 半年 / 一年</el-descriptions-item>
      </el-descriptions>
      <p class="renew-tip">
        续费说明：联系管理员缴纳费用后，管理员在后台延长服务周期，本程序将在 5 分钟内自动恢复，也可点击右上角“刷新状态”立即生效。
      </p>
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref } from "vue";
import { ElMessage } from "element-plus";
import dayjs from "dayjs";
import { useLicenseStoreHook } from "@/store/modules/license";

defineOptions({ name: "Service" });

const license = useLicenseStoreHook();
const refreshing = ref(false);
let timer: ReturnType<typeof setInterval> | null = null;

const status = computed(() => license.status);
const expired = computed(() => license.expired);
const maskedPhone = computed(() => {
  const p = status.value?.phone || "";
  return p.length === 11 ? p.slice(0, 3) + "****" + p.slice(7) : p || "--";
});
const shortDevice = computed(() => {
  const d = status.value?.deviceId || "";
  return d ? d.slice(0, 8) + "…" + d.slice(-6) : "--";
});
const profileText = computed(() =>
  status.value?.profile === "B" ? "稳健模式" : "进攻模式"
);

const untilText = computed(() => {
  const ms = status.value?.serviceUntilMs;
  return ms ? dayjs(ms).format("YYYY-MM-DD HH:mm:ss") : "--";
});

const remainingText = computed(() => {
  const ms = status.value?.serviceUntilMs;
  if (!ms) return "--";
  const remainSec = status.value?.remainingSec ?? 0;
  if (remainSec <= 0) return "已到期";
  const d = Math.floor(remainSec / 86400);
  const h = Math.floor((remainSec % 86400) / 3600);
  const m = Math.floor((remainSec % 3600) / 60);
  if (d > 0) return `${d} 天 ${h} 小时`;
  if (h > 0) return `${h} 小时 ${m} 分`;
  return `${m} 分钟`;
});

async function handleRefresh() {
  if (refreshing.value) return;
  refreshing.value = true;
  try {
    await license.refresh();
    ElMessage.success("服务状态已刷新");
  } catch {
    // 提示已弹出
  } finally {
    refreshing.value = false;
  }
}

onMounted(() => {
  timer = setInterval(() => {
    license.refresh();
  }, 60000);
});

onUnmounted(() => {
  if (timer) {
    clearInterval(timer);
    timer = null;
  }
});
</script>

<style scoped>
.service-panel {
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
.alert-item {
  margin-bottom: 16px;
}
.status-grid {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(200px, 1fr));
  gap: 16px;
  margin-bottom: 16px;
}
.quant-card {
  background: var(--quant-card, #1d1f27);
  border: 1px solid var(--quant-border, #2c2f3a);
  border-radius: 8px;
  padding: 20px;
}
.metric-label {
  font-size: 12px;
  color: var(--quant-text-secondary, #8b8fa3);
  margin-bottom: 8px;
}
.metric-value {
  font-size: 22px;
  font-weight: 600;
  color: var(--quant-text, #e0e0e0);
}
.metric-value.ok {
  color: #22c55e;
}
.metric-value.danger {
  color: #ef4444;
}
.mono {
  font-family: Consolas, "Courier New", monospace;
}
.info-card h3 {
  margin: 0 0 16px;
  color: var(--quant-text, #e0e0e0);
  font-size: 15px;
}
.renew-tip {
  margin: 16px 0 0;
  padding: 12px;
  border-radius: 8px;
  background: rgba(240, 169, 59, 0.08);
  border: 1px solid rgba(240, 169, 59, 0.25);
  font-size: 13px;
  color: #e8b45a;
}
</style>
