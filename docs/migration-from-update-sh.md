# 从 update.sh 迁移到内核

本文记录迁移过程中发现的**真实差异**，以及为什么新引擎会在某些情况下
"失败"——这些失败不是缺陷，而是旧引擎把问题掩盖了。

## 迁移原则

1. **行为等价优先**：替换期间，新引擎对同一输入应产生与旧引擎相同的结果。
2. **失败要响**：旧引擎里被静默忽略的问题，新引擎应明确报错。
   因此"迁移后失败"的第一反应应是**查配置**，而不是查引擎。
3. **可回滚**：任何时刻都能退回旧路径（crontab 备份见迁移脚本）。

## 已发现的差异

### 1. 构建命令的工作目录

`update.sh` 里是 `(cd "$1/backend" && uv sync)`——子 shell 内的相对路径。
迁移到 `deploy.yaml` 时若直接抄成 `cd backend && uv sync`，
内核会以 **workdir** 为基准再进一次 `backend`，路径变成 `workdir/backend/backend`。

**结论**：`deploy.yaml` 的 `build` 应是**绝对路径或明确基准**，不要依赖隐式 cwd。
（这也是内核刻意的设计：它不猜调用者想要哪个目录。）

### 2. 不存在的 hook 文件

`deploy.yaml` 里写了 `before_deploy: bash /opt/.../sync-dev-data.sh`，
但该文件从未创建。旧 `update.sh` 把 dev 数据同步内联在流程里，
迁移时我把它"抽象"成了一个不存在的脚本。

**结论**：迁移时要**逐条核对配置引用的实体是否存在**。
内核在这里正确地失败了（`before_deploy hook failed`），
这正是我们想要的：配置错误不该被静默跳过。

### 3. 健康检查的 expect

`deploy.yaml` 里部分 health 没写 `expect`。内核此时只验证"curl 成功"（2xx/3xx），
不验证具体状态码。

**结论**：关键目标应显式写 `expect: "200"`，让断言可读且可审计。

### 4. 定时任务的"注册"与"绑定"

内核的 cron 由**内核**注册（负责到点触发），但"到点做什么"属于**服务**。
早期实现只注册未绑定，导致定时任务按时触发却什么都不做——
在日志里几乎看不出来。

**结论**：统一用 `sdk.BindCron`（注册+绑定一步），不要单独调 `sdk.Cron`。
内核侧对应 `SetCronHook`。

## 迁移步骤（安全顺序）

```bash
# 1. 并行运行（只读）：新内核另起端口，与旧 update.sh 同时存在
#    验证：内核读到的事实与 update.sh --status 一致
python3 scripts/verify-consistency.py

# 2. 交接调度：内核接管 cron，移除旧条目
crontab -l | grep -v 'update.sh' | crontab -   # 备份先行

# 3. 观察：内核的部署事件与目标版本是否如期变化
curl -s localhost:3040/api/kernel/events?topic=deployment

# 4. 回滚（如需要）
crontab /tmp/crontab.backup.<ts>
```

## 已知待办

- `deploy.yaml` 里的构建/重启命令仍需与服务器实际流程逐条对齐
- dev 数据同步（旧 `sync_dev_data`）尚未迁移为独立服务或 hook 脚本
- `ops` 目标的 `release` 来源驱动尚未实现（当前仍由旧 `ops_self_update.sh` 负责）
