<template>
  <div class="expired-page">
    <div class="expired-card">
      <div class="icon">!</div>
      <h1>服务已到期</h1>
      <p>策略已自动停止，核心功能暂时锁定。</p>
      <p>请联系管理员续费，续费成功后本程序会自动恢复使用。</p>
      <div class="actions">
        <el-button type="primary" :loading="refreshing" @click="handleRefresh">
          我已续费，刷新状态
        </el-button>
        <el-button @click="handleLogout">退出登录</el-button>
      </div>
    </div>
  </div>
</template>

<script setup lang="ts">
import { ref } from "vue";
import { useRouter } from "vue-router";
import { ElMessage } from "element-plus";
import { useLicenseStoreHook } from "@/store/modules/license";

defineOptions({ name: "Expired" });

const router = useRouter();
const license = useLicenseStoreHook();
const refreshing = ref(false);

async function handleRefresh() {
  if (refreshing.value) return;
  refreshing.value = true;
  try {
    await license.refresh();
    if (license.expired) {
      ElMessage.warning("服务仍未续费，请稍后再试或联系管理员");
    } else {
      ElMessage.success("服务已恢复");
    }
  } catch {
    // 提示已弹出
  } finally {
    refreshing.value = false;
  }
}

async function handleLogout() {
  await license.logout();
  router.replace("/login");
}
</script>

<style scoped>
.expired-page {
  min-height: 100vh;
  display: flex;
  align-items: center;
  justify-content: center;
  background:
    radial-gradient(900px 500px at 50% 20%, rgba(239, 68, 68, 0.16), transparent),
    #14161b;
  padding: 24px;
}
.expired-card {
  width: 420px;
  max-width: 100%;
  background: #1d1f27;
  border: 1px solid rgba(239, 68, 68, 0.35);
  border-radius: 12px;
  padding: 40px 32px;
  text-align: center;
  box-shadow: 0 20px 60px rgba(0, 0, 0, 0.45);
}
.icon {
  width: 64px;
  height: 64px;
  line-height: 64px;
  margin: 0 auto 16px;
  border-radius: 50%;
  background: rgba(239, 68, 68, 0.15);
  color: #ef4444;
  font-size: 34px;
  font-weight: 800;
}
.expired-card h1 {
  margin: 0 0 12px;
  font-size: 24px;
  color: #ef4444;
}
.expired-card p {
  margin: 6px 0;
  font-size: 14px;
  color: #9ca3af;
}
.actions {
  display: flex;
  gap: 12px;
  justify-content: center;
  margin-top: 24px;
}
</style>
