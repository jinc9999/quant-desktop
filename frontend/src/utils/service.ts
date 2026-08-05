/**
 * Wails 服务调用包装层
 * 统一拦截 QuantService 调用的错误，自动弹窗提示，支持静默模式和重试
 */
import { showError } from "./error-handler";

/** 调用选项 */
interface CallOptions {
  /** 操作上下文（如"启动策略"），用于错误弹窗标题 */
  context?: string;
  /** 静默模式：失败不弹窗（轮询场景使用） */
  silent?: boolean;
  /** 重试次数（默认 0，不重试） */
  retryCount?: number;
  /** 重试间隔毫秒（默认 2000） */
  retryDelay?: number;
}

/**
 * 包装 QuantService 调用，统一错误处理
 * 成功返回数据，失败返回 null（已自动弹窗提示，除非 silent 模式）
 *
 * @param fn QuantService 方法调用（箭头函数）
 * @param options 调用选项
 * @returns 成功时返回 T，失败时返回 null
 *
 * @example
 * // 用户主动操作（弹窗提示）
 * const res = await callService(
 *   () => QuantService.StartStrategy(mode, key, secret),
 *   { context: "启动策略" }
 * );
 * if (res !== null) showSuccess("策略已启动");
 *
 * @example
 * // 轮询（静默，不弹窗）
 * const data = await callService(
 *   () => QuantService.GetDashboardData(),
 *   { silent: true }
 * );
 */
export async function callService<T>(
  fn: () => Promise<T>,
  options: CallOptions = {}
): Promise<T | null> {
  const { context, silent = false, retryCount = 0, retryDelay = 2000 } = options;

  let lastError: unknown = null;

  for (let attempt = 0; attempt <= retryCount; attempt++) {
    try {
      return await fn();
    } catch (e) {
      lastError = e;

      // 还有重试机会时等待后继续
      if (attempt < retryCount) {
        await new Promise(resolve => setTimeout(resolve, retryDelay));
        continue;
      }
    }
  }

  // 全部失败
  if (!silent) {
    showError(lastError, context);
  } else {
    console.warn(`[Service] ${context || "调用"}失败（静默）:`, lastError);
  }
  return null;
}
