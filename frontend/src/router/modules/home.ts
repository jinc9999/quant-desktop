const Layout = () => import("@/layout/index.vue");

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
  children: [
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
    }
  ]
} satisfies RouteConfigsTable;
