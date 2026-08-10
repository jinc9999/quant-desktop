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
import { QuantService } from "../bindings/quant-desktop/internal/bindings";

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
    // 按策略名应用主题主色（A=琥珀金进攻 / B=科技蓝稳健），与策略版本标识联动
    this.applyStrategyTheme();
    // 监听后端推送的错误事件，弹出前端通知
    // 注意：回调参数是 WailsEvent 对象，实际数据在 event.data 中（CustomEvent.Data）
    Events.On("backend-error", (event: { data?: { context?: string; message?: string } }) => {
      const d = event.data || {};
      const ctx = d.context || "后台";
      const msg = d.message || "未知错误";
      showError(msg, ctx);
    });
  },
  methods: {
    async applyStrategyTheme() {
      try {
        const cfg = await QuantService.GetConfig();
        const name = cfg?.strategyName || "";
        const primary = name.includes("稳健B") ? "#3B82F6" : "#F0A93B";
        const root = document.documentElement;
        root.style.setProperty("--quant-primary", primary);
        root.style.setProperty("--el-color-primary", primary);
        // Element Plus 主色衍生档（半透明近似，保证按钮/输入框/链接同色系）
        root.style.setProperty("--el-color-primary-light-3", primary + "66");
        root.style.setProperty("--el-color-primary-light-5", primary + "59");
        root.style.setProperty("--el-color-primary-light-7", primary + "4d");
        root.style.setProperty("--el-color-primary-light-8", primary + "3d");
        root.style.setProperty("--el-color-primary-light-9", primary + "2e");
        root.style.setProperty("--el-color-primary-dark-2", primary);
      } catch {
        // 读取失败时保持默认琥珀金主题
      }
    }
  }
});
</script>
