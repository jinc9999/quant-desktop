/**
 * 授权状态 store（C 版“超能战士”专用）
 * 保存产品变体信息与授权状态，驱动登录页/到期锁定页的全局切换
 */
import { defineStore } from "pinia";
import { store } from "../utils";
import { QuantService } from "../../../bindings/quant-desktop/internal/bindings";
import { callService } from "../../utils/service";

export interface LicenseStatus {
  loggedIn: boolean;
  phone: string;
  deviceId: string;
  serviceUntilMs: number;
  remainingSec: number;
  expired: boolean;
  unopened: boolean;
  needsPassword: boolean;
  online: boolean;
  serverUnreachable: boolean;
  profile: string;
  message: string;
}

export interface LoginResult {
  token: string;
  phone: string;
  deviceId: string;
  needsPassword: boolean;
  serviceUntilMs: number;
  serverNowMs: number;
  message: string;
}

/** 给绑定调用加超时，避免某个调用异常挂起导致界面永远停在“加载中” */
function withTimeout<T>(promise: Promise<T>, ms: number, fallback: T): Promise<T> {
  return new Promise(resolve => {
    const timer = setTimeout(() => resolve(fallback), ms);
    promise
      .then(v => {
        clearTimeout(timer);
        resolve(v);
      })
      .catch(() => {
        clearTimeout(timer);
        resolve(fallback);
      });
  });
}

export const useLicenseStore = defineStore("license", {
  state: () => ({
    // 构建期即确定变体（C 版前端以 --mode c 构建），不依赖后端调用，登录门禁永不因调用挂起而卡死
    variant: (import.meta.env.VITE_PRODUCT_VARIANT as string) || "A",
    productName: "",
    ready: false,
    status: null as LicenseStatus | null,
    /** 到期锁定标记（后端到期回调推送后置为 true） */
    locked: false,
    /** 上次授权错误信息 */
    lastError: ""
  }),
  getters: {
    isC(): boolean {
      return this.variant === "C";
    },
    loggedIn(): boolean {
      return !!(this.status && this.status.loggedIn && !this.status.expired);
    },
    expired(): boolean {
      return !!(this.status && this.status.loggedIn && this.status.expired);
    },
    serverUnreachable(): boolean {
      return !!(this.status && this.status.serverUnreachable);
    }
  },
  actions: {
    /** 初始化：获取产品信息 + 授权状态 */
    async init() {
      try {
        const info = await withTimeout(
          callService(() => QuantService.GetProductInfo(), { silent: true }),
          5000,
          null
        );
        if (info) {
          this.variant = info.variant || this.variant;
          this.productName = info.productName || "";
        }
        if (this.isC) {
          const status = await withTimeout(
            callService(() => QuantService.GetLicenseStatus(), { silent: true }),
            5000,
            null
          );
          if (status) {
            this.status = status;
            this.locked = !!status.expired;
          }
        }
      } finally {
        this.ready = true;
      }
    },
    /** 刷新授权状态（手动刷新 / 事件触发） */
    async refresh() {
      if (!this.isC) return;
      const status = await callService(() => QuantService.LicenseRefresh(), {
        silent: true
      });
      if (status) {
        this.status = status;
        this.locked = !!status.expired;
      }
    },
    /** 发送验证码（返回提示信息） */
    async sendCode(phone: string): Promise<string> {
      const msg = await callService(
        () => QuantService.LicenseSendSmsCode(phone),
        { context: "获取验证码" }
      );
      if (msg === null) throw new Error("获取验证码失败");
      return msg;
    },
    /** 验证码登录/注册 */
    async loginWithCode(phone: string, code: string): Promise<LoginResult> {
      const result = await callService(
        () => QuantService.LicenseLoginWithCode(phone, code),
        { context: "登录" }
      );
      if (!result) throw new Error("登录失败");
      console.log("[login-result]", JSON.stringify(result));
      this.applyLogin(result);
      return result;
    },
    /** 密码登录 */
    async loginWithPassword(phone: string, password: string): Promise<LoginResult> {
      const result = await callService(
        () => QuantService.LicenseLoginWithPassword(phone, password),
        { context: "登录" }
      );
      if (!result) throw new Error("登录失败");
      this.applyLogin(result);
      return result;
    },
    applyLogin(result: LoginResult) {
      this.status = {
        loggedIn: true,
        phone: result.phone,
        deviceId: result.deviceId,
        serviceUntilMs: result.serviceUntilMs,
        remainingSec: 0,
        expired: false,
        unopened: false,
        needsPassword: !!result.needsPassword,
        online: true,
        serverUnreachable: false,
        profile: this.status?.profile || "A",
        message: result.message || ""
      };
      this.locked = false;
    },
    /** 首次登录设置密码 */
    async setPassword(password: string): Promise<string> {
      const msg = await callService(
        () => QuantService.LicenseSetPassword(password),
        { context: "设置密码" }
      );
      if (msg === null) throw new Error("设置密码失败");
      if (this.status) this.status.needsPassword = false;
      return msg;
    },
    /** 退出登录 */
    async logout(): Promise<string> {
      const msg = await callService(() => QuantService.LicenseLogout(), {
        context: "退出登录"
      });
      this.status = null;
      this.locked = false;
      return msg || "已退出登录";
    },
    /** 凭证失效：仅清空本地状态回到登录页（服务端已拒绝凭证，无需再请求） */
    sessionExpired() {
      this.status = null;
      this.locked = false;
    },
    /** 切换 A/B 模式 */
    async setProfile(profile: string): Promise<string> {
      const msg = await callService(
        () => QuantService.SetActiveProfile(profile),
        { context: "切换模式" }
      );
      if (msg !== null && this.status) this.status.profile = profile;
      return msg || "";
    }
  }
});

export function useLicenseStoreHook() {
  return useLicenseStore(store);
}
