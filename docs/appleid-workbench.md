# Apple ID 独立工作台

入口：管理员侧边栏 **Apple ID 工作台**，路由 `/admin/appleid`。

## 当前功能

- 点击「初始化并获取验证码」后，后端初始化官方注册组件并请求真实图形验证码。
- 支持等待、错误重试、清空、取消请求、离开页面取消请求，以及原有中英文和明暗主题。
- 邮箱验证、手机号验证、最终创建 Apple ID **尚未接入**。获取图形验证码不等于注册成功。
- 「打开 Apple 官方注册页」使用独立浏览器会话，不继承工作台会话。

## 模块边界

前端 `frontend/src/views/admin/AppleIDView.vue`，API `frontend/src/api/admin/appleid.ts`。
后端 `backend/internal/appleid`；唯一接口 `POST /api/v1/admin/appleid/bootstrap`。
该接口挂在现有 admin 鉴权、全局限流、审计与合规中间件之后。
没有数据库迁移，不读写现有账号池，不改变供应商或 Hark 注册、登录流程。

每次请求创建独立 CookieJar；请求总时限 25 秒，TLS 验证保持开启，无自动重试。
仅允许 Apple 官方域名之间的 HTTPS 重定向。后端限制响应体和图片大小，拒绝非 JPEG/PNG 内容。
每个进程同时只运行一个检查，启动间隔至少 5 秒。
响应设置 `Cache-Control: no-store`，只返回图片和图片元信息；Apple 会话头、challenge ID 和 token 不进入前端、日志或持久存储。

## 验证

```sh
cd frontend
pnpm exec vitest run src/views/admin/__tests__/AppleIDView.spec.ts src/api/admin/__tests__/appleid.spec.ts src/router/__tests__/appleid-route.spec.ts src/components/layout/__tests__/AppSidebar.spec.ts
pnpm typecheck
pnpm build
cd ../backend
go test ./internal/appleid ./internal/server/routes
# 可选：只测试官方初始化和图形验证码，不提交账户注册。
APPLEID_LIVE_PROBE=1 go test ./internal/appleid -run TestLiveBootstrap -count=1 -v
```

若本机 pnpm 12 拒绝仓库现有 `pnpm.overrides`，使用 `npx --yes pnpm@10.34.9` 运行上述前端命令；不改动仓库锁文件。

## 后续开发提示词

```text
继续维护独立 Apple ID 工作台：仅修改 backend/internal/appleid 和 /admin/appleid 页面所属模块；
保留现有账号池与 Hark 业务边界。先增加测试再接入经实际验证的下一阶段协议。
不要将初始化、图片获取标记为账户注册成功；复用现有管理员鉴权与中英文主题。
```
