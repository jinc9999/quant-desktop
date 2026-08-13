/* 超能战士 · 客户管理系统（Vue3 + Element Plus 单页应用） */
(function () {
  const { createApp, ref, reactive, computed } = Vue;
  const { ElMessage, ElMessageBox } = ElementPlus;

  const api = axios.create({ baseURL: "/api/v1", timeout: 15000 });
  api.interceptors.request.use(function (cfg) {
    const token = localStorage.getItem("admin_token");
    if (token) cfg.headers.Authorization = "Bearer " + token;
    return cfg;
  });
  api.interceptors.response.use(
    function (res) {
      const d = res.data;
      if (d && d.code === 0) return d.data;
      const msg = (d && d.message) || "请求失败";
      const err = new Error(msg);
      err.status = res.status;
      throw err;
    },
    function (err) {
      const msg = (err.response && err.response.data && err.response.data.message) || err.message || "网络错误";
      const e = new Error(msg);
      e.status = err.response ? err.response.status : 0;
      throw e;
    }
  );

  const periods = [
    { value: "3d", label: "3天试用" },
    { value: "1w", label: "一周" },
    { value: "1m", label: "一月" },
    { value: "6m", label: "半年" },
    { value: "1y", label: "一年" }
  ];
  const periodLabel = function (p) {
    const hit = periods.find(function (x) { return x.value === p; });
    return hit ? hit.label : p;
  };
  const fmt = function (ms) {
    if (!ms) return "--";
    return dayjs(ms).format("YYYY-MM-DD HH:mm:ss");
  };

  const App = {
    setup() {
      const loggedIn = ref(!!localStorage.getItem("admin_token"));
      const username = ref(localStorage.getItem("admin_username") || "");
      const mustChange = ref(false);

      // 登录表单
      const loginForm = reactive({ username: "", password: "" });
      const loginLoading = ref(false);

      // 强制改密
      const changeVisible = ref(false);
      const changeForm = reactive({ oldPassword: "", newPassword: "" });
      const changeLoading = ref(false);

      // 客户管理
      const tab = ref("customers");
      const customers = ref([]);
      const customerTotal = ref(0);
      const customerPage = ref(1);
      const customerSize = ref(20);
      const searchPhone = ref("");
      const customerLoading = ref(false);

      const createVisible = ref(false);
      const createLoading = ref(false);
      const createForm = reactive({ phone: "", period: "" });

      const grantVisible = ref(false);
      const grantLoading = ref(false);
      const grantForm = reactive({ customerId: 0, phone: "", period: "1m" });

      // 短信记录
      const smsItems = ref([]);
      const smsLoading = ref(false);

      // 审计日志
      const auditItems = ref([]);
      const auditTotal = ref(0);
      const auditPage = ref(1);
      const auditSize = ref(20);
      const auditLoading = ref(false);

      async function doLogin() {
        if (!loginForm.username || !loginForm.password) {
          ElMessage.warning("请输入用户名和密码");
          return;
        }
        loginLoading.value = true;
        try {
          const data = await api.post("/admin/login", loginForm);
          localStorage.setItem("admin_token", data.token);
          localStorage.setItem("admin_username", data.username);
          username.value = data.username;
          loggedIn.value = true;
          if (data.mustChangePassword) {
            mustChange.value = true;
            changeVisible.value = true;
            changeForm.oldPassword = loginForm.password;
            ElMessage.warning("首次登录请修改初始密码");
          } else {
            ElMessage.success("登录成功");
            loadCustomers();
          }
        } catch (e) {
          ElMessage.error(e.message);
        } finally {
          loginLoading.value = false;
        }
      }

      async function doChangePassword() {
        if (changeForm.newPassword.length < 8) {
          ElMessage.warning("新密码至少 8 位，需包含字母和数字");
          return;
        }
        changeLoading.value = true;
        try {
          const data = await api.post("/admin/password", changeForm);
          ElMessage.success("密码修改成功");
          if (data.token) localStorage.setItem("admin_token", data.token);
          changeVisible.value = false;
          mustChange.value = false;
          loadCustomers();
        } catch (e) {
          ElMessage.error(e.message);
        } finally {
          changeLoading.value = false;
        }
      }

      function logout() {
        localStorage.removeItem("admin_token");
        localStorage.removeItem("admin_username");
        loggedIn.value = false;
      }

      async function loadCustomers() {
        customerLoading.value = true;
        try {
          const data = await api.get("/admin/customers", {
            params: { search: searchPhone.value, page: customerPage.value, pageSize: customerSize.value }
          });
          customers.value = data.items;
          customerTotal.value = data.total;
        } catch (e) {
          ElMessage.error(e.message);
        } finally {
          customerLoading.value = false;
        }
      }

      function openCreate() {
        createForm.phone = "";
        createForm.period = "";
        createVisible.value = true;
      }

      async function doCreate() {
        if (!/^1[3-9]\d{9}$/.test(createForm.phone)) {
          ElMessage.warning("手机号格式不正确");
          return;
        }
        createLoading.value = true;
        try {
          await api.post("/admin/customers", createForm);
          ElMessage.success("客户创建成功");
          createVisible.value = false;
          loadCustomers();
        } catch (e) {
          ElMessage.error(e.message);
        } finally {
          createLoading.value = false;
        }
      }

      function openGrant(row) {
        grantForm.customerId = row.id;
        grantForm.phone = row.phone;
        grantForm.period = "1m";
        grantVisible.value = true;
      }

      async function doGrant() {
        grantLoading.value = true;
        try {
          const data = await api.post("/admin/customers/" + grantForm.customerId + "/grant", { period: grantForm.period });
          ElMessage.success(data.message || "开通/续费成功");
          grantVisible.value = false;
          loadCustomers();
        } catch (e) {
          ElMessage.error(e.message);
        } finally {
          grantLoading.value = false;
        }
      }

      async function unbind(row) {
        try {
          await ElMessageBox.confirm("确认解绑客户 " + row.phone + " 的设备？解绑后该账号需重新登录绑定。", "解绑设备", { type: "warning" });
        } catch (e) { return; }
        try {
          const data = await api.post("/admin/customers/" + row.id + "/unbind-device");
          ElMessage.success(data.message || "已解绑");
          loadCustomers();
        } catch (e) {
          ElMessage.error(e.message);
        }
      }

      async function toggleStatus(row) {
        const action = row.status === 1 ? "disable" : "enable";
        const tip = row.status === 1 ? "确认停用该客户账号？" : "确认启用该客户账号？";
        try {
          await ElMessageBox.confirm(tip, "状态变更", { type: "warning" });
        } catch (e) { return; }
        try {
          const data = await api.post("/admin/customers/" + row.id + "/" + action);
          ElMessage.success(data.message || "操作成功");
          loadCustomers();
        } catch (e) {
          ElMessage.error(e.message);
        }
      }

      async function loadSMS() {
        smsLoading.value = true;
        try {
          const data = await api.get("/admin/sms-codes");
          smsItems.value = data.items || [];
        } catch (e) {
          ElMessage.error(e.message);
        } finally {
          smsLoading.value = false;
        }
      }

      async function loadAudit() {
        auditLoading.value = true;
        try {
          const data = await api.get("/admin/audit-logs", {
            params: { page: auditPage.value, pageSize: auditSize.value }
          });
          auditItems.value = data.items;
          auditTotal.value = data.total;
        } catch (e) {
          ElMessage.error(e.message);
        } finally {
          auditLoading.value = false;
        }
      }

      function onTabChange(name) {
        if (name === "customers") loadCustomers();
        if (name === "sms") loadSMS();
        if (name === "audit") loadAudit();
      }

      return {
        loggedIn, username, mustChange, loginForm, loginLoading,
        changeVisible, changeForm, changeLoading, doLogin, doChangePassword, logout,
        tab, onTabChange,
        customers, customerTotal, customerPage, customerSize, searchPhone, customerLoading,
        loadCustomers, openCreate, createVisible, createLoading, createForm, doCreate,
        grantVisible, grantLoading, grantForm, openGrant, doGrant, unbind, toggleStatus,
        smsItems, smsLoading, auditItems, auditTotal, auditPage, auditSize, auditLoading,
        loadAudit, periods, periodLabel, fmt
      };
    },
    template: `
      <div v-if="!loggedIn" class="login-wrap">
        <div class="login-card">
          <div class="login-title">
            <h1>超能战士 · 客户管理系统</h1>
            <p>请使用管理员账号登录</p>
          </div>
          <el-form label-position="top" @submit.prevent>
            <el-form-item label="用户名">
              <el-input v-model="loginForm.username" placeholder="管理员用户名" />
            </el-form-item>
            <el-form-item label="密码">
              <el-input v-model="loginForm.password" type="password" show-password placeholder="密码"
                @keyup.enter="doLogin" />
            </el-form-item>
            <el-button type="primary" style="width:100%" :loading="loginLoading" @click="doLogin">登 录</el-button>
          </el-form>
        </div>
      </div>

      <el-container v-else class="main-wrap">
        <el-header>
          <h1>超能战士 · 客户管理系统</h1>
          <div class="toolbar">
            <span>管理员：{{ username }}</span>
            <el-button size="small" @click="changeVisible = true">修改密码</el-button>
            <el-button size="small" type="danger" plain @click="logout">退出</el-button>
          </div>
        </el-header>
        <el-container>
          <el-aside width="180px">
            <el-menu :default-active="tab" @select="onTabChange" style="height:100%">
              <el-menu-item index="customers">客户管理</el-menu-item>
              <el-menu-item index="sms">短信记录</el-menu-item>
              <el-menu-item index="audit">审计日志</el-menu-item>
            </el-menu>
          </el-aside>
          <el-main>
            <el-alert v-if="mustChange" type="warning" :closable="false" show-icon
              title="首次登录请先修改初始密码，否则无法操作其他功能" style="margin-bottom:12px" />

            <!-- 客户管理 -->
            <div v-if="tab === 'customers'">
              <div style="display:flex;gap:8px;margin-bottom:12px">
                <el-input v-model="searchPhone" placeholder="按手机号搜索" clearable style="width:220px"
                  @keyup.enter="loadCustomers" />
                <el-button type="primary" @click="loadCustomers">查询</el-button>
                <el-button type="success" @click="openCreate">新增客户</el-button>
              </div>
              <el-table :data="customers" v-loading="customerLoading" border stripe>
                <el-table-column prop="id" label="ID" width="70" />
                <el-table-column prop="phone" label="手机号" width="130" />
                <el-table-column label="状态" width="90">
                  <template #default="{ row }">
                    <el-tag :type="row.status === 1 ? 'success' : 'danger'">{{ row.status === 1 ? '正常' : '停用' }}</el-tag>
                  </template>
                </el-table-column>
                <el-table-column label="服务到期" width="170">
                  <template #default="{ row }">{{ fmt(row.serviceUntilMs) }}</template>
                </el-table-column>
                <el-table-column label="绑定设备" min-width="180">
                  <template #default="{ row }">
                    <span class="muted">{{ row.deviceId ? row.deviceId.slice(0,8) + '…' : '未绑定' }}</span>
                  </template>
                </el-table-column>
                <el-table-column label="操作" width="300" fixed="right">
                  <template #default="{ row }">
                    <el-button size="small" type="primary" @click="openGrant(row)">开通/续费</el-button>
                    <el-button size="small" type="warning" :disabled="!row.deviceId" @click="unbind(row)">解绑</el-button>
                    <el-button size="small" :type="row.status === 1 ? 'danger' : 'success'" @click="toggleStatus(row)">
                      {{ row.status === 1 ? '停用' : '启用' }}
                    </el-button>
                  </template>
                </el-table-column>
              </el-table>
              <div class="page-bar">
                <el-pagination layout="total, prev, pager, next" :total="customerTotal"
                  :page-size="customerSize" v-model:current-page="customerPage" @current-change="loadCustomers" />
              </div>
            </div>

            <!-- 短信记录 -->
            <div v-if="tab === 'sms'">
              <el-alert type="info" :closable="false" show-icon title="模拟短信模式"
                description="此处展示模拟通道生成的验证码（仅开发/联调环境），生产接入真实短信后不再展示" style="margin-bottom:12px" />
              <el-table :data="smsItems" v-loading="smsLoading" border stripe>
                <el-table-column prop="id" label="ID" width="80" />
                <el-table-column prop="phone" label="手机号" width="150" />
                <el-table-column prop="code" label="验证码" width="120" />
                <el-table-column label="发送时间">
                  <template #default="{ row }">{{ fmt(row.createdAt) }}</template>
                </el-table-column>
              </el-table>
            </div>

            <!-- 审计日志 -->
            <div v-if="tab === 'audit'">
              <el-table :data="auditItems" v-loading="auditLoading" border stripe>
                <el-table-column prop="id" label="ID" width="80" />
                <el-table-column prop="adminId" label="管理员" width="90" />
                <el-table-column prop="action" label="动作" width="190" />
                <el-table-column prop="targetType" label="对象" width="100" />
                <el-table-column prop="targetId" label="对象ID" width="90" />
                <el-table-column prop="detail" label="详情" min-width="220" />
                <el-table-column prop="ip" label="IP" width="130" />
                <el-table-column label="时间" width="170">
                  <template #default="{ row }">{{ fmt(row.createdAt) }}</template>
                </el-table-column>
              </el-table>
              <div class="page-bar">
                <el-pagination layout="total, prev, pager, next" :total="auditTotal"
                  :page-size="auditSize" v-model:current-page="auditPage" @current-change="loadAudit" />
              </div>
            </div>
          </el-main>
        </el-container>

        <!-- 新增客户 -->
        <el-dialog v-model="createVisible" title="新增客户" width="420px">
          <el-form label-width="90px">
            <el-form-item label="手机号">
              <el-input v-model="createForm.phone" maxlength="11" placeholder="客户手机号" />
            </el-form-item>
            <el-form-item label="初始周期">
              <el-select v-model="createForm.period" placeholder="可不选，稍后开通" clearable style="width:100%">
                <el-option v-for="p in periods" :key="p.value" :label="p.label" :value="p.value" />
              </el-select>
            </el-form-item>
          </el-form>
          <template #footer>
            <el-button @click="createVisible = false">取消</el-button>
            <el-button type="primary" :loading="createLoading" @click="doCreate">创建</el-button>
          </template>
        </el-dialog>

        <!-- 开通/续费 -->
        <el-dialog v-model="grantVisible" :title="'开通/续费：' + grantForm.phone" width="420px">
          <el-form label-width="90px">
            <el-form-item label="服务周期">
              <el-select v-model="grantForm.period" style="width:100%">
                <el-option v-for="p in periods" :key="p.value" :label="p.label" :value="p.value" />
              </el-select>
            </el-form-item>
            <div class="muted">续费会在当前到期时间上自动叠加</div>
          </el-form>
          <template #footer>
            <el-button @click="grantVisible = false">取消</el-button>
            <el-button type="primary" :loading="grantLoading" @click="doGrant">确认开通</el-button>
          </template>
        </el-dialog>

        <!-- 修改密码 -->
        <el-dialog v-model="changeVisible" title="修改密码" width="420px">
          <el-form label-width="90px">
            <el-form-item label="原密码">
              <el-input v-model="changeForm.oldPassword" type="password" show-password />
            </el-form-item>
            <el-form-item label="新密码">
              <el-input v-model="changeForm.newPassword" type="password" show-password placeholder="8-20位，含字母和数字" />
            </el-form-item>
          </el-form>
          <template #footer>
            <el-button @click="changeVisible = false">取消</el-button>
            <el-button type="primary" :loading="changeLoading" @click="doChangePassword">保存</el-button>
          </template>
        </el-dialog>
      </el-container>
    `
  };

  const app = createApp(App);
  app.use(ElementPlus);
  app.mount("#app");
})();
