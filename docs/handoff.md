# 交接文档：Samryetha-Ops 内核与部署编排器

> 交接时间：2026-09-30 深夜（上一轮 AI 完成 OIDC / 崩溃修复 / 内核自更新后）。
> 本文件描述的是**我实际操作过并验证过的**环境。请先看 §0：你的环境可能没有 shell，
> 那样你只能读代码、给方案，**不要假装能验证**。

---

## 0. 先做这三件事

1. **确认工具**：有 `shell` / `read` / `write` / `edit` 吗？有的话能不能 SSH 到
   `ubuntu@samryetha.com`（免密 sudo 已配好）？没有 shell 就只能出方案。
2. **仓库**：`Samryetha-Ops`（**运维层**，不是论坛主仓库）。
   ```
   git clone https://github.com/Samryetha-Development/Samryetha-Ops.git
   ```
3. **读**：本文件 → `docs/architecture.md`（设计契约）→ `docs/migration-from-update-sh.md`
   （迁移记录与已知待办）。**注意：文档里有历史性的过度承诺，§7 列出了哪些地方与代码不符。**

---

## 1. 这是什么

一个**部署/更新编排器**，Go 单静态二进制，零第三方依赖。隐喻是"把 Web 应用当操作系统"：
内核极小且稳定，功能都在"用户态服务"里，服务只能通过 syscall 调用内核。

```
kernel/          内核：boot/route/events/store/config/perm/proc/task/cron/auth/ui/syscall
  main.go        入口 + 服务装配（装配层，允许认识 pm2/systemd/deploy.yaml）
  wiring.go      组件构造 + 路由分发
  auth/          认证驱动：none / header / proxy / oidc   ← 本轮新增
services/
  deployer/      部署服务（UI 声明 ui.go / ui_full.go）
  statuspage/    状态页
  deploycfg/     deploy.yaml 的受限 YAML 解析器（自己实现）
  drivers/       git / release(未实现) / health(http,tcp,exec) / process(pm2,systemd,exec)
                 / migrations(command)   ← migrations 本轮新增
sdk/             插件侧客户端
etc/             deploy.yaml / auth.example.json / systemd/   ← systemd 单元本轮新增
kernel_self_update.sh          内核自更新   ← 本轮新增
ops_self_update.sh             旧控制台自更新（其组件已退役）
```

**三条不变量（脚本强制，`bash scripts/check-all.sh` 一次跑完）**
1. 内核不含领域词汇（`scripts/check-kernel-invariant.py`，装配层 `main.go`/`wiring.go` 豁免）
2. syscall 实现与权限表一致（`scripts/check-syscall-contract.py`）
3. 配置自洽（`cmd/cfgcheck`）+ `gofmt` / `go vet` / **`go test -race`**（本轮接入）

> ⚠️ **本轮修掉一个"假绿"**：`check-all.sh` 的 vet/build 门以前写成
> `go vet ./... | tee f | grep -q .`，配合 `set -o pipefail` 时 `grep -q` 提前退出会让
> `tee` 收到 SIGPIPE，管道被判失败 → **vet/build 报错时脚本反而输出"✓ 通过"**。
> 现已改为依据退出码。**修改任何检查脚本后，务必用一个"故意坏掉"的输入验证它真的会失败。**

---

## 2. 当前真实状态（我离开时，已验证）

### 2.1 线上

| 项 | 值 |
|---|---|
| 内核服务 | `samryetha-kernel.service`（systemd，`User=ubuntu`，`KERNEL_ROOT=/opt/Samryetha/kernel`，`KERNEL_LISTEN=127.0.0.1:3040`）|
| 内核版本 | **v1.4.1**（标记 `/opt/Samryetha/logs/.last-deployed-kernel`）|
| 控制台 | `https://status.samryetha.com/update`，由内核提供，OIDC 登录 |
| 认证 | `mode=oidc`，`/opt/Samryetha/kernel/etc/auth.json` 存在（`chmod 600`）|
| 授权 | 按 Lako 组：`samryetha-admins` → admin（准入与角色都靠它）|
| 内核自更新 | `samryetha-kernel-update.timer` **已启用**，每 15 分钟 |
| 论坛主站 | pm2：`samryetha-backend`(3001) / `samryetha-frontend`(3000)；dev 3010/3011；i18n 3002 |
| 旧控制台 | `samryetha-status.service` **disabled/inactive**（:3030 已退役）|
| crontab | 只剩注释；调度由内核内部 cron + systemd timer 负责 |
| 崩溃计数 | `NRestarts=0` |

### 2.2 内核 cron（`/api/kernel/cron`，需登录）

`deploy:main` `*/5`、`deploy:dev` `*/5`、`statuspage` `*/1`。
`deploy:ops` 已随目标停用而消失。

### 2.3 部署目标（`deploy.yaml`）

- `main`：git，workdir `/opt/Samryetha`，build 三步（uv sync / pnpm install / pnpm build），
  pm2 重启，health 3001+3000，marker `markers:.last-deployed`
- `dev`：workdir `/opt/Samryetha-dev`，before_deploy 跑 `scripts/dev-sync-data.sh`，
  health 3011，marker `markers:.last-deployed-dev`
- `ops`：**停用**（`enabled: false`）。它指向已退役的 `samryetha-status`+`:3030`。

---

## 3. 本轮（2026-09-30）做了什么

按 tag：`v1.1.0 → v1.2.0 → v1.2.1 → v1.3.0 → v1.4.0 → v1.4.1`

| commit | 内容 |
|---|---|
| `7b6d45d` | **修内核崩溃**：`deployer.Service.running` 是无锁 map，被 cron 起的多个 goroutine 并发写 → `fatal error: concurrent map writes` → 内核重启（restart counter 曾到 63）。加锁 + 原子 `claim/release`；`RollbackToPrevious` 也纳入互斥 |
| `4bd19b8` | **OIDC 认证**（`kernel/auth`）：授权码 + PKCE(S256) + userinfo；HMAC 签名会话 Cookie；header/proxy 模式非 loopback **拒绝启动**；认证闸门覆盖整条分发链 |
| `999be92` | 修测试里的 vet 问题；**修 `check-all.sh` 假绿** |
| `6ee3b76` | **按 Lako 组授权**：`Policy.AssignGroups`/`RoleFor`；`CallerInfo.Groups`；默认 scope 含 `groups` |
| `cea5f9c` | 删掉我引入的死配置 `admin_emails`；登录日志带 email |
| `d62cdfc` | **migrations 静默失效**：实现 `command` 驱动；配置了驱动却未注册 → **显式失败**；删掉指向不存在 npm/drizzle 的过时配置 |
| `7a549c2` | **`kernel_self_update.sh`**（含安装前预检）+ 文档 |
| `27ab731` | **ops 如实停用** + 目标级 `enabled` 真正生效 + 控制台 `runCapture` 取真实 stdout + 面板定时刷新 + "连接中"改真实状态 |
| `ddc4c91` | **systemd timer** 定时内核自更新 |
| `8664bc5` | **补 `panels` 插槽**（幽灵插槽）+ `plugin` 插槽清单改为别名 `ui` + `ui.declare` 对未知插槽告警 |
| `9fba807` | 停用目标不再进入状态页监控 |

---

## 4. 认证与授权（必读）

### 4.1 配置

`/opt/Samryetha/kernel/etc/auth.json`（**不在仓库里**，含密钥）：
```json
{
  "mode": "oidc",
  "issuer": "https://auth.samryetha.com",
  "client_id": "samryetha-status",
  "redirect_uri": "https://status.samryetha.com/update/callback",
  "scopes": ["openid", "email", "profile", "groups"],
  "allowed_groups": ["samryetha-admins"],
  "admin_groups": ["samryetha-admins"],
  "session_secret": "<openssl rand -base64 48>",
  "session_ttl_hours": 12
}
```
形状见 `etc/auth.example.json`。**内核启动即校验**：oidc 必须齐备且 `session_secret ≥ 32`
字符，否则**拒绝启动**。

### 4.2 加用户 = 在 Lako 里给角色

Lako 角色：`lako.admin`（服务账号 `admin` 持有）、`samryetha-users`、`samryetha-admins`。
**要让人进控制台，就在 Lako 给他 `samryetha-admins`。** 内核信任 IdP 的 `groups` claim，
不再需要改内核配置。判定顺序：**sub 点名 > 组（取最高）> 默认 viewer**。

### 4.3 关键设计（别破坏）

- **认证只回答"你是谁"，授权只回答"你能做什么"**；角色**只能**来自 `perm.Policy`，
  **绝不**采信请求头/请求体。
- `header`/`proxy` 模式仅在 loopback 监听下允许启动——这是一次真实事故的代价：
  迁移期内核退化成 `header` 却经 Caddy 暴露公网，任何人自带 `X-Kernel-Subject` 即自称 admin。
- 登录端点放在 `/update/auth/*`，回调用 `/update/callback`：**Caddy 只反代 `/update*`
  与 `/api/kernel*`**，这样零基础设施改动。`samryetha-status` 是**公共客户端**（无 secret），
  回调已在 Lako 登记，**不需要改 Caddy、不需要动 Lako 数据库**。

---

## 5. 怎么改、怎么发、怎么部署（正确流程）

```bash
# 1) 改代码（仓库）
# 2) 检查（必须全绿，注意它会真的失败）
bash scripts/check-all.sh
# 3) 提交：首行含 [minor] → minor；[major] → major；其它 → patch
git commit -m "... [minor]"
# 4) push main  → CI：两个 workflow（Kernel checks / Build and release binaries）
#    CI 会：gofmt+vet+测试 → 交叉编译 update-service + kernel → 自动打 tag → 发 Release
# 5) 部署
#    main/dev：内核 cron 自动（每 5 分钟）
#    内核自身：kernel_self_update.sh（已由 timer 每 15 分钟自动跑）
```

- **Release 附件**：`update-service`、`kernel`（+ 各自 `.sha256`）。
- **内核部署**：`/opt/Samryetha/kernel/scripts/kernel_self_update.sh [--status|--force]`
  （下载 → sha256 → **安装前预检** → 备份 → 原子替换 → 重启 → 健康门 → 回滚 → 标记）。
- **服务器没有 Go**，也没有内核源码；只有 Release 附件 + `kernel/scripts/`。

---

## 6. 排查配方（很有用）

### 6.1 服务日志去哪了

- **内核级**（启动、崩溃、systemd）→ `journalctl -u samryetha-kernel`
- **服务级**（部署步骤、statuspage）→ 内核**内存日志环** `/api/kernel/log`（需登录）。
  **重启会清空**，别拿它当历史记录。

### 6.2 不用密码验证"登录后的状态"（运维利器）

`auth.json` 里的 `session_secret` 可以自己签一个合法会话，从而在没有浏览器/密码的情况下
验证授权链路。**仅在你有服务器读权限时使用**：

```bash
COOKIE=$(python3 - <<'PY'
import json,hmac,hashlib,base64,time
s=json.load(open("/opt/Samryetha/kernel/etc/auth.json"))["session_secret"].encode()
p={"s":"a14dc53e-c7f3-48f0-94d5-4f3cf058ae78","g":["samryetha-admins"],"e":int(time.time())+600}
b=base64.urlsafe_b64encode(json.dumps(p,separators=(",",":")).encode()).rstrip(b"=")
m=base64.urlsafe_b64encode(hmac.new(s,b,hashlib.sha256).digest()).rstrip(b"=")
print((b+b"."+m).decode())
PY
)
curl -s -H "Cookie: samryetha_kernel_session=$COOKIE" http://127.0.0.1:3040/update | grep -oE '<h2>[^<]*'
curl -s -H "Cookie: samryetha_kernel_session=$COOKIE" http://127.0.0.1:3040/api/kernel/cron
```

### 6.3 部署到底跑没跑（不依赖日志）

```bash
ls -la --time-style=+%F_%T /opt/Samryetha/logs/.last-deployed /opt/Samryetha/logs/.last-deployed-dev
ls -la --time-style=+%F_%T /opt/Samryetha/.git/FETCH_HEAD     # cron 是否触发
git -C /opt/Samryetha rev-parse --short HEAD                  # 与 marker 比对
systemctl show samryetha-kernel -p NRestarts --value           # 必须是 0
```

### 6.4 端口

`3000` 论坛前端 · `3001` 论坛后端 · `3010/3011` dev · `3002` i18n · `3040` 内核（唯一控制台入口）
· `3030` 旧控制台（**已退役**）· `3020` Lako · `3099` 内核自更新预检用临时端口

---

## 7. 与历史文档/认知不符之处（避免被误导）

1. **`kernel/auth/auth.go` 曾被认为已存在**——实际从未存在，OIDC 是本轮从零实现的。
2. **"deploy:main 的 cron 从不触发"** 是误判：cron 一直在触发，真因是内核数据竞争崩溃重启，
   把内存态的 `last_run`/事件清空了。
3. **前端 3 个写入 bug（`9bd8b16`）早已部署**（是 dev HEAD 的祖先）。
4. **服务器 `deploy.yaml` 与仓库里的不是一份**：内核读 `/opt/Samryetha/kernel/etc/deploy.yaml`。
   改配置要改**服务器那份**（目前无自动同步，见 §8.2）。
5. **`deploy.yaml` 里的 `auth:` 段曾经没有任何代码读**（已删除）；认证配置在 `auth.json`。
6. `ops` 目标名义上"自举更新控制台"，实际其组件早已退役——**内核不能通过 deployer
   更新自己**（deployer 在内核进程内，自更新重启会腰斩部署，控制台会显示永不结束的部署）。

---

## 8. 已知缺口（未做，按建议优先级）

1. **`source: release` 来源驱动未实现**。`ops` 已停用规避；将来要有"从 Release 取产物"的
   目标时需先实现（`deployer.Plan` 也得补 `binary: {asset,dest}` 的解析）。
2. **内核自己的 `etc/` 没有自动部署通道**。本轮 `deploy.yaml`、`auth.json` 是手工同步的。
   建议把 `etc/` 纳入 `kernel_self_update.sh` 或写一个 `sync-etc` 步骤。
3. **"配置提醒"面板不会出现**：管道已修好，但没有任何东西再生成
   `status/data/config-notice.json`（旧 `update.sh` 的 `write_config_notice` 已退役，
   其检查条件也可能过时）。**要恢复，先决定"提醒什么"。**
4. **deployer 的每目标状态显示 `unknown`**：没有上报最近一次部署状态（概览卡片因此不精确）。
5. **面板采集里有硬编码 `/opt/Samryetha/...` 路径**（`ui_full.go`）。通用化时应改为配置驱动。
6. **通用性改造**（交接文档旧版第 5 节）：流水线仍是固定八步、装配层仍认识 pm2/systemd、
   没有第二个示例项目验证过"换项目只换配置"。**优先级最低，且做之前先跟人确认范围。**

---

## 9. 坑（都是真实发生过的，别重复）

| 坑 | 教训 |
|---|---|
| **配置写了但没有任何代码读**（`deploy.yaml` 的 `auth:`、`migrations`、目标级 `enabled`、`admin_emails`） | 这是本项目最常见的病。**加配置项时同时写读取代码，并让"配置了却不生效"变成失败或告警。** |
| `task.submit` 之后的 `st.Log` 是**步骤轨迹**（▶/✓），不是 stdout | 取命令输出必须用 `proc.spawn(capture=true)` + `proc.output`。`ui_full.go` 曾因此让所有面板消失 |
| 插槽声明成功却永不渲染 | `panels` 曾是"幽灵插槽"：模板渲染它、服务声明它，但**不在 `AllSlots` 里**。加插槽必须同步 `ui.AllSlots`；`plugin` 的清单已改为别名 `ui`，别再各写一份 |
| `if m, ok := reg.X(...); ok` 的静默跳过 | 配置与能力不符必须**显式失败**，否则代码与 schema 会悄悄漂移（migrations 就是这样） |
| `grep -q` + `pipefail` 让检查脚本假绿 | 检查要依据**退出码**；改完检查脚本要故意放坏输入验证它真的会失败 |
| 无锁 map + 多 goroutine | 内核 cron 对每个到期任务 `go j.fn()`；共享 map 必须加锁。**`go test -race` 已接入，别关掉** |
| **顺序错误：先上执行代码、后同步配置** | 我在 23:06–23:37 让 main 部署持续失败：先上了"让 migrations 真执行"，服务器配置却还写着 `npm run migrate`。**改行为前先把环境配置同步好。** |
| 直接改服务器文件 | 除非是 `etc/` 配置（目前无自动通道），否则一律走"改代码 → 推 dev/main → 让更新器部署" |

---

## 10. 给下一个 AI 的建议

1. **先确认工具与真实状态**，不要相信"编译通过/日志显示成功"。
2. 从 §8 的 2 → 3 → 4 开始；每改一处都跑 `check-all.sh` 并**端到端验证**。
3. 凡是"看起来配置了但可能没人读"的东西，先 `grep` 确认有没有读取方。
4. 改内核行为前，先确认**服务器上的配置**已经与代码期望一致（否则会出现我这轮的顺序事故）。
5. 通用性改造（§8.6）放最后，且先与人确认范围——**不要为了通用而通用**。
