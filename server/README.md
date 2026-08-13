# 超能战士 · 授权服务端

Go + MySQL 授权服务（客户管理、短信验证码、一机一号、服务周期到期控制），内嵌 Web 管理后台。

## 快速开始（本地开发）

```bash
cd server
go run ./cmd/server
```

默认配置（开发模式）：

- 数据层：内存（重启清空，仅开发/测试）
- 短信：模拟通道（固定验证码 `123456`，同时打印到服务端日志）
- 管理后台：`http://127.0.0.1:8081/`
- 初始管理员：`admin / Admin@123456`（首次登录强制改密）
- 客户端授权地址：`http://127.0.0.1:8081`（C 版客户端默认值）

## 生产部署（MySQL）

方式一：docker compose

```bash
cd server
cp .env.example .env   # 按需修改
docker compose up -d
```

方式二：直接运行

```bash
ENV=prod \
DB_DRIVER=mysql \
DB_DSN="user:pass@tcp(127.0.0.1:3306)/quant_server" \
JWT_SECRET="替换为随机长密钥" \
ADMIN_USERNAME=admin \
ADMIN_PASSWORD="初始管理员密码" \
SMS_MODE=mock \
./quant-server
```

生产环境必须配置：

| 变量 | 说明 |
|---|---|
| `ENV=prod` | 生产模式 |
| `DB_DRIVER=mysql` + `DB_DSN` | MySQL 连接串（首次启动自动建表） |
| `JWT_SECRET` | 随机密钥（`openssl rand -hex 32` 生成） |
| `ADMIN_USERNAME/ADMIN_PASSWORD` | 初始管理员（首次登录强制改密） |
| `SMS_MODE` | `mock`（模拟）或 `aliyun`（真实短信，需配置阿里云凭据/签名/模板） |
| `AUTO_TRIAL_PERIOD` | 客户首次验证码注册自动赠送的服务周期（默认 `3d`=三天试用；置空则关闭） |

数据库表结构：首次启动自动执行 `internal/store/schema.sql`；完整文档见
`D:\0001_ba-A - 03\docs\超能战士-数据库设计文档.md`。

## 主要接口

- `POST /api/v1/sms/send` 发送验证码（mock 模式返回 `devCode`）
- `POST /api/v1/auth/login` 验证码/密码登录（首次自动注册 + 绑定设备）
- `POST /api/v1/auth/password` 首次登录设置密码
- `GET /api/v1/license` 授权状态（到期时间/剩余状态）
- `POST /api/v1/auth/logout` 退出登录
- `POST /api/v1/admin/login` 管理员登录
- `GET/POST /api/v1/admin/customers` 客户列表/新增
- `POST /api/v1/admin/customers/{id}/grant` 开通/续费（一周/一月/半年/一年）
- `POST /api/v1/admin/customers/{id}/unbind-device` 解绑设备
- `POST /api/v1/admin/customers/{id}/disable|enable` 停用/启用
- `GET /api/v1/admin/sms-codes` 模拟验证码查看（仅 mock 模式）
- `GET /api/v1/admin/audit-logs` 审计日志

## 测试

```bash
go test ./...
```

覆盖：验证码发送/校验/限频、验证码与密码登录、首次绑定/换机拒绝/解绑重绑、
四种周期开通与叠加续费、到期时间计算、管理员鉴权与强制改密、审计日志、
密码哈希与 JWT 安全（篡改/过期拒绝）、管理后台页面。

## 真实短信接入（阿里云）

1. 在阿里云开通短信服务，申请签名与验证码模板；
2. 设置 `SMS_MODE=aliyun` 及 `ALIYUN_ACCESS_KEY_ID/ALIYUN_ACCESS_KEY_SECRET/ALIYUN_SIGN_NAME/ALIYUN_TEMPLATE_CODE`；
3. 在 `internal/sms/sms.go` 的 `Aliyun.Send` 中按阿里云 SDK 文档完成下发实现。
