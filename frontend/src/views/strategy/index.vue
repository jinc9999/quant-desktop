<script setup lang="ts">
/**
 * 策略配置面板
 * 提供策略参数配置、启动/停止控制、运行状态展示
 * 通过 Wails 绑定调用 Go 后端 QuantService
 */
import { ref, watch, onMounted, onUnmounted } from "vue";
import { showError, showSuccess } from "../../utils/error-handler";
import { callService } from "../../utils/service";
import { QuantService } from "../../../bindings/quant-desktop/internal/bindings";

defineOptions({ name: "Strategy" });

/**
 * 策略参数（界面值）
 * 注意：stopLossPct / trailingActivation / trailingCallback / takeProfitPct 在界面上以百分比显示，
 * Go 后端存储的是比例（0.10 表示 10%），加载时 ×100，保存时 ÷100
 * 默认值 = S01 纯追涨（无门控）锁定参数（2026-08-04）
 */
const strategyParams = ref({
  scanIntervalSec: 15,
  timeframe: "15m",
  minGainPct: 4.0,
  min24hGainPct: 4.0,
  minQuoteVolume: 100000,
  topN: 10,
  maxOpenPositions: 10,         // 最大同时持仓数（2026-08-04 5→10）
  leverage: 10,
  positionMarginUsdt: 10.0,
  cooldownMin: 60,
  marginMode: "ISOLATED",
  stopLossPct: 6,
  trailingActivation: 3,
  trailingCallback: 2,
  takeProfitPct: 0,            // 固定止盈（%）：价格达到入场价*(1+该比例) 先止盈，0=关闭（纯跟踪，2026-08-04 用户否决 10% 封顶）
  maxHoldMin: 120,             // 最长持仓（分钟）：超时按当前价平仓，0=关闭
  dailyLossLimitPct: 5.0,
  maxDrawdownPct: 15.0,
  enableShort: false,          // S01 纯追涨：只做多，不做空
  enableAddOn: true,           // 追加仓位：移动止盈激活（现价>=首仓入场价*1.03）+ 再次命中信号 → 追加 1 张独立新单
  confirmWindowMin: 2,         // 放量确认窗口（分钟）：最近 N 分钟成交量 vs 之前窗口
  confirmThreshold: 0,         // 短窗口涨幅确认阈值（%），kline 模式下不生效
  volumeSurgeThreshold: 1.5,   // 成交量放大倍数阈值（0=关闭，1.5=放量 1.5 倍才追）
  signalMode: "kline",         // 信号模式：kline=15m K线实体实时检测 / sliding=滑动窗口（旧版）
  maxPullbackPct: 9.0          // 山顶过滤器（%）：距 24h 最高/最低回撤超此值不追，0=关闭
});

// 运行状态
const isRunning = ref(false);
const statusText = ref("已停止");
// 已运行 Tick 数
const tickCount = ref(0);
// 启动/停止操作进行中标记（防止重复点击）
const operating = ref(false);
// 状态轮询定时器
let statusTimer: ReturnType<typeof setInterval> | null = null;

// 运行模式与密钥
const runMode = ref("SIMULATION");
const apiKey = ref("");
const apiSecret = ref("");
// 保存模式操作进行中标记
const savingMode = ref(false);

// 代理配置
const proxyAddress = ref("");
const proxyPort = ref(0);
const savingProxy = ref(false);

/**
 * 将界面配置值转换为后端 StrategyConfig 对象
 * 三个比例字段（stopLossPct / trailingActivation / trailingCallback）界面值 ÷100
 * @returns 后端可直接存储的配置对象
 */
function toBackendConfig() {
  const p = strategyParams.value;
  return {
    scanIntervalSec: p.scanIntervalSec,
    timeframe: p.timeframe,
    minGainPct: p.minGainPct,
    min24hGainPct: p.min24hGainPct,
    minQuoteVolume: p.minQuoteVolume,
    topN: p.topN,
    maxOpenPositions: p.maxOpenPositions,
    leverage: p.leverage,
    positionMarginUsdt: p.positionMarginUsdt,
    cooldownMin: p.cooldownMin,
    marginMode: p.marginMode,
    stopLossPct: p.stopLossPct / 100,
    trailingActivation: p.trailingActivation / 100,
    trailingCallback: p.trailingCallback / 100,
    takeProfitPct: p.takeProfitPct / 100,
    maxHoldMin: p.maxHoldMin,
    dailyLossLimitPct: p.dailyLossLimitPct,
    maxDrawdownPct: p.maxDrawdownPct,
    enableShort: p.enableShort,
    enableAddOn: p.enableAddOn,
    confirmWindowMin: p.confirmWindowMin,
    confirmThreshold: p.confirmThreshold,
    volumeSurgeThreshold: p.volumeSurgeThreshold,
    signalMode: p.signalMode,
    maxPullbackPct: p.maxPullbackPct
  };
}

/**
 * 从后端加载策略配置填充表单
 * 三个比例字段后端值 ×100 转为百分比显示
 */
async function loadConfig() {
  const cfg = await callService(() => QuantService.GetConfig(), { silent: true });
  if (cfg) {
    strategyParams.value = {
      scanIntervalSec: cfg.scanIntervalSec,
      timeframe: cfg.timeframe,
      minGainPct: cfg.minGainPct,
      min24hGainPct: cfg.min24hGainPct ?? 5.0,
      minQuoteVolume: cfg.minQuoteVolume,
      topN: cfg.topN ?? 8,
      maxOpenPositions: cfg.maxOpenPositions,
      leverage: cfg.leverage,
      positionMarginUsdt: cfg.positionMarginUsdt,
      cooldownMin: cfg.cooldownMin ?? 60,
      marginMode: cfg.marginMode ?? "ISOLATED",
      stopLossPct: cfg.stopLossPct * 100,
      trailingActivation: cfg.trailingActivation * 100,
      trailingCallback: cfg.trailingCallback * 100,
      takeProfitPct: (cfg.takeProfitPct ?? 0) * 100,
      maxHoldMin: cfg.maxHoldMin ?? 120,
      dailyLossLimitPct: cfg.dailyLossLimitPct ?? 5.0,
      maxDrawdownPct: cfg.maxDrawdownPct ?? 15.0,
      enableShort: cfg.enableShort ?? false,
      enableAddOn: cfg.enableAddOn ?? true,
      confirmWindowMin: cfg.confirmWindowMin ?? 2,
      confirmThreshold: cfg.confirmThreshold ?? 0,
      volumeSurgeThreshold: cfg.volumeSurgeThreshold ?? 1.5,
      signalMode: cfg.signalMode ?? "kline",
      maxPullbackPct: cfg.maxPullbackPct ?? 9.0
    };
  }
}

/**
 * 拉取策略运行状态，更新 running 标记与 Tick 计数
 */
async function fetchStatus() {
  const status = await callService(() => QuantService.GetStrategyStatus(), { silent: true });
  if (status) {
    isRunning.value = !!status.running;
    statusText.value = status.running ? "运行中" : "已停止";
    tickCount.value = status.tickCount || 0;
  }
}

/**
 * 从后端加载当前运行模式
 */
async function loadMode() {
  const mode = await callService(() => QuantService.GetMode(), { silent: true });
  if (mode) runMode.value = mode;
}

/**
 * 加载指定模式已保存的凭据，自动填充到输入框
 * @param mode 运行模式
 */
async function loadSavedCredentials(mode: string) {
  const creds = await callService(() => QuantService.GetSavedCredentials(mode), { silent: true });
  if (creds) {
    apiKey.value = creds.apiKey || "";
    apiSecret.value = creds.apiSecret || "";
  }
}

/**
 * 加载已保存的代理配置
 */
async function loadProxyConfig() {
  const cfg = await callService(() => QuantService.GetProxyConfig(), { silent: true });
  if (cfg) {
    proxyAddress.value = cfg.address || "";
    proxyPort.value = cfg.port || 0;
  }
}

/**
 * 保存代理配置到后端
 */
async function saveProxyConfig() {
  if (savingProxy.value) return;
  savingProxy.value = true;
  try {
    const msg = await callService(
      () => QuantService.SetProxyConfig(proxyAddress.value, proxyPort.value),
      { context: "保存代理配置" }
    );
    if (msg !== null) showSuccess(msg || "代理配置已保存");
  } finally {
    savingProxy.value = false;
  }
}

/**
 * 监听模式切换，自动加载该模式已保存的凭据
 */
watch(runMode, (newMode) => {
  loadSavedCredentials(newMode);
});

/**
 * 保存运行模式与 API 密钥到后端
 * 策略运行中时后端会拒绝切换，由后端返回提示
 */
async function saveCredentials() {
  if (savingMode.value) return;
  savingMode.value = true;
  try {
    const msg = await callService(
      () => QuantService.SetCredentials(runMode.value, apiKey.value, apiSecret.value),
      { context: "保存模式" }
    );
    if (msg !== null) {
      showSuccess(msg || "模式已保存");
      await loadMode();
    }
  } finally {
    savingMode.value = false;
  }
}

/** 启动策略：先保存配置，再调用启动接口 */
async function handleStart() {
  if (operating.value) return;
  operating.value = true;
  try {
    const saveMsg = await callService(
      () => QuantService.SetConfig(toBackendConfig()),
      { context: "保存配置" }
    );
    if (saveMsg === null) return;
    const msg = await callService(
      () => QuantService.StartStrategy(),
      { context: "启动策略" }
    );
    if (msg !== null) {
      showSuccess(msg || "策略已启动");
      await fetchStatus();
    }
  } finally {
    operating.value = false;
  }
}

/** 停止策略 */
async function handleStop() {
  if (operating.value) return;
  operating.value = true;
  try {
    const msg = await callService(
      () => QuantService.StopStrategy(),
      { context: "停止策略" }
    );
    if (msg !== null) {
      showSuccess(msg || "策略已停止");
      await fetchStatus();
    }
  } finally {
    operating.value = false;
  }
}

/** 应用到项目：把当前表单配置持久化到数据库（重启后自动加载，运行中也可保存，下次启动生效） */
async function handlePersist() {
  if (operating.value) return;
  operating.value = true;
  try {
    const msg = await callService(
      () => QuantService.PersistConfig(toBackendConfig()),
      { context: "应用到项目" }
    );
    if (msg !== null) showSuccess(msg || "配置已应用到项目");
  } finally {
    operating.value = false;
  }
}

onMounted(async () => {
  await loadConfig();
  await loadMode();
  await loadSavedCredentials(runMode.value);
  await loadProxyConfig();
  await fetchStatus();
  // 每 3 秒轮询一次运行状态
  statusTimer = setInterval(fetchStatus, 3000);
});

onUnmounted(() => {
  // 清除轮询定时器，避免内存泄漏
  if (statusTimer) {
    clearInterval(statusTimer);
    statusTimer = null;
  }
});
</script>

<template>
  <div class="strategy-panel">
    <div class="panel-header">
      <h2>策略配置</h2>
      <div class="status-badge" :class="isRunning ? 'running' : 'stopped'">
        {{ statusText }}
      </div>
      <span class="tick-count">已运行 Tick：{{ tickCount }}</span>
    </div>

    <!-- 运行模式卡片（全宽） -->
    <div class="quant-card mode-card">
      <h3>运行模式</h3>
      <el-form label-width="120px" size="default" inline>
        <el-form-item label="模式">
          <el-select v-model="runMode" :disabled="isRunning" style="width: 160px">
            <el-option label="模拟盘" value="SIMULATION" />
            <el-option label="实盘" value="LIVE" />
          </el-select>
        </el-form-item>
        <el-form-item label="API Key">
          <el-input
            v-model="apiKey"
            :disabled="isRunning"
            placeholder="请输入 API Key"
            style="width: 220px"
            clearable
          />
        </el-form-item>
        <el-form-item label="API Secret">
          <el-input
            v-model="apiSecret"
            type="password"
            show-password
            :disabled="isRunning"
            placeholder="请输入 API Secret"
            style="width: 220px"
          />
        </el-form-item>
        <el-form-item>
          <el-button
            type="primary"
            :loading="savingMode"
            :disabled="isRunning"
            @click="saveCredentials"
          >
            保存模式
          </el-button>
        </el-form-item>
      </el-form>
      <p class="mode-tip">
        密钥加密保存在本地数据库，切换模式时自动填充。切换模式需先停止策略。
      </p>
    </div>

    <!-- 代理配置卡片（全宽） -->
    <div class="quant-card mode-card">
      <h3>代理配置</h3>
      <el-form label-width="120px" size="default" inline>
        <el-form-item label="代理地址">
          <el-input
            v-model="proxyAddress"
            :disabled="isRunning"
            placeholder="如 127.0.0.1（留空自动检测）"
            style="width: 220px"
            clearable
          />
        </el-form-item>
        <el-form-item label="代理端口">
          <el-input-number
            v-model="proxyPort"
            :disabled="isRunning"
            :min="0"
            :max="65535"
            placeholder="如 7890"
            style="width: 140px"
          />
        </el-form-item>
        <el-form-item>
          <el-button
            type="primary"
            :loading="savingProxy"
            :disabled="isRunning"
            @click="saveProxyConfig"
          >
            保存代理
          </el-button>
        </el-form-item>
      </el-form>
      <p class="mode-tip">
        留空则自动检测本地代理（Clash/V2Ray 等）。修改代理需先停止策略。
      </p>
    </div>

    <div class="card-grid">
      <!-- 扫描参数卡片 -->
      <div class="quant-card">
        <h3>扫描参数</h3>
        <el-form label-width="120px" size="default">
          <el-form-item label="扫描间隔(秒)">
            <el-input-number v-model="strategyParams.scanIntervalSec" :min="1" :max="300" />
          </el-form-item>
          <el-form-item label="滑动窗口">
            <el-select v-model="strategyParams.timeframe">
              <el-option label="1m" value="1m" />
              <el-option label="5m" value="5m" />
              <el-option label="15m" value="15m" />
            </el-select>
          </el-form-item>
          <el-form-item label="最小涨幅(%)">
            <el-tooltip content="15 分钟窗口涨幅门槛（%）" placement="top">
              <el-input-number v-model="strategyParams.minGainPct" :min="0.1" :max="50" :step="0.5" />
            </el-tooltip>
          </el-form-item>
          <el-form-item label="24h最小涨幅(%)">
            <el-tooltip content="24 小时涨幅门槛（%），与 15m 涨幅同时满足才入选（双条件筛选）" placement="top">
              <el-input-number v-model="strategyParams.min24hGainPct" :min="0.1" :max="50" :step="0.5" />
            </el-tooltip>
          </el-form-item>
          <el-form-item label="信号模式">
            <el-tooltip content="kline=当前 15m K 线相对开盘价实时检测（真上涨确认，推荐）；sliding=滑动窗口过程涨幅（旧版，插针也算涨，易假信号）" placement="top">
              <el-select v-model="strategyParams.signalMode" style="width: 200px">
                <el-option label="K线实体确认 (kline)" value="kline" />
                <el-option label="滑动窗口 (sliding)" value="sliding" />
              </el-select>
            </el-tooltip>
          </el-form-item>
          <el-form-item label="山顶过滤器(%)">
            <el-tooltip content="当前价距 24h 最高/最低价回撤超过该值不追（防止买在山顶接飞刀），0=关闭" placement="top">
              <el-input-number v-model="strategyParams.maxPullbackPct" :min="0" :max="50" :step="0.5" />
            </el-tooltip>
          </el-form-item>
          <el-form-item label="最小成交额">
            <el-input-number v-model="strategyParams.minQuoteVolume" :min="10000" :step="10000" />
          </el-form-item>
          <el-form-item label="候选数量(Top N)">
            <el-tooltip content="0 = 不限制，所有达标币种均可开仓（受最大持仓数约束）" placement="top">
              <el-input-number v-model="strategyParams.topN" :min="0" :max="20" />
            </el-tooltip>
          </el-form-item>
          <el-form-item label="启用做空">
            <el-switch v-model="strategyParams.enableShort" />
          </el-form-item>
          <el-form-item label="追加仓位">
            <el-tooltip content="持仓币移动止盈已激活（现价>=首仓入场价*(1+激活比例)）且再次命中信号时，追加 1 张独立新单（独立止损/跟踪/超时），单币最多 2 仓" placement="top">
              <el-switch v-model="strategyParams.enableAddOn" />
            </el-tooltip>
          </el-form-item>
          <el-form-item label="确认窗口(分钟)">
            <el-tooltip content="短窗口确认动量仍在，0=关闭" placement="top">
              <el-input-number v-model="strategyParams.confirmWindowMin" :min="0" :max="10" :step="0.5" />
            </el-tooltip>
          </el-form-item>
          <el-form-item label="确认阈值(%)">
            <el-tooltip content="短窗口内最小涨跌幅，0=关闭二次确认" placement="top">
              <el-input-number v-model="strategyParams.confirmThreshold" :min="0" :max="10" :step="0.5" />
            </el-tooltip>
          </el-form-item>
          <el-form-item label="放量倍数">
            <el-tooltip content="最近成交量需达到之前的N倍，0=关闭" placement="top">
              <el-input-number v-model="strategyParams.volumeSurgeThreshold" :min="0" :max="5" :step="0.1" />
            </el-tooltip>
          </el-form-item>
        </el-form>
      </div>

      <!-- 持仓参数卡片 -->
      <div class="quant-card">
        <h3>持仓参数</h3>
        <el-form label-width="120px" size="default">
          <el-form-item label="最大持仓数">
            <el-input-number v-model="strategyParams.maxOpenPositions" :min="1" :max="200" />
          </el-form-item>
          <el-form-item label="杠杆倍数">
            <el-input-number v-model="strategyParams.leverage" :min="1" :max="125" />
          </el-form-item>
          <el-form-item label="保证金(USDT)">
            <el-input-number v-model="strategyParams.positionMarginUsdt" :min="1" :max="1000" :step="1" />
          </el-form-item>
          <el-form-item label="冷却期(分钟)">
            <el-input-number v-model="strategyParams.cooldownMin" :min="1" :max="1440" />
          </el-form-item>
          <el-form-item label="保证金模式">
            <el-select v-model="strategyParams.marginMode" style="width: 160px">
              <el-option label="逐仓 (ISOLATED)" value="ISOLATED" />
              <el-option label="全仓 (CROSS)" value="CROSSED" />
            </el-select>
          </el-form-item>
        </el-form>
      </div>

      <!-- 风控参数卡片 -->
      <div class="quant-card">
        <h3>风控参数</h3>
        <el-form label-width="140px" size="default">
          <el-form-item label="初始止损(%)">
            <el-input-number v-model="strategyParams.stopLossPct" :min="1" :max="50" />
          </el-form-item>
          <el-form-item label="固定止盈(%)">
            <el-tooltip content="价格达到入场价*(1+该比例) 先固定止盈，与跟踪止盈先到先平，0=关闭" placement="top">
              <el-input-number v-model="strategyParams.takeProfitPct" :min="0" :max="50" />
            </el-tooltip>
          </el-form-item>
          <el-form-item label="最长持仓(分钟)">
            <el-tooltip content="持仓超过该时长后按当前价市价平仓，防止仓位长期滞留，0=关闭" placement="top">
              <el-input-number v-model="strategyParams.maxHoldMin" :min="0" :max="1440" :step="5" />
            </el-tooltip>
          </el-form-item>
          <el-form-item label="跟踪激活(%)">
            <el-input-number v-model="strategyParams.trailingActivation" :min="1" :max="50" />
          </el-form-item>
          <el-form-item label="跟踪回撤(%)">
            <el-input-number v-model="strategyParams.trailingCallback" :min="1" :max="20" />
          </el-form-item>
          <el-form-item label="日损限制(%)">
            <el-tooltip content="当日累计亏损达到此比例后熔断，停止开新仓（已生效）" placement="top">
              <el-input-number v-model="strategyParams.dailyLossLimitPct" :min="0.5" :max="20" :step="0.5" />
            </el-tooltip>
          </el-form-item>
          <el-form-item label="最大回撤(%)">
            <el-tooltip content="账户从启动时权益回撤达到此比例后全面熔断，停止开新仓（已生效）" placement="top">
              <el-input-number v-model="strategyParams.maxDrawdownPct" :min="1" :max="50" :step="1" />
            </el-tooltip>
          </el-form-item>
        </el-form>
      </div>

      <!-- 控制卡片 -->
      <div class="quant-card control-card">
        <h3>策略控制</h3>
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
          <el-tooltip content="把当前表单配置持久化到项目数据库，重启应用后自动加载；不影响正在运行的策略" placement="top">
            <el-button
              type="warning"
              size="large"
              :loading="operating"
              @click="handlePersist"
            >
              应用到项目
            </el-button>
          </el-tooltip>
        </div>
      </div>
    </div>
  </div>
</template>

<style scoped>
.strategy-panel {
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
.tick-count {
  font-size: 13px;
  color: var(--quant-text-secondary, #8b8fa3);
}
.card-grid {
  display: grid;
  grid-template-columns: repeat(2, 1fr);
  gap: 16px;
}
.mode-card {
  margin-bottom: 16px;
}
.mode-tip {
  margin: 8px 0 0;
  font-size: 12px;
  color: var(--quant-text-secondary, #8b8fa3);
}
.quant-card {
  background: var(--quant-card, #1d1f27);
  border: 1px solid var(--quant-border, #2c2f3a);
  border-radius: 8px;
  padding: 20px;
}
.quant-card h3 {
  margin: 0 0 16px;
  color: var(--quant-text, #e0e0e0);
  font-size: 15px;
}
.control-card {
  display: flex;
  flex-direction: column;
  justify-content: center;
}
.control-buttons {
  display: flex;
  gap: 16px;
  justify-content: center;
}
</style>
