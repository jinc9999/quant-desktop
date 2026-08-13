<template>
  <div class="account-panel">
    <div class="panel-header">
      <h2>账户设置</h2>
      <el-button type="danger" plain @click="handleLogout">退出登录</el-button>
    </div>

    <div class="quant-card">
      <h3>币安账户</h3>
      <p class="mode-tip">API 密钥仅保存在本机（加密存储），请确保币安账户已开启合约交易并配置 IP 白名单</p>
      <el-form label-width="130px" size="default" inline>
        <el-form-item label="运行模式">
          <el-select v-model="runMode" :disabled="isRunning" style="width: 160px">
            <el-option label="模拟盘" value="SIMULATION" />
            <el-option label="实盘" value="LIVE" />
          </el-select>
        </el-form-item>
        <el-form-item label="API Key">
          <el-input v-model="apiKey" placeholder="粘贴 API Key" style="width: 260px" show-password />
        </el-form-item>
        <el-form-item label="API Secret">
          <el-input
            v-model="apiSecret"
            placeholder="粘贴 API Secret"
            style="width: 260px"
            show-password
          />
        </el-form-item>
        <el-form-item>
          <el-button type="primary" :loading="savingMode" @click="saveCredentials">
            保存
          </el-button>
          <el-button :loading="testingConnection" @click="testConnection">
            测试连接
          </el-button>
        </el-form-item>
      </el-form>
    </div>

    <div class="quant-card">
      <h3>网络代理</h3>
      <el-form label-width="130px" size="default" inline>
        <el-form-item label="代理地址">
          <el-input v-model="proxyAddress" placeholder="如 127.0.0.1" style="width: 220px" />
        </el-form-item>
        <el-form-item label="代理端口">
          <el-input-number v-model="proxyPort" :min="0" :max="65535" />
        </el-form-item>
        <el-form-item>
          <el-button :loading="savingProxy" @click="saveProxyConfig">保存代理</el-button>
        </el-form-item>
      </el-form>
    </div>
  </div>
</template>

<script setup lang="ts">
import { onMounted, ref, watch } from "vue";
import { ElNotification } from "element-plus";
import { showError, showSuccess } from "../../utils/error-handler";
import { callService } from "../../utils/service";
import { QuantService } from "../../../bindings/quant-desktop/internal/bindings";
import { useLicenseStoreHook } from "@/store/modules/license";
import { useRouter } from "vue-router";

defineOptions({ name: "Account" });

const router = useRouter();
const license = useLicenseStoreHook();

const runMode = ref("SIMULATION");
const apiKey = ref("");
const apiSecret = ref("");
const savingMode = ref(false);
const testingConnection = ref(false);
const proxyAddress = ref("");
const proxyPort = ref(0);
const savingProxy = ref(false);
const isRunning = ref(false);

async function loadMode() {
  const mode = await callService(() => QuantService.GetMode(), { silent: true });
  if (mode) runMode.value = mode;
}

async function loadSavedCredentials(mode: string) {
  const creds = await callService(
    () => QuantService.GetSavedCredentials(mode),
    { silent: true }
  );
  if (creds) {
    apiKey.value = creds.apiKey || "";
    apiSecret.value = creds.apiSecret || "";
  }
}

async function loadProxyConfig() {
  const cfg = await callService(() => QuantService.GetProxyConfig(), { silent: true });
  if (cfg) {
    proxyAddress.value = cfg.address || "";
    proxyPort.value = cfg.port || 0;
  }
}

async function loadStatus() {
  const status = await callService(() => QuantService.GetStrategyStatus(), {
    silent: true
  });
  isRunning.value = !!status?.running;
}

watch(runMode, newMode => {
  loadSavedCredentials(newMode);
});

async function saveCredentials() {
  if (savingMode.value) return;
  savingMode.value = true;
  try {
    const msg = await callService(
      () =>
        QuantService.SetCredentials(
          runMode.value,
          apiKey.value.trim(),
          apiSecret.value.trim()
        ),
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

async function testConnection() {
  if (testingConnection.value) return;
  testingConnection.value = true;
  try {
    await callService(
      () =>
        QuantService.SetCredentials(
          runMode.value,
          apiKey.value.trim(),
          apiSecret.value.trim()
        ),
      { silent: true }
    );
    const result = await callService(() => QuantService.TestConnection(), {
      silent: true
    });
    if (!result) return;
    if (result.ok === "true") {
      showSuccess(result.message || "连接测试通过");
      return;
    }
    const lines = [result.message || "未知错误"];
    if (result.domain) lines.push(`请求域名：${result.domain}`);
    if (result.proxy) lines.push(`代理链路：${result.proxy}`);
    if (result.network) lines.push(`网络自检：${result.network}`);
    if (result.exit_ip) lines.push(`出口 IP：${result.exit_ip}（请与币安 IP 白名单核对）`);
    ElNotification({
      title: "连接测试失败",
      message: lines.join("<br/>"),
      type: "error",
      duration: 0,
      position: "top-right",
      dangerouslyUseHTMLString: true
    });
  } finally {
    testingConnection.value = false;
  }
}

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

async function handleLogout() {
  try {
    const msg = await license.logout();
    showSuccess(msg || "已退出登录");
    router.replace("/login");
  } catch (e) {
    showError(e, "退出登录");
  }
}

onMounted(async () => {
  await loadMode();
  await loadSavedCredentials(runMode.value);
  await loadProxyConfig();
  await loadStatus();
});
</script>

<style scoped>
.account-panel {
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
</style>
