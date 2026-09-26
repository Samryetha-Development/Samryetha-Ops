# 架构：把 Web 应用当操作系统来写

> 本文是项目的**地基契约**。代码围绕它生长；与本文件冲突的实现一律视为 bug。
> 读者：内核贡献者、插件作者、以及想借鉴这套思路的人。

## 1. 为什么用 OS 隐喻

单机部署工具通常长成"一堆脚本 + 一个大页面"。脚本能跑，但三件事会持续恶化：

1. **不稳定**：任何改动都碰到同一坨代码，边角逻辑互相牵连，越改越脆。
2. **不可插拔**：想换进程管理器（pm2 → systemd）、换来源（git → OCI）、换认证（OIDC → Basic），
   都要改核心逻辑。
3. **自我更新是死结**：更新器更新自己时，如果新版本坏了，连恢复入口都没有。

操作系统的做法恰好正面回答这三点：**极小内核 + 驱动/模块 + 用户态服务 + 包管理 + 救援模式**。
本项目的核心主张是：

> **把"部署/更新"降级为用户态的一个普通服务；内核不知道"更新"是什么。**

内核只知道进程、事件、存储、权限、路由。`deployer` 只是第一个用户态服务，与未来的
`statuspage`、`inspector`、`notifier` 同级。这让"通用"不是靠堆可插拔开关实现的，而是
**结构上就不存在特权功能**。

## 2. 分层

```
层 0  kernel/       内核       路由 · 事件 · 存储 · 配置 · 权限 · 进程生命周期   ← 极小、极少变
层 1  kmod/         内核模块   内建驱动：git/pm2/systemd/docker/http/tcp/exec…    ← 编译期进入内核
层 2  services/     用户态服务  deployer · statuspage · inspector · notifier…     ← 可独立崩溃/重启/升级
层 3  web/          前端外壳   插槽渲染器 + 设计令牌，只渲染声明                   ← 不含业务
层 4  etc/          配置       deploy.yaml · secrets · 策略                      ← 用户态，更新代码不动它
层 5  pkg/          包管理     插件安装/升级/签名/依赖解析                         ← 内建 + 外部
```

约束（**不变量**）：

- 内核**不得**包含任何领域概念：不得出现 `deploy`、`build`、`branch`、`pm2` 等词。
  出现即违规。内核只知道：路由、事件、键值/文件存储、配置树、权限点、任务与进程。
- 内核**只增不改**：外部可见行为一旦发布，只允许新增，不允许破坏性修改（见 §6 版本策略）。
- 一切功能必须能表达为"服务 + 驱动"，否则说明内核抽象不足。

## 3. 内核边界（层 0）

内核只做六件事，其余都在层 1/2：

| # | 职责 | 说明 | 不做什么 |
|---|---|---|---|
| 1 | **路由** | HTTP/WebSocket 挂载、中间件链、静态资源 | 不解析业务语义 |
| 2 | **事件总线** | 发布/订阅、标准事件信封、持久化游标 | 不知道事件含义 |
| 3 | **存储** | KV、文件、日志段；统一寻址 | 不做领域建模 |
| 4 | **配置** | 加载/校验/热重载/密钥标记/来源分层 | 不含具体配置项的语义 |
| 5 | **权限** | 身份 → 主体 → 能力点（capability）判定 | 不绑定具体 IdP（见 §7） |
| 6 | **进程生命周期** | 启动/停止/探活/重启/资源限额/优雅退出 | 不知道进程"是什么服务" |

内核明确**不做**：定时任务（服务自己声明）、构建（驱动）、部署编排（服务）、
通知（服务）、页面渲染（外壳）、认证协议实现（认证驱动）。

## 4. syscall：插件与内核的唯一通道

插件（层 1/2）**只能**通过 syscall 与内核交互，不得直接依赖内核内部结构。
两个后端实现同一张 syscall 表：

- **内建（in-proc）**：Go 接口调用，零序列化开销。稳定性由"内核只增不改"保证。
- **外部（out-of-proc）**：`JSON-RPC 2.0` over stdio 或 Unix socket。任何语言可实现、可隔离、可崩溃重启。

两种后端暴露**同一组调用名与语义**，因此同一份插件逻辑可内建或外置。

### 4.1 syscall 表（v1）

命名 `<域>.<动作>`，全部返回统一信封 `Result{ok, data, error}`。`perm` 列为所需能力点。

**日志**
| 调用 | 参数 | 返回 | perm |
|---|---|---|---|
| `log.write` | `level,msg,fields` | `{}` | — |
| `log.query` | `since,limit,level,q` | `entries[]` | `log.read` |

**事件**
| 调用 | 参数 | 返回 | perm |
|---|---|---|---|
| `event.emit` | `topic,payload` | `{id}` | `event.emit` |
| `event.subscribe` | `topic` | `stream` | `event.read` |
| `event.history` | `topic,since,limit` | `events[]` | `event.read` |

**存储**
| 调用 | 参数 | 返回 | perm |
|---|---|---|---|
| `store.get` / `store.set` / `store.del` | `ns,key,[val]` | `{val}` | `store.own` |
| `store.list` | `ns,prefix` | `keys[]` | `store.own` |
| `fs.read` / `fs.write` / `fs.list` | `path,[data]` | — | `fs:<scope>` |

**配置**
| 调用 | 参数 | 返回 | perm |
|---|---|---|---|
| `config.get` | `path` | `value` | `config.read` |
| `config.watch` | `path` | `stream` | `config.read` |

**权限**
| 调用 | 参数 | 返回 | perm |
|---|---|---|---|
| `auth.subject` | — | `{id,roles,claims}` | — |
| `auth.check` | `capability` | `{allowed}` | — |

**进程（内核原语，语义中立）**
| 调用 | 参数 | 返回 | perm |
|---|---|---|---|
| `proc.spawn` | `argv,env,cwd,limits` | `{pid}` | `proc.manage` |
| `proc.signal` | `pid,sig` | `{}` | `proc.manage` |
| `proc.wait` | `pid,timeout` | `{code}` | `proc.manage` |
| `proc.list` | — | `procs[]` | `proc.read` |

**任务（内核调度的执行单元）**
| 调用 | 参数 | 返回 | perm |
|---|---|---|---|
| `task.submit` | `name,steps,timeout` | `{taskId}` | `task.submit` |
| `task.status` | `taskId` | `{state,log}` | `task.read` |
| `task.cancel` | `taskId` | `{}` | `task.submit` |
| `task.once` / `task.cron` | `name,schedule,call` | `{id}` | `task.submit` |

**路由与 UI 声明**
| 调用 | 参数 | 返回 | perm |
|---|---|---|---|
| `route.mount` | `method,path,handler` | `{}` | `route.mount` |
| `ws.mount` | `path` | `{}` | `route.mount` |
| `ui.declare` | `slots,nav,settings,…` | `{}` | — |

**时钟/随机/哈希**（避免插件各自造轮子、各自引入不安全实现）
| 调用 | 返回 | perm |
|---|---|---|
| `time.now` / `rand.bytes` / `hash.sha256` | — | — |

> 说明：`proc.*` 与 `task.*` 刻意保持**领域中立**——内核只管"起一个进程/跑一个任务"，
> 不管它是构建、是部署、还是采集指标。这正是"内核不知道更新"的落地方式。

### 4.2 事件信封

```json
{
  "id": "evt_...",
  "topic": "deployment.finished",     // 点分层级，前缀命名空间归发布者
  "ts": 1790000000000,
  "actor": {"id":"...","roles":["admin"]},   // 谁触发
  "source": "service:deployer",              // 谁发出
  "payload": { "...": "..." }
}
```
事件是**只追加**的，可按 topic / since 回放；服务重启后能从游标续订。

## 5. 插件形态

两种形态，同一份 manifest、同一张 syscall 表：

```
plugin/
├── plugin.yaml      # manifest：id/version/apiVersion/capabilities/requires/permissions/isolation/slots…
├── (内建) *.go       # 编译进内核，注册后即为 in-proc 后端
└── (外部) 任意语言 + JSON-RPC over stdio/UDS
```

### 5.1 manifest 规范

```yaml
id: deployer
version: 0.3.1
apiVersion: kernel/v1          # 依赖的内核 syscall 版本
kind: service                  # kernel | service | driver
isolation: inproc              # inproc | process | socket
capabilities: [deploy, rollback]      # 我提供什么（供其它插件/UI 声明依赖）
requires: [git, pm2, http]            # 我需要哪些能力
permissions:                          # 我要用的 syscall 能力点
  - proc.manage
  - task.submit
  - store.own
  - config.read
slots: [overview.cards, actions, logs.sources]   # 我往 UI 哪些插槽注入
nav: [{ id: deploy, label: Deploy, icon: rocket, order: 10 }]
config:                               # 我的配置 schema（内核负责校验与展示）
  - key: branch
    type: string
    default: main
```

内核在**加载期**校验：`apiVersion` 兼容、`requires` 可满足、`permissions` 在允许集合内；
不满足则**拒绝加载并把原因写进内核日志**（而不是半死不活地跑起来）。

### 5.2 隔离级别

| 级别 | 崩溃影响 | 性能 | 适用 |
|---|---|---|---|
| `inproc` | 拖垮内核（须最严格评审） | 最优 | 内核模块、可信驱动 |
| `process` | 内核重启它即可 | 良好 | 大多数服务（**默认**） |
| `socket` | 同上，且可跨语言/跨机 | 有序列化开销 | 第三方插件 |

**原则：默认 `process`。** 只有经过评审、且在内核崩溃预算内的代码才允许 `inproc`。

## 6. 版本与兼容策略

- 内核暴露的 syscall 表有独立版本 `kernel/v1`、`kernel/v2`…
- **只增不改**：同一大版本内，不得删除调用、不得改变既有参数/返回语义。
- 新增能力走新调用或新字段；破坏性变更必须开新大版本，并**同时**支持旧版一段时间。
- 插件 manifest 声明 `apiVersion`；内核按兼容矩阵决定是否加载。
- 内核自身版本与 syscall 版本**解耦**：内核可以修 bug 而不动 syscall 版本。

## 7. 认证与权限（有立场的默认值）

内核的 `auth.*`/能力判定是**机制**；身份来源是**驱动**。

- 驱动可插拔：`oidc`（默认）· `basic` · `token` · `none`。
- **默认立场：OIDC + 以不可变 `sub` 授权。** 邮箱可变、可被抢注，因此默认
  **不**用邮箱做授权标识；若必须用邮箱，则要求 `email_verified=true`。
  这条会写进文档，作为给使用者的建议——**通用不等于没有主见**。
- 权限模型：`身份 → 角色 → 能力点`。内建角色 `viewer` / `operator` / `admin`，
  能力点按 `域.动作`（如 `deploy.trigger`、`proc.manage`）。插件在 manifest 里声明所需能力点。

## 8. 启动、救援模式与 OTA

**boot 序列（init）**：
```
读取 etc/ → 校验配置 → 计算插件加载计划（依赖排序）→ 起内核 →
按 isolation 拉起服务 → self-test → 对外提供
```

**救援模式（rescue）** —— 这是整套设计的安全底线：
- 触发条件：配置校验失败 / 内核 self-test 失败 / 关键服务连续失败超过阈值 /
  显式标记（上次启动未完成）。
- 行为：以**最小配置**启动——只挂载 Web 外壳 + 内核日志 + 一张"救援界面"，
  **不加载任何服务**，并把失败原因原样呈现。
- 退出：修复后由人工点"退出救援模式"重启到正常态。

**OTA（内核自身更新）**：
```
kernel-a/  kernel-b/ + current -> 原子软链
```
- 新内核先写到非当前槽位 → 校验（可执行、self-test）→ 原子切换软链 →
  启动失败自动回滚到旧槽位；连救援入口都保不住时，仍有"从旧槽位冷启"的兜底。
- 服务（含 deployer）的更新走普通部署流程，**不需要 OTA**——这正是把更新降级为
  用户态服务换来的好处：更新器自己坏了，重启它即可，与内核无关。

## 9. 前端外壳（层 3）

外壳**不含业务**，只做三件事：布局/路由/认证、**插槽渲染**、设计令牌下发。

**受限插槽**：插件不能注入任意 DOM，只能声明使用**预定义组件类型**
（`card` / `table` / `chart` / `form` / `list` / `log` / `keyvalue` / `timeline` / `tree`）。
所有组件**自动继承设计令牌**（颜色/间距/字体/圆角/暗色），因此风格永远统一。

**逃生舱**：确需自由渲染时，用**沙箱 iframe**，并在 UI 上明确标注"外部插件"。
这样"通用"与"设计不崩"可以同时成立。

**插槽清单（v1）**：`overview.cards` · `tabs` · `actions` · `settings.sections` ·
`logs.sources` · `nav` · `commands` · `hotkeys` · `toasts` · `modals` · `footer`。

## 10. 包袱边界（明确不做的事）

为了不变成四不像，以下**不在**范围内：

- 不做容器编排/K8s 替代品（可驱动外部系统，但不自己实现调度器）。
- 不做服务发现、分布式共识、多租户计费。
- 不追求"零配置万能"：通用来自**结构**（内核/服务/驱动），不是靠自动猜。
- 不引入重量级运行时依赖：内核是单静态二进制；外部插件用标准协议，不绑定语言。

---

**变更本文件 = 变更契约**：任何对内核边界、syscall 语义、不变量、版本策略的修改，
必须在此文件体现，并在 PR 说明理由与兼容性影响。
