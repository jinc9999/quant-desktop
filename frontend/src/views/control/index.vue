<template>
  <div class="control-panel">
    <div class="panel-header">
      <h2>策略控制</h2>
      <span class="status-badge" :class="statusClass">{{ statusText }}</span>
      <span v-if="isRunning" class="tick-count">已运行 {{ tickCount }} Tick</span>
    </div>

    <div class="quant-card">
      <h3>策略模式</h3>
      <p class="mode-tip">两套策略参数均已由运营方锁定，无法查看或修改</p>
      <el-radio-group
        v-model="profile"
        size="large"
        :disabled="isRunning"
        @change="handleProfileChange"
      >
        <el-radio-button value="A">进攻模式</el-radio-button>
        <el-radio-button value="B">稳健模式</el-radio-button>
      </el-radio-group>
      <div v-if="isRunning" class="lock-tip">
        策略运行中，请先停止再切换模式
      </div>
    </div>

    <div class="quant-card control-card">
      <h3>启动 / 停止</h3>
      <div class="control-buttons">
        <el-button
          type="success"
          size="large"
          :loading="operating"
          :disabled="isRunning"
          @click="handleStart"
        >
          启动策略
        </el-button>
        <el-button
          type="danger"
          size="large"
          :loading="operating"
          :disabled="!isRunning"
          @click="handleStop"
        >
          停止策略
        </el-button>
      </div>
      <p v-if="expired" class="expired-tip">服务已到期，无法启动策略，请前往“服务状态”页查看续费指引</p>
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref, watch } from "vue";
import { ElMessage } from "element-plus";
import { showSuccess } from "../../utils/error-handler";
import { callService } from "../../utils/service";
import { QuantService } from "../../../bindings/quant-desktop/internal/bindings";
import { useLicenseStoreHook } from "@/store/modules/license";

defineOptions({ name: "Control" });

const license = useLicenseStoreHook();
const profile = ref("A");
const isRunning = ref(false);
const warming = ref(false);
const statusText = ref("已停止");
const tickCount = ref(0);
const operating = ref(false);
let statusTimer: ReturnType<typeof setInterval> | null = null;

const expired = computed(() => license.expired);
const statusClass = computed(() => {
  if (warming.value) return "warming";
  return isRunning.value ? "running" : "stopped";
});

async function fetchStatus() {
  const status = await callService(() => QuantService.GetStrategyStatus(), {
    silent: true
  });
  if (!status) return;
  isRunning.value = !!status.running;
  const warmup = status.warmupRemainingSec || 0;
  if (status.running && warmup > 0) {
    warming.value = true;
    statusText.value = `预热中（约 ${Math.ceil(warmup / 60)} 分钟）`;
  } else {
    warming.value = false;
    statusText.value = status.running ? "运行中" : "已停止";
  }
  tickCount.value = status.tickCount || 0;
}

async function loadProfile() {
  const p = await callService(() => QuantService.GetActiveProfile(), {
    silent: true
  });
  if (p) profile.value = p;
}

async function handleProfileChange(value: string) {
  if (isRunning.value) {
    ElMessage.warning("策略运行中，请先停止再切换模式");
    await loadProfile();
    return;
  }
  const msg = await callService(
    () => QuantService.SetActiveProfile(value),
    { context: "切换模式" }
  );
  if (msg !== null) {
    showSuccess(msg || "模式已切换");
  } else {
    await loadProfile();
  }
}

async function handleStart() {
  if (operating.value) return;
  operating.value = true;
  try {
    if (expired.value) {
      ElMessage.error("服务已到期，无法启动策略");
      return;
    }
    const msg = await callService(() => QuantService.StartStrategy(), {
      context: "启动策略"
    });
    if (msg !== null) {
      showSuccess(msg || "策略已启动");
      await fetchStatus();
    }
  } finally {
    operating.value = false;
  }
}

async function handleStop() {
  if (operating.value) return;
  operating.value = true;
  try {
    const msg = await callService(() => QuantService.StopStrategy(), {
      context: "停止策略"
    });
    if (msg !== null) {
      showSuccess(msg || "策略已停止");
      await fetchStatus();
    }
  } finally {
    operating.value = false;
  }
}

watch(() => license.status?.profile, p => {
  if (p) profile.value = p;
});

onMounted(async () => {
  await loadProfile();
  await fetchStatus();
  statusTimer = setInterval(fetchStatus, 3000);
});

onUnmounted(() => {
  if (statusTimer) {
    clearInterval(statusTimer);
    statusTimer = null;
  }
});
</script>

<style scoped>
.control-panel {
  padding: 20px;
}
.panel-header {
  display: flex;
  align-items: center;
  gap: 12px;
  margin-bottom: 20px;
}
.panel-header h2 {
  margin: 0;
  color: var(--quant-text, #e0e0e0);
  font-size: 20px;
}
.status-badge {
  padding: 4px 12px;
  border-radius: 12px;
  font-size: 12px;
  font-weight: 500;
}
.status-badge.running {
  background: rgba(34, 197, 94, 0.15);
  color: #22c55e;
}
.status-badge.stopped {
  background: rgba(239, 68, 68, 0.15);
  color: #ef4444;
}
.status-badge.warming {
  background: rgba(240, 169, 59, 0.15);
  color: #f0a93b;
  animation: warming-pulse 1.6s ease-in-out infinite;
}
@keyframes warming-pulse {
  0%,
  100% {
    opacity: 1;
  }
  50% {
    opacity: 0.55;
  }
}
.tick-count {
  font-size: 13px;
  color: var(--quant-text-secondary, #8b8fa3);
}
.quant-card {
  background: var(--quant-card, #1d1f27);
  border: 1px solid var(--quant-border, #2c2f3a);
  border-radius: 8px;
  padding: 20px;
  margin-bottom: 16px;
}
.quant-card h3 {
  margin: 0 0 12px;
  color: var(--quant-text, #e0e0e0);
  font-size: 15px;
}
.mode-tip {
  margin: 0 0 12px;
  font-size: 12px;
  color: var(--quant-text-secondary, #8b8fa3);
}
.lock-tip {
  margin-top: 12px;
  font-size: 12px;
  color: #f0a93b;
}
.control-card {
  display: flex;
  flex-direction: column;
}
.control-buttons {
  display: flex;
  gap: 16px;
  justify-content: center;
}
.expired-tip {
  margin: 16px 0 0;
  text-align: center;
  font-size: 13px;
  color: #ef4444;
}
</style>
