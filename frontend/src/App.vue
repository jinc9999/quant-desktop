<template>
  <el-config-provider :locale="currentLocale">
    <router-view />
    <ReDialog />
  </el-config-provider>
</template>

<script lang="ts">
import { defineComponent } from "vue";
import { ElConfigProvider } from "element-plus";
import { ReDialog } from "@/components/ReDialog";
import zhCn from "element-plus/es/locale/lang/zh-cn";
import { Events } from "@wailsio/runtime";
import { showError } from "@/utils/error-handler";

export default defineComponent({
  name: "app",
  components: {
    [ElConfigProvider.name]: ElConfigProvider,
    ReDialog
  },
  computed: {
    currentLocale() {
      return zhCn;
    }
  },
  mounted() {
    // 监听后端推送的错误事件，弹出前端通知
    // 注意：回调参数是 WailsEvent 对象，实际数据在 event.data 中（CustomEvent.Data）
    Events.On("backend-error", (event: { data?: { context?: string; message?: string } }) => {
      const d = event.data || {};
      const ctx = d.context || "后台";
      const msg = d.message || "未知错误";
      showError(msg, ctx);
    });
  }
});
</script>
