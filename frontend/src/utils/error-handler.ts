/**
 * 统一错误处理工具
 * 职责：错误分类、用户友好提示、ElNotification 弹窗、去重防刷屏
 */
import { ElNotification } from "element-plus";

/** 错误级别 */
type ErrorLevel = "error" | "warning" | "success" | "info";

/** 错误分类信息 */
interface ErrorInfo {
  code: string;
  level: ErrorLevel;
  title: string;
  description: string;
  suggestion: string;
}

/** 错误码 → 用户友好提示映射规则（按优先级排列，先匹配先命中） */
const ERROR_RULES: Array<{
  pattern: RegExp;
  code: string;
  level: ErrorLevel;
  title: string;
  description: string;
  suggestion: string;
}> = [
  // 网络连接类
  {
    pattern: /deadline exceeded|EOF|connection reset|network|socket hang up|ECONNREFUSED/i,
    code: "NET_TIMEOUT",
    level: "error",
    title: "网络连接超时",
    description: "无法连接到币安服务器",
    suggestion: "请检查网络和代理设置，稍后重试"
  },
  // 认证类
  {
    pattern: /-2015|Invalid API-key|invalid api key/i,
    code: "AUTH_INVALID",
    level: "error",
    title: "API 认证失败",
    description: "API Key 无效或已过期",
    suggestion: "请在策略配置页重新填写 API Key"
  },
  {
    pattern: /-2014|API-key format|api key format/i,
    code: "AUTH_FORMAT",
    level: "error",
    title: "API Key 格式错误",
    description: "API Key 格式不正确",
    suggestion: "请检查 API Key 是否完整复制"
  },
  {
    pattern: /-1022|Signature|signature.*invalid/i,
    code: "AUTH_SIGN",
    level: "error",
    title: "签名验证失败",
    description: "API Secret 不正确",
    suggestion: "请检查 API Secret 是否完整复制"
  },
  // 交易操作类
  {
    pattern: /-2027|Exceeded.*position|exceeded.*maximum/i,
    code: "TRADE_LIMIT",
    level: "error",
    title: "仓位已达上限",
    description: "该币种在当前杠杆下已无法加仓",
    suggestion: "等待现有仓位平仓后自动重试"
  },
  {
    pattern: /-2019|Margin is insufficient|margin.*insufficient/i,
    code: "TRADE_MARGIN",
    level: "error",
    title: "保证金不足",
    description: "账户可用余额不足以开仓",
    suggestion: "请减少开仓数量或增加资金"
  },
  {
    pattern: /-4061|position.*mode|hedge/i,
    code: "TRADE_MODE",
    level: "error",
    title: "持仓模式错误",
    description: "需要双向持仓模式",
    suggestion: "系统正在自动修复，请稍后重试"
  },
  {
    pattern: /-4046|No need to change/i,
    code: "SKIP",
    level: "info",
    title: "",
    description: "",
    suggestion: ""
  },
  // 交易通用失败
  {
    pattern: /开仓失败|平仓失败|挂单失败|下单失败|order.*fail/i,
    code: "TRADE_FAIL",
    level: "error",
    title: "交易操作失败",
    description: "",
    suggestion: "请查看日志了解详情"
  },
  // 数据获取类
  {
    pattern: /获取.*失败|查询.*失败|fetch.*fail|query.*fail/i,
    code: "DATA_FETCH",
    level: "warning",
    title: "数据获取失败",
    description: "",
    suggestion: "页面将在下次刷新时重试"
  }
];

/** 去重：同一错误码在间隔内不重复弹窗 */
const recentErrors = new Map<string, number>();
const DEDUP_INTERVAL = 5000;

/**
 * 判断是否应该弹窗（去重）
 * @param code 错误码
 * @returns true 表示应该弹窗
 */
function shouldShow(code: string): boolean {
  const last = recentErrors.get(code);
  const now = Date.now();
  if (last && now - last < DEDUP_INTERVAL) return false;
  recentErrors.set(code, now);
  // 清理过期记录，防止内存泄漏
  if (recentErrors.size > 50) {
    for (const [k, v] of recentErrors) {
      if (now - v > DEDUP_INTERVAL * 2) recentErrors.delete(k);
    }
  }
  return true;
}

/**
 * 从 unknown 类型的错误中提取消息文本
 * @param error 任意错误对象
 * @returns 错误消息字符串
 */
function extractMessage(error: unknown): string {
  if (error instanceof Error) return error.message;
  if (typeof error === "string") return error;
  return String(error);
}

/**
 * 根据错误消息分类错误
 * @param msg 错误消息文本
 * @returns 错误分类信息
 */
function classifyError(msg: string): ErrorInfo {
  for (const rule of ERROR_RULES) {
    if (rule.pattern.test(msg)) {
      return {
        code: rule.code,
        level: rule.level,
        title: rule.title,
        description: rule.description || msg,
        suggestion: rule.suggestion
      };
    }
  }
  // 兜底：未知错误
  return {
    code: "SYS_UNKNOWN",
    level: "error",
    title: "系统异常",
    description: msg,
    suggestion: "请查看日志或重启应用"
  };
}

/**
 * 显示错误通知弹窗
 * 自动分类错误、格式化为用户友好提示、去重防刷屏
 * @param error 错误对象（Error / string / unknown）
 * @param context 操作上下文（如"启动策略"、"保存配置"），用于弹窗标题
 */
export function showError(error: unknown, context?: string): void {
  const msg = extractMessage(error);
  const info = classifyError(msg);

  // -4046 等无需提示的错误，静默跳过
  if (info.code === "SKIP") return;

  if (!shouldShow(info.code)) return;

  const title = context ? `${context}失败` : info.title;
  const description = info.description !== msg
    ? `${info.description}\n原因：${msg}`
    : info.description;
  const message = info.suggestion
    ? `${description}\n建议：${info.suggestion}`
    : description;

  ElNotification({
    title,
    message,
    type: info.level,
    duration: info.level === "error" ? 0 : 5000,
    position: "top-right",
    dangerouslyUseHTMLString: false
  });
}

/**
 * 显示成功通知
 * @param message 成功消息
 * @param title 标题（默认"操作成功"）
 */
export function showSuccess(message: string, title = "操作成功"): void {
  ElNotification({
    title,
    message,
    type: "success",
    duration: 3000,
    position: "top-right"
  });
}

/**
 * 显示警告通知
 * @param message 警告消息
 * @param title 标题（默认"注意"）
 */
export function showWarning(message: string, title = "注意"): void {
  ElNotification({
    title,
    message,
    type: "warning",
    duration: 5000,
    position: "top-right"
  });
}

/**
 * 显示信息通知
 * @param message 信息内容
 * @param title 标题（默认"提示"）
 */
export function showInfo(message: string, title = "提示"): void {
  ElNotification({
    title,
    message,
    type: "info",
    duration: 3000,
    position: "top-right"
  });
}
