<template>
  <div class="login-page">
    <div class="login-card">
      <div class="brand">
        <div class="brand-logo">超</div>
        <h1>{{ productName || "超能战士" }}</h1>
        <p>币安合约量化策略客户端</p>
      </div>

      <!-- 首次登录：设置密码 -->
      <div v-if="step === 'password'" class="login-body">
        <h2>设置登录密码</h2>
        <p class="sub-tip">首次登录需要设置密码，之后可用密码或验证码登录</p>
        <el-form label-position="top" @submit.prevent>
          <el-form-item label="新密码">
            <el-input
              v-model="password"
              type="password"
              show-password
              placeholder="8-20 位，需包含字母和数字"
              maxlength="20"
              @input="scorePassword"
            />
          </el-form-item>
          <div v-if="password" class="strength">
            <div class="strength-bars">
              <span
                v-for="i in 4"
                :key="i"
                class="bar"
                :class="{ active: i <= strength }"
              />
            </div>
            <span class="strength-text">{{ strengthText }}</span>
          </div>
          <el-form-item label="确认密码">
            <el-input
              v-model="confirmPassword"
              type="password"
              show-password
              placeholder="再次输入密码"
              maxlength="20"
              @keyup.enter="handleSetPassword"
            />
          </el-form-item>
          <el-button
            type="primary"
            class="submit-btn"
            :loading="submitting"
            @click="handleSetPassword"
          >
            完成设置
          </el-button>
        </el-form>
      </div>

      <!-- 登录 -->
      <div v-else class="login-body">
        <el-tabs v-model="loginMode" class="login-tabs">
          <el-tab-pane label="验证码登录" name="code" />
          <el-tab-pane label="密码登录" name="password" />
        </el-tabs>

        <el-form label-position="top" @submit.prevent>
          <el-form-item label="手机号">
            <el-input
              v-model="phone"
              placeholder="请输入手机号"
              maxlength="11"
              clearable
            />
          </el-form-item>

          <el-form-item v-if="loginMode === 'code'" label="验证码">
            <div class="code-row">
              <el-input
                v-model="code"
                placeholder="6 位验证码"
                maxlength="6"
                @keyup.enter="handleCodeLogin"
              />
              <el-button
                :disabled="countdown > 0 || !validPhone"
                @click="handleSendCode"
              >
                {{ countdown > 0 ? `${countdown}s 后重发` : "获取验证码" }}
              </el-button>
            </div>
          </el-form-item>

          <el-form-item v-else label="密码">
            <el-input
              v-model="password"
              type="password"
              show-password
              placeholder="请输入密码"
              maxlength="20"
              @keyup.enter="handlePasswordLogin"
            />
          </el-form-item>

          <el-button
            type="primary"
            class="submit-btn"
            :loading="submitting"
            :disabled="loginMode === 'code' && (!code || !validPhone)"
            @click="loginMode === 'code' ? handleCodeLogin() : handlePasswordLogin()"
          >
            登 录
          </el-button>
        </el-form>

        <p class="foot-tip">首次使用请用手机验证码登录，并设置密码</p>
      </div>
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed, onUnmounted, ref } from "vue";
import { useRouter } from "vue-router";
import { ElMessage } from "element-plus";
import { useLicenseStoreHook } from "@/store/modules/license";

defineOptions({ name: "Login" });

const router = useRouter();
const license = useLicenseStoreHook();

const productName = computed(() => license.productName || "超能战士");
const loginMode = ref<"code" | "password">("code");
const phone = ref("");
const code = ref("");
const password = ref("");
const confirmPassword = ref("");
const step = ref<"login" | "password">("login");
const submitting = ref(false);
const countdown = ref(0);
let countdownTimer: ReturnType<typeof setInterval> | null = null;

const validPhone = computed(() => /^1[3-9]\d{9}$/.test(phone.value));

const strength = ref(0);
const strengthText = computed(() =>
  ["", "弱", "较弱", "中等", "强"][strength.value] || ""
);

function scorePassword() {
  const pwd = password.value;
  let score = 0;
  if (pwd.length >= 8) score++;
  if (/[a-zA-Z]/.test(pwd) && /\d/.test(pwd)) score++;
  if (pwd.length >= 12 || /[^a-zA-Z0-9]/.test(pwd)) score++;
  if (pwd.length >= 16) score++;
  strength.value = score;
}

function startCountdown() {
  countdown.value = 60;
  if (countdownTimer) clearInterval(countdownTimer);
  countdownTimer = setInterval(() => {
    countdown.value--;
    if (countdown.value <= 0 && countdownTimer) {
      clearInterval(countdownTimer);
      countdownTimer = null;
    }
  }, 1000);
}

async function handleSendCode() {
  if (!validPhone.value) {
    ElMessage.warning("请输入正确的手机号");
    return;
  }
  try {
    const msg = await license.sendCode(phone.value);
    ElMessage.success(msg || "验证码已发送");
    startCountdown();
  } catch {
    // 提示已在 callService 内弹出
  }
}

async function handleCodeLogin() {
  if (!validPhone.value) {
    ElMessage.warning("请输入正确的手机号");
    return;
  }
  if (!/^\d{6}$/.test(code.value)) {
    ElMessage.warning("请输入 6 位验证码");
    return;
  }
  submitting.value = true;
  try {
    const result = await license.loginWithCode(phone.value, code.value);
    ElMessage.success("登录成功");
    // 以服务端权威状态判断是否需要设置密码（避免绑定返回值差异导致跳过）
    await license.refresh();
    if (license.status?.needsPassword || result.needsPassword) {
      step.value = "password";
      password.value = "";
      confirmPassword.value = "";
      strength.value = 0;
    } else {
      goHome();
    }
  } catch {
    // 错误提示已弹出
  } finally {
    submitting.value = false;
  }
}

async function handlePasswordLogin() {
  if (!validPhone.value) {
    ElMessage.warning("请输入正确的手机号");
    return;
  }
  if (!password.value) {
    ElMessage.warning("请输入密码");
    return;
  }
  submitting.value = true;
  try {
    await license.loginWithPassword(phone.value, password.value);
    ElMessage.success("登录成功");
    goHome();
  } catch {
    // 错误提示已弹出
  } finally {
    submitting.value = false;
  }
}

async function handleSetPassword() {
  if (password.value.length < 8 || password.value.length > 20) {
    ElMessage.warning("密码长度需为 8-20 位");
    return;
  }
  if (!/[a-zA-Z]/.test(password.value) || !/\d/.test(password.value)) {
    ElMessage.warning("密码需同时包含字母和数字");
    return;
  }
  if (password.value !== confirmPassword.value) {
    ElMessage.warning("两次输入的密码不一致");
    return;
  }
  submitting.value = true;
  try {
    const msg = await license.setPassword(password.value);
    ElMessage.success(msg || "密码设置成功");
    goHome();
  } catch {
    // 错误提示已弹出
  } finally {
    submitting.value = false;
  }
}

function goHome() {
  if (router.currentRoute.value.path === "/login") {
    router.replace("/dashboard");
  }
}

onUnmounted(() => {
  if (countdownTimer) clearInterval(countdownTimer);
});
</script>

<style scoped>
.login-page {
  min-height: 100vh;
  display: flex;
  align-items: center;
  justify-content: center;
  background:
    radial-gradient(1200px 600px at 20% 10%, rgba(225, 29, 72, 0.18), transparent),
    radial-gradient(1000px 500px at 85% 90%, rgba(245, 158, 11, 0.12), transparent),
    #14161b;
  padding: 24px;
}
.login-card {
  width: 400px;
  max-width: 100%;
  background: #1d1f27;
  border: 1px solid #2c2f3a;
  border-radius: 12px;
  padding: 32px 28px;
  box-shadow: 0 20px 60px rgba(0, 0, 0, 0.45);
}
.brand {
  text-align: center;
  margin-bottom: 24px;
}
.brand-logo {
  width: 56px;
  height: 56px;
  line-height: 56px;
  margin: 0 auto 12px;
  border-radius: 14px;
  background: linear-gradient(135deg, #e11d48, #f97316);
  color: #fff;
  font-size: 26px;
  font-weight: 800;
}
.brand h1 {
  margin: 0 0 4px;
  font-size: 22px;
  color: #e5e7eb;
}
.brand p {
  margin: 0;
  font-size: 13px;
  color: #8b8fa3;
}
.login-body h2 {
  margin: 0 0 6px;
  font-size: 17px;
  color: #e5e7eb;
}
.sub-tip {
  margin: 0 0 16px;
  font-size: 12px;
  color: #8b8fa3;
}
.login-tabs {
  margin-bottom: 12px;
}
.code-row {
  display: flex;
  gap: 10px;
  width: 100%;
}
.code-row .el-input {
  flex: 1;
}
.submit-btn {
  width: 100%;
  margin-top: 8px;
}
.strength {
  margin: -4px 0 16px;
}
.strength-bars {
  display: flex;
  gap: 4px;
  margin-bottom: 4px;
}
.bar {
  flex: 1;
  height: 4px;
  border-radius: 2px;
  background: #2c2f3a;
}
.bar.active {
  background: linear-gradient(90deg, #e11d48, #f97316);
}
.strength-text {
  font-size: 12px;
  color: #8b8fa3;
}
.foot-tip {
  margin: 16px 0 0;
  text-align: center;
  font-size: 12px;
  color: #6b7280;
}
</style>
