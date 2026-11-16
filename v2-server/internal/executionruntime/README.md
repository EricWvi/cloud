# 公共执行服务装配

本包将 `execution.Service` / `Catalog` 连接 PostgreSQL、`modelturn` 和项目 Agent driver。
调用方传入可信配置；接收（开始执行）时用 `store.TenantAccess` 检查当前租户授权，已经接收的 worker 按原归属保存终态。
页面 HTTP 接口由领域服务接入。

## 接收、事件与结束

`New` 只在调用方已完成 Node 停止恢复后构造，并先结束旧的持久会话、结算未完成轮次为 interrupted。
同一数据库和数据目录只能有一个服务实例。它不重建旧 worker、不重放 prompt，也不恢复旧 Agent 上下文。
`main` 持有数据目录锁，先构造项目 driver，再构造本服务，最后开放 HTTP。

Start/StartModel/Append 以当前租户授权（`TenantAccess`，store 在记录会话或轮次的事务内再查一次）和 Caller attempt 接收，记录原始 prompt 后启动服务自有 worker。
同一个 attempt 同参数返回同一 Ref，参数改变返回 Conflict；同一对话（定时、任务同理）的轮次串行，忙时拒绝；workflow run 的每个 attempt 是独立会话，只在各自会话内串行，同一 run 的多个会话同时执行轮次。
观察者已收到持久化的 turn_ended、但原 worker 尚在 ACK/收尾时，Append 等待该 worker 退出后
重新检查授权、会话和 attempt，再接收下一轮；等待遵从请求 context，不提前释放目录或重叠派发。
原轮尚无持久终态时仍立即返回 Busy，已存在的同 attempt 重试仍直接返回原 Ref。
不同对话、定时触发各有自己的 checkout；同一 workflow run 复用一个 checkout，run 的各个会话在里面并存：换节点时不结束之前的会话（`LifetimeHeld` 的交互节点会话因此能一直等到发起人回来），每个会话由调用方自己 End；它们的轮次可以同时执行。调用方 End 失败的会话不会再被下一个节点的 Start 顺带结束，它留在 checkout 上并占 Node 的一个会话名额，直到该项目的 Node 被重建（见下文）或进程重启。Node 以 `shared_checkout_sessions` 能力允许同一 checkout 上有多个 Agent 会话。
没有完成的 Agent 轮次结束 `LifetimeTurn` 会话；`LifetimeHeld` 会话只在 driver 报错（Agent 丢失）或这一轮的结果写不进数据库时结束，runtime 自己写的结果此时带 `AgentLost`。
只支持文本 prompt；附件、权限询问、运行中切换模型尚不支持。

公共读取、Cancel 和 End 只按原归属（租户与用户）定位，不再检查租户授权：页面请求已由 HTTP 租户路由授权。执行中撤权后不能再接收新轮次。
已经接收的 worker 按数据库保留的原始归属继续写入事件和终态，内部 ownership-only 写入方法不暴露给页面。
有序事件、完整结果和最终停止原因由 PostgreSQL 保留；Node 的历史快照/轮次终态先写库再 ACK。
Subscribe 从持久 cursor 轮询，关闭观察者不停止 worker；会话结束后仍可按原序号读完再 EOF。

Agent Cancel 使用固定 Node 的 EndSession，因此会结束整个物理会话；纯模型 Cancel 仅取消指定轮。
取消旧的已完成 Ref 只返回原结果。End 等 worker 与物理停止确认后再写 session_ended；
停止或终态写入无法确认时保留目录、阻止该会话后续接收，返回 StopUnconfirmed 或 Unavailable。

一轮的失败只影响它自己的会话，不让整个服务停止接收：
- 终态写入失败先重试两次（间隔 1s、2s，每次 10s 上限）；仍失败时该会话不再接收新轮次，有项目的会话停止物理 Agent。
  没写进去的结果留在内存，服务每 5s 在后台继续写，直到写入成功或服务关闭：等订阅或轮询结果的调用方（工作流、任务运行、定时任务、对话）
  不需要调用 End，数据库恢复后就能收到 turn_ended。写入成功后，无项目或物理停止已确认的会话随即结束；停止未确认的只补写结果，
  会话要等之后的 End 拿到停止确认才结束。End、Cancel 也会先补写。进程重启前没补上的按重启中断处理。
- 存储拒绝的无效结果（例如 completed 但答案不完整）永远写不进去，改记为失败：只把状态改为 failed、失败码设为 `execution_failed`，
  答案、停止原因、用量和 `AgentLost`、`NotStarted` 照原样保留，不让这一轮一直停在运行中。
- 物理停止由服务自己限时 30s，worker 收尾、End 和 Cancel 对同一会话共用一次尝试：调用方可以提前放弃等待，尝试本身继续，
  所以调用方的短 deadline 不会把只是回复慢的 Node 标坏，没有 deadline 的调用方也不会一直等。到时仍未确认就调用 driver 的 `Rebuild`，
  只把该项目的 Node 标为需要重建；同一项目的下一个会话或之后的 End 先由 supervisor 确认旧进程树停止，再起新 Node。
  该会话保持结束中；同一 workflow run 的后继节点照常开始，不受它阻挡。
- 等待停止都在接收锁之外，期间不阻塞其他会话和项目的接收、清单和 Close。
Close 取消并 join worker，再关闭 Agent driver，必须在数据库关闭前完成。

`ModelsFromEnvironment` 只返回真实配置：`V2_MODEL_*` 和 `V2_ANTHROPIC_MODEL_*` 各可提供一个 provider
（`configured` / `messages-configured`），每组三个值必须同时设置；没有完整配置时返回空目录。
执行器按 Agent 的协议配对绑定：OpenCode 得到 `opencode:configured`，Claude Code 得到去掉
`messages-` 前缀的 `claude:configured`；OpenCode 排在前面。Messages 绑定不进模型清单，纯模型 Start
选它返回 NotFound。无 driver 时执行器清单为空，Agent Start 返回 Unavailable；纯模型仍可执行。
服务在没有真实模型时也能启动，两个目录都返回空列表，不回退 fixture。
`V2_MODEL_IDLE_TIMEOUT` 与 `V2_MODEL_MAX_DURATION`（默认 `2m`、`30m`）设到这两个 provider 上，
取值非法或空闲超时大于总上限时返回错误，不静默回退默认值。

fixture 目录只由 `WithFixtureModels` 显式合入，供 fixture-check、编排测试和 Playwright 使用：
OpenAI 协议的 `fixture` 与 Messages 协议的 `messages-fixture` 分别生成 `opencode:fixture` 和
`claude:fixture`。正常启动不调用这个函数。
公开清单只有模型名/绑定版本和执行器选择，不返回 provider 地址、密钥或本地路径。
服务关闭时，清单也返回 Unavailable，供消费侧阻止新触发；单个会话的终态写入失败或停止未确认不影响清单。

## 验证

普通包测试使用独立的真实 PostgreSQL schema，覆盖 attempt 幂等、并发接收、事件续读、多轮、
取消、停止失败只围住该会话、停止限时且不占接收锁、终态写入的重试与后台补写、撤权后的公开控制拒绝与内部终态结算、关闭重开后的完整历史。
`TestRealServiceAgentPostgresRestartAndConcurrentConversations` 在显式配置 `V2_NODE_ASSETS_DIR` 后
通过真实固定 Node/OpenCode 与 fixture 模型验证 shell 文件、双 checkout、End 和重启读回。
`TestRealClaudeCodeRunsTheMessagesFixture` 用同样的资产让固定 Claude Code 走 Messages fixture：
冻结 agent 配置、Messages grant、Bash 工具写 marker，再在同一会话上 Append 一轮。
缺资产会明确 skip，不能将普通测试或编译通过称作真实 Agent 验收。

## 外部 provider 验收

Linux 测试 `TestConfiguredProviderServicePostgresRestart` 使用真实公共 Service、专用 PostgreSQL
schema、`SeedDemo` 的独立 Git 项目，以及固定 Node/OpenCode。它验证无项目短文本回答和有项目
工具写入固定 marker，随后 End、关闭并重新打开连接池和 Service，比较持久化结果与完整终态。
模型回答只要求 completed、AnswerComplete 和非空，不要求固定文案。

默认明确 skip，表示**外部 provider 未验收**。只有 `V2_REAL_PROVIDER_TEST=1` 才会发真实请求；
显式开启后，缺少任意 `V2_MODEL_BASE_URL`、`V2_MODEL_API_KEY`、`V2_MODEL_NAME`、
`TEST_DATABASE_URL` 或 `V2_NODE_ASSETS_DIR` 都直接失败，不退回 fixture 或跳过。
provider 必须是 `ModelsFromEnvironment` 支持的 HTTPS OpenAI 兼容接口，并支持固定 OpenCode 的
工具调用。配置应由本地安全环境注入，不把密钥写入命令、提交或验收记录。

`TestConfiguredMessagesProviderRunsClaudeCode` 是 `claude:configured` 的同类验收：
`V2_REAL_MESSAGES_PROVIDER_TEST=1` 才发真实请求，要求 `V2_ANTHROPIC_MODEL_*` 指定地址、模型和一种凭据（`API_KEY` 或 `AUTH_TOKEN`）、`TEST_DATABASE_URL`
和 `V2_NODE_ASSETS_DIR`，由真实 Claude Code 用 Bash 工具写 marker 并给出回答。默认 skip，表示外部
Messages provider 未验收。

在仓库根目录运行 `task v2:node:prepare` 准备已校验资源。需要原生 Linux/WSL x86_64 或 aarch64、Git、
`git-http-backend`、Go（版本见 `go.mod`）、可创建/删除 schema 的测试 PostgreSQL，以及可直连
provider 的网络；当前 RemoteProvider 不使用环境代理。HOME 必须位于原生 ext4/xfs/btrfs
短路径，不能使用 Windows 挂载或容器 overlay。测试资源目录和执行目录的约束见
[`node/README.md`](../node/README.md)。

上述变量已经安全注入环境后，在 `v2-server` 运行：

```sh
V2_REAL_PROVIDER_TEST=1 go test -race -count=1 -timeout=12m \
  -run '^TestConfiguredProviderServicePostgresRestart$' -v ./internal/executionruntime
```

也可以在构建容器里 `go test -race -c -o /output/executionruntime.test ./internal/executionruntime`，
再在原生 Linux/WSL 文件系统运行该二进制，传入同名 `-test.run`、`-test.v`、`-test.timeout=12m`。
容器编译无需 provider 配置或密钥。

测试入口不自动重试失败请求或整轮；固定 OpenCode 保留自己的模型请求与工具调用策略，因此
一次 Agent 轮次可能产生多次模型请求；是否收费以所选 provider/model 为准。成功与失败输出都不打印
provider/数据库 URL、密钥、回答正文或原始 Node 日志。模型输出会暂存在本次专用 schema 和私有目录内；成功确认自有进程停止后，清理
这些测试数据，不使用服务的 `V2_DATA_DIR` 或已有项目。停止未确认时失败并保留独立证据，避免删除
活跃运行目录。此入口不验证页面路由，也不把默认 skip 或仅编译通过算作外部 provider 验收成功。
