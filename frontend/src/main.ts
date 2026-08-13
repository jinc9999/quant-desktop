import App from "./App.vue";
import router from "./router";
import { setupStore } from "@/store";
import { getPlatformConfig } from "./config";
import { MotionPlugin } from "@vueuse/motion";
// import { useEcharts } from "@/plugins/echarts";
import { createApp, type Directive } from "vue";
import { useElementPlus } from "@/plugins/elementPlus";
import { injectResponsiveStorage } from "@/utils/responsive";
import { initRouter } from "@/router/utils";
import { showError } from "@/utils/error-handler";
import { QuantService } from "../bindings/quant-desktop/internal/bindings";

import Table from "@pureadmin/table";
// import PureDescriptions from "@pureadmin/descriptions";

// 引入重置样式
import "./style/reset.scss";
// 导入公共样式
import "./style/index.scss";
// 一定要在main.ts中导入tailwind.css，防止vite每次hmr都会请求src/style/index.scss整体css文件导致热更新慢的问题
import "./style/tailwind.css";
import "element-plus/dist/index.css";
// 导入字体图标
import "./assets/iconfont/iconfont.js";
import "./assets/iconfont/iconfont.css";

// 强制启用深色主题
document.documentElement.classList.add("dark");

const app = createApp(App);

// 前端运行诊断：把模块加载与 JS 错误上报到客户端日志（client.log），便于定位界面问题
function reportClientEvent(message: string) {
  try {
    QuantService.LogClientEvent(message).catch(() => {});
  } catch {
    // 绑定不可用时静默忽略
  }
}
reportClientEvent("main.ts 模块加载完成");
window.addEventListener("error", e => {
  reportClientEvent("JS error: " + (e.message || "unknown"));
});
window.addEventListener("unhandledrejection", e => {
  reportClientEvent("JS unhandledrejection: " + String(e.reason));
});

// 全局错误兜底：捕获未处理的组件错误和 Promise rejection
app.config.errorHandler = (err, _instance, info) => {
  console.error(`[GlobalError] ${info}:`, err);
  reportClientEvent(`Vue error [${info}]: ${String(err)}`);
  showError(err, "应用异常");
};
window.addEventListener("unhandledrejection", e => {
  console.error("[GlobalError] unhandledrejection:", e.reason);
  reportClientEvent("GlobalError unhandledrejection: " + String(e.reason));
  showError(e.reason, "后台任务异常");
});

// 自定义指令
import * as directives from "@/directives";
Object.keys(directives).forEach(key => {
  app.directive(key, (directives as { [key: string]: Directive })[key]);
});

// 全局注册@iconify/vue图标库
import {
  IconifyIconOffline,
  IconifyIconOnline,
  FontIcon
} from "./components/ReIcon";
app.component("IconifyIconOffline", IconifyIconOffline);
app.component("IconifyIconOnline", IconifyIconOnline);
app.component("FontIcon", FontIcon);

// 全局注册按钮级别权限组件
import { Auth } from "@/components/ReAuth";
import { Perms } from "@/components/RePerms";
app.component("Auth", Auth);
app.component("Perms", Perms);

// 全局注册vue-tippy
import "tippy.js/dist/tippy.css";
import "tippy.js/themes/light.css";
import VueTippy from "vue-tippy";
app.use(VueTippy);

getPlatformConfig(app).then(async config => {
  setupStore(app);
  app.use(router);
  await router.isReady();
  // 桌面端：初始化静态菜单（无动态路由请求）
  await initRouter();
  injectResponsiveStorage(app, config);
  app.use(MotionPlugin).use(useElementPlus).use(Table);
  // .use(PureDescriptions)
  // .use(useEcharts);
  app.mount("#app");
});
