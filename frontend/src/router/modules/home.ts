const Layout = () => import("@/layout/index.vue");

/**
 * 产品变体：构建 C 版时通过 VITE_PRODUCT_VARIANT=C 注入
 * （build_clients.ps1 以 --mode c 构建前端，A/B 版不设置该变量）
 */
const VARIANT: string = import.meta.env.VITE_PRODUCT_VARIANT || "A";
const isC = VARIANT === "C";

const children = isC
  ? [
      {
        path: "/dashboard",
        name: "Dashboard",
        component: () => import("@/views/dashboard/index.vue"),
        meta: {
          title: "仪表盘",
          icon: "ep/odometer"
        }
      },
      {
        path: "/control",
        name: "Control",
        component: () => import("@/views/control/index.vue"),
        meta: {
          title: "策略控制",
          icon: "ep/video-play"
        }
      },
      {
        path: "/positions",
        name: "Positions",
        component: () => import("@/views/positions/index.vue"),
        meta: {
          title: "实时持仓",
          icon: "ep/document"
        }
      },
      {
        path: "/orders",
        name: "Orders",
        component: () => import("@/views/orders/index.vue"),
        meta: {
          title: "委托状态",
          icon: "ep/document"
        }
      },
      {
        path: "/trades",
        name: "Trades",
        component: () => import("@/views/trades/index.vue"),
        meta: {
          title: "已完成交易",
          icon: "ep/finished"
        }
      },
      {
        path: "/logs",
        name: "Logs",
        component: () => import("@/views/logs/index.vue"),
        meta: {
          title: "交易日志",
          icon: "ep/notebook"
        }
      },
      {
        path: "/summary",
        name: "Summary",
        component: () => import("@/views/summary/index.vue"),
        meta: {
          title: "每日总结",
          icon: "ep/notebook"
        }
      },
      {
        path: "/strategy-summary",
        name: "StrategySummary",
        component: () => import("@/views/strategy-summary/index.vue"),
        meta: {
          title: "每日策略总结",
          icon: "ep/data-analysis"
        }
      },
      {
        path: "/service",
        name: "Service",
        component: () => import("@/views/service/index.vue"),
        meta: {
          title: "服务状态",
          icon: "ep/medal"
        }
      },
      {
        path: "/account",
        name: "Account",
        component: () => import("@/views/account/index.vue"),
        meta: {
          title: "账户设置",
          icon: "ep/user"
        }
      }
    ]
  : [
      {
        path: "/dashboard",
        name: "Dashboard",
        component: () => import("@/views/dashboard/index.vue"),
        meta: {
          title: "仪表盘",
          icon: "ep/odometer"
        }
      },
      {
        path: "/strategy",
        name: "Strategy",
        component: () => import("@/views/strategy/index.vue"),
        meta: {
          title: "策略配置",
          icon: "ep/setting"
        }
      },
      {
        path: "/positions",
        name: "Positions",
        component: () => import("@/views/positions/index.vue"),
        meta: {
          title: "实时持仓",
          icon: "ep/document"
        }
      },
      {
        path: "/logs",
        name: "Logs",
        component: () => import("@/views/logs/index.vue"),
        meta: {
          title: "交易日志",
          icon: "ep/notebook"
        }
      },
      {
        path: "/orders",
        name: "Orders",
        component: () => import("@/views/orders/index.vue"),
        meta: {
          title: "委托状态",
          icon: "ep/document"
        }
      },
      {
        path: "/trades",
        name: "Trades",
        component: () => import("@/views/trades/index.vue"),
        meta: {
          title: "已完成交易",
          icon: "ep/finished"
        }
      },
      {
        path: "/summary",
        name: "Summary",
        component: () => import("@/views/summary/index.vue"),
        meta: {
          title: "每日总结",
          icon: "ep/notebook"
        }
      },
      {
        path: "/strategy-summary",
        name: "StrategySummary",
        component: () => import("@/views/strategy-summary/index.vue"),
        meta: {
          title: "每日策略总结",
          icon: "ep/data-analysis"
        }
      }
    ];

export default {
  path: "/",
  name: "Home",
  component: Layout,
  redirect: "/dashboard",
  meta: {
    icon: "ep/setting",
    title: "量化交易",
    rank: 0
  },
  children
} satisfies RouteConfigsTable;
