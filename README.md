# Samryetha Ops

Samryetha 生产环境的**运维层**：自动更新器、公开状态页、以及 OAuth 保护的更新控制台。
与业务代码仓库 [`Samryetha-Development/Samryetha`](https://github.com/Samryetha-Development/Samryetha) 分离——
这里只放"怎么把代码跑起来并保证它一直健康"的部分。

> 线上路径：部署在 `/opt/Samryetha/`（生产）与 `/opt/Samryetha-dev/`（dev 镜像）。
> 本仓库是这些文件的**版本化管理源头**（此前它们只存在于服务器本地）。

## 组成

| 文件 | 作用 |
|---|---|
| `update.sh` | 自动更新器。cron 每 5 分钟跑一次：拉取上游 → 构建到临时目录 → 原子替换 → pm2 滚动重启 → 健康检查 → 失败自动回滚。main / dev 双环境，支持 `--status`、`--force`、`--rollback <sha>`。 |
| `generate.py` | 公开状态页生成器（`generate.sh` 每分钟调用）。6 语言、亮/暗主题、滞回故障检测引擎，输出静态页到 `www/`。 |
| `codecheck.mjs` | 代码体检：tsc 语义 + ESLint type-checked + 自定义 AST + Python 静态检查，带基线棘轮（只对**新增**问题告警）。 |
| `update-service/` | **更新控制台**。Go 单静态二进制（零第三方依赖），systemd 托管，OAuth（Lako OIDC）登录 + 白名单，WebSocket 实时推送，可远程执行更新/回滚/重启/备份等。 |
| `caddy-status.conf` | `status.samryetha.com` 的 Caddy 站点块：`/update*` 与 `/api/update*` 反代到控制台，其余走静态状态页。 |
| `analysis/` | 代码体检依赖的 ESLint/TypeScript 配置与 `package.json`（`node_modules` 不入库，`pnpm install` 生成）。 |

## 部署

### 状态页 + 代码体检

```bash
# 放到生产根目录
sudo mkdir -p /opt/Samryetha/status /opt/Samryetha/analysis
sudo cp generate.py generate.sh codecheck.mjs /opt/Samryetha/status/
sudo cp analysis/* /opt/Samryetha/analysis/          # eslint.config.mjs, package.json
cd /opt/Samryetha/analysis && pnpm install           # 生成 node_modules（不入库）
```

每分钟由 cron 调用 `status/generate.sh` 生成静态页；`update.sh` 成功后调用 `codecheck.mjs`。

### 更新控制台（Go 服务）

```bash
cd update-service
# 交叉编译（在任意机器上；服务器无需 Go 环境）
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o update-service .
# 部署
sudo install -m 0755 update-service /opt/Samryetha/status/update-service/bin/update-service
sudo cp admin.html main.go go.mod /opt/Samryetha/status/update-service/
sudo cp samryetha-status.service /etc/systemd/system/
sudo systemctl daemon-reload && sudo systemctl enable --now samryetha-status
```

也可用 `deploy-status-service.sh` 一键完成（systemd + Caddy 块 + 健康检查）。

### 必需的密钥与配置

控制台启动前需要（**不入库**）：

```bash
# 会话签名密钥（64 字节随机）
head -c 48 /dev/urandom | base64 | tr -d '\n' > /opt/Samryetha/status/.service-secret
chmod 600 /opt/Samryetha/status/.service-secret
```

`update-config.json`（由控制台写入，可初始化）：

```json
{
  "main":  { "enabled": true, "branch": "main" },
  "dev":   { "enabled": true, "branch": "dev" },
  "notify": { "webhook": "" }
}
```

`samryetha-status.service` 里通过环境变量注入：`SAMRYETHA_ROOT`、`LISTEN`、`OIDC_ISSUER`、
`OIDC_CLIENT_ID`、`OIDC_REDIRECT_URI`、`ADMIN_EMAIL`（逗号分隔白名单）。

### OAuth 客户端注册

控制台是一个 **public client + PKCE**，需在 Lako（IdP）中登记：

```
client_id:   samryetha-status
is_public:   true
auto_consent:true
redirect_uri:https://status.samryetha.com/update/callback
```

（Lako 用 Postgres；此条目的插入见提交历史中的 SQL。）

## 更新控制台能做什么

登录 `https://status.samryetha.com/update`（OAuth + 邮箱白名单）后：

- **概览**：版本/远端 SHA 差异、进程状态（内存、重启数）、磁盘、最近提交、配置提醒
- **参数**：main/dev 自动更新开关、分支、告警 Webhook（可测试）
- **操作**：立即更新、强制重新部署、重启任一 pm2 进程、重新生成状态页、运行代码体检、备份数据库
- **回滚**：一键回到最近 3 个版本之一；**备份**：历史数据库快照列表
- **日志**：update / backend / frontend / dev-* / status-gen 在线查看
- **审计**：谁在何时执行了什么操作
- **实时**：WebSocket 推送状态变化（断开自动回退轮询，页面右上角显示「实时/轮询」）

## 设计说明

- **零停机**：构建到临时目录，产物校验通过才原子替换 + pm2 滚动重启；失败自动回滚。
- **安全失败**：`.env` 键集合快照（少一个键即中止）、受保护文件冲突预检、本地改动备份、定制层重套。
- **有界自动化**：可回滚的代码变更全自动；不可逆的数据迁移由控制台人工触发并保留快照。
- **公开页 vs 控制台隔离**：控制台挂了不影响静态状态页；静态状态页右下角有极不显眼的入口点。
