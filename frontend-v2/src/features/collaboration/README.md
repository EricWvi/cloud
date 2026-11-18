# 协作

页面和导航叫「协作」（路由 `/collaboration`），承载任务（task）：工作区里指派给人、Agent 或小队的一件活，属于一个项目，全团队可见，数据来自 v2-server 的候选接口。小队（squad）是工作区内人和 Agent 的协作编组，属于配置，在「设置 / 小队」里管理（`features/settings/squads/`）；协作页只把任务指派给小队。

## 页面结构

协作页有自己的侧栏（`page.tsx`），右侧是子路由（`index.ts`）：

- `/collaboration`：任务看板（默认）或列表（`tasks-page.tsx`）。看板占满窗口高度，每列各自滚动。侧栏的视图「我的任务 / 待我处理 / 全部任务」都是同一份全团队任务列表的筛选；页头的小队切换把任一视图收窄到一个小队，我所在的小队排在前面，选中后新建的任务默认指派给它，这只是筛选，不隔离可见范围。这些都写在 URL 里（`?view=`、`?squad=`、`?assignee=`、`?layout=list`），链接发给队友看到的是同一个视图。没有推送：有 Agent 在执行时列表每 3 秒重读，否则 15 秒。
- `/collaboration/tasks/:taskId`：一个任务（`task-page.tsx`）。左边是「Agent 运行」、「工作流」和「讨论」页签，下面一个输入框，右边是任务信息、指派、执行和状态推进。
- `/collaboration/squads`：旧地址，带着查询参数重定向到 `/settings/squads`。

协作里的一切都属于团队；个人的对话留在对话页。运行过程用 `src/components/agent-thread` 的线程视图，和对话页、工作流运行页是同一套。

## 已经有的

- 任务列表显示状态、标题、项目、指派对象，以及谁在干活（某个 Agent 执行中、等待追问）。看板按「待处理 / 进行中 / 待验收 / 已完成」分列，「已取消」默认隐藏。把卡片拖到另一列，等同于点任务里对应的推进按钮：拖动时只有合法的下一步亮起，状态逐级推进，不能跳过待验收；Agent 正在执行的卡片拿不起来；指派给 Agent 的任务只能在任务里「开始处理」进入进行中，不能拖进去。松手后卡片立即移动，服务端拒绝（例如 `409 run_active`）时退回原列，并弹出提示说明原因，几秒后自动消失。拖动只支持指针，键盘用任务页的按钮。窄屏下看板变成单列。
- **拆分**：大需求在任务页点「拆分」，选一个已发布的工作流（默认「需求拆分」）运行，任务的标题和描述是输入。能否拆分由服务端判定（`GET …/decomposition-workflows`，客户端在 `decomposition-api/`）：可拆分的排在前面，不可拆分的放进禁用分组「不可拆分（缺少 tasks 输出）」，下面的折叠说明只写一次原因，再列出它挡住的工作流名；启动被拒绝时显示服务端 `params.message`。跑完后输出的 `tasks` 变成这个任务的子任务：待处理、未指派、同一个项目，任务页列出它们，看板上子任务标着「拆分自 …」，大需求标着「N 个子任务」，Tech Lead 再把它们分给人和 Agent。拆分中按钮变成「拆分中…」，失败时任务页写明原因并可「重新拆分」。子任务不能再拆。工作流运行也在工作流页的运行历史里。
- **工作流页签**：始终显示，按持久任务身份列出处理、拆分、巡检来源与评论 @ 四类记录，包含版本、真实发起人、状态、失败原因与时间；不按标题匹配。点一行在页签里打开同一个运行视图（`?tab=workflows&workflow=<运行 id>`），巡检来源不再跳转到对话页。拆分开始后直接打开运行。运行视图不提供启动和重跑；能查看任务的成员可在活处理运行的列表或打开的运行中点「停止」，使用对话页同一取消接口，不限发起人。标题左边返回列表；页头「工作区」查看这次运行的文件。打开运行时收起输入框并折叠子任务，让画布获得空间。
- 任务指派给人、可执行的预装 Agent 或未归档的小队；指派给小队不挑具体的人：每个队员的「我的任务」都有它，队员还没评论或开始处理时它也在「待我处理」，卡片标出是哪个小队派来的。
- **执行**：指派给 Agent 的任务在右侧选模型后「开始处理」，用你的身份运行，任务进入进行中，一轮结束自动进入待验收；停止或服务重启后回到待处理。Agent 执行期间不能手动推进状态。已经有运行时按钮是「重新执行」，会结束空闲的旧会话。运行开始后和对话页一样显示准备阶段：列出全部阶段、当前第几步和已用时，首次输出后收起为总用时。项目第一次用某个 Agent 要先在 Node 里安装，这段时间算在「安装插件」里。
- **工作区**：页头的「工作区」打开当前运行的文件和 diff，占掉右侧属性栏的位置，可以拖宽，关掉后属性栏恢复。390px 下工作区占用可用全宽，关闭后返回任务，避免挤压任务与文件内容。每次 Agent 运行和每次工作流运行都有自己的目录，互相看不到改动，所以面板标明正在看哪次运行；没有运行在屏幕上时，它打开最新一次。变更和 diff 读这次运行最近一次成功的回合交付，可以下载 patch；交付失败时任务回到待处理（服务端），失败原因显示在这次运行的工作区面板「变更」上方，规则与对话相同（见根目录 README 的对话一节）。
- **派给人的任务**：负责人本人可以在右侧「用 Agent 处理」：从自己能用的全部 Agent 与模型里选一个，用自己的身份开一次运行，之后和 Agent 任务一样（跑完进入待验收、可停止、只有发起人能追问）。其他人看不到这组控件，输入框只发评论；负责人不用 Agent 时照旧手动推进。
- **用工作流处理**：权限与 Agent 处理一致：人类负责人本人、小队当前成员，以及 Agent 任务的任一工作区成员可见，其他成员看到不可用原因；不受 demo 开关控制。明确选择已发布工作流，编辑需求及 Start 变量后，以负责人的身份启动；服务端固定项目与发布快照、持久记录关联并按幂等键防重。活 Agent 会话（含空闲会话）与处理工作流互斥。开始进入进行中，成功或失败进入待验收，执行后取消或重启中断回到待处理；执行前就被停止或删除的启动不算发生过，任务保持原状态，提交后启动失败而运行已开始执行的改由宿主停止并按真实结果结算。按处理关联自己的修订号与所写任务状态防护，元数据与指派修改不阻止结算；状态循环会撤销旧运行的状态所有权；活处理运行期间禁止人工推进与关闭任务，能查看任务的成员先在「工作流」页签点击「停止」。启动前拒绝不创建关联、不移动状态；提交后拒绝恢复原状态；拆分与巡检不推进状态。
- **派给小队的任务**：和派给人的任务一样，任一队员都能用自己能用的 Agent 与模型开运行；同一时间一个运行，只有发起人能追问，其他队员可以重新执行。队外的人可以评论，不能开运行。
- **运行过程**：全团队都能看每次运行的思考、工具调用和回答，按运行切换；运行结束的原因（停止、被重新执行替代、任务结束、重启中断）和最近一轮的结果写在运行上方。
- **讨论与 @**：能发给这个任务的 Agent 时（指派了 Agent、它没在执行、调用者是当前运行的发起人或还没有运行），输入框默认发给 Agent，评论同时全团队可见；关掉「发给 Agent」就只发团队评论；发不了时开关关闭并说明原因。输入 `@` 弹出工作区成员、Agent 和已发布的工作流：选成员是提及，被提及的人在回复前会在「待我处理」里看到这个任务，卡片标着谁 @ 了他；选这个任务的 Agent 等同打开「发给 Agent」；选工作流会加载它的启动表单、按字段校验输入，发送后以评论人身份启动运行（不改变任务状态、不占用处理锁），并在讨论时间线卡片实时显示运行状态、链接（`?tab=workflows&workflow=<运行 id>`）与完成后的输出。只有从菜单选中的对象算数，正文里手打的「@名字」不路由。
- **谁能追问**：运行属于发起人，只有发起人能追问（不然就是用别人的身份跑）；其他成员的「发给 Agent」是灰的并写明原因，他们可以评论，或重新执行用自己的身份开一次新运行。
- **谁能结束任务**：「确认完成」和「取消任务」只限任务创建人和工作区 owner，由服务端判定（其他人 `403`）。负责人负责开始和提交验收，不能验收自己接到的任务；自己建给自己的任务，创建人身份照常可以结束。其他人在任务页看到禁用的按钮和原因，在看板上拖到这两列会被拦下并说明原因。其余操作（编辑、指派、拆分、停止）目前仍对全体成员开放。
- **按指派对象筛选**：「全部任务」页头可以按人、Agent、小队或未指派筛选（`?assignee=human:<id>`、`?assignee=none`），只列手上有任务的对象和数量；和小队切换互斥，选一个清掉另一个。
- 写操作按任务 version 做乐观校验；被拒绝时按服务端的码给出原因，不静默重试。

## 还没有的

- 自动委派与 leader 路由；@小队（正式契约里有，v2 不返回）。
- 实时推送：靠定时重读，别人的改动最多晚十几秒出现。
- 别人运行进行中时的追问排队：现在直接拒绝并说明原因。
- 运行交付的成果（提交、PR）：v2 的 Agent 改动不提交、不推送。

## 接口与检查

小队与任务接口都不在正式 Cloud 契约里：小队由 `v2-server/squads.openapi.json` 描述，用 `npm run api:generate:squads` 生成 `src/lib/squads-api/generated.ts`（协作页与设置共用）；任务（含运行、评论、时间线与拆分）由 `v2-server/tasks.openapi.json` 描述；@ 目录是正式契约的 `collaboration/targets`，用 `src/api` 的生成客户端，用 `npm run api:generate:tasks` 生成 `src/lib/tasks-api/generated.ts`（旧 feature 路径保留转导出）。两个客户端分目录存放，各自的 Orval 配置会清空自己的输出目录。任务表单用的项目列表来自 `src/lib/projects-api`（聊天页与协作页共用，因此放在 `src/lib/`），由 `api:generate:chat` 生成。`npm run check` 会先做 `api:check:squads`、`api:check:tasks`、`api:check:chat`，确认生成结果没有漂移。会话与租户继续走正式生成客户端。

`npm run check` 运行类型检查、lint、单元测试和构建；`npm run e2e` 用真实 v2-server 与 PostgreSQL 跑建任务、指派、推进、评论、看板，以及在设置 / 小队里建队、增删成员、归档的流程；设了 `V2_NODE_ASSETS_DIR` 时另跑真实 Agent 执行任务、另一个成员旁观与评论、发起人追问和停止，并通过 `E2E_DATABASE_URL` 与 `PW_CHROMIUM_PATH` 指定数据库和浏览器。

## Task workflow behavior (English)

The task page always shows one workflow tab. Its generated client reads durable handling,
decomposition and inspection-source associations, including version, actual starter, status, failure
and time. It never matches task titles. Deleted handling/decomposition records remain visible but cannot open. Other rows open in the tab; inspection sources stay on the
task page. The embedded run view has a back control and hides the composer while open. Every member
who can see the task can stop a live handling run from its row or the opened run using the existing
chat cancellation endpoint; this is not restricted to the starter.
Start and rerun remain unavailable here.

Handling uses the same assignee policy as Agent starts: the named human, current squad members,
or any workspace member for an Agent-assigned task; others see why it is unavailable. No demo
flag is required. The dialog explicitly picks a published workflow and reuses the normal typed Start fields, sending the displayed snapshot, input and Start values
in one idempotent start. The server fixes project and snapshot and moves task status: start to
running, success/failure to review, cancellation after execution and restart interruption to
pending, while a start stopped or deleted before it ever executed never happened and keeps the
task's original status; a start failure whose run already began executing is stopped by the host
and settles with its real outcome, all with the handling link’s own revision and admitted-status fence; metadata edits and reassignment
do not invalidate settlement. Manual status changes are blocked while handling is live; any member who
can see the task clicks 停止 in the Workflow tab first.
Settlement promptly refreshes the task and its version before the next manual move.
Preflight refusal writes nothing; a later start refusal restores the original task status.
A live handling run excludes live Agent sessions, including idle sessions. Decomposition and
inspection never move task status. At 390px the workspace takes the available full width and closes
back to the task.

The candidate contract is `v2-server/task-workflows.openapi.json`; regenerate its client with
`npm run api:generate:task-workflows`. The check gate verifies this client too. Playwright's
`e2e/task-workflows.spec.ts` covers real same-title task isolation, all three kinds, Start values,
backlinks, the starter's stop and phone layouts; `e2e/task-workflow-stop.spec.ts` covers a member
who did not start the run stopping it; `e2e/workflow-forms.spec.ts` covers opening inspection reports in-tab.
