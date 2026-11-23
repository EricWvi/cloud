# workspacefs

只读查看 Agent 的工作副本（另有删除已删对话的工作副本，见文末）：一层目录、一个文本文件、相对克隆时 commit 的变更列表和单文件 diff。路由与行为见 [v2-server README 的「Agent 工作区查看」](../../README.md#agent-工作区查看)。

- 页面传入的路径先经 `Clean` 规范化（拒绝绝对路径、反斜杠、NUL 和 `.git`），再在解析符号链接后确认仍在工作副本内。
- 变更和 diff 用 index 的临时副本加 `git add --intent-to-add --all` 让未跟踪文件可见，不写 Agent 的 index。
- 所有读取都有上限（`MaxFileBytes`、`MaxDiffBytes`、`MaxEntries`、`MaxChanges`），超出返回 `ErrTooLarge` 或标记 `Truncated`。

## 回合交付

一次交付使用独立的临时 Git 目录和以登记的起点初始化的临时索引。Git 从工作目录读取 `.gitignore`，并使用复制进临时仓库的本地与共享 Git 排除规则。排除文件的链接只能解析到工作目录或对应的 Git 目录内，无法安全读取时交付失败。`.git` 可以是目录、gitfile 指针或指向目录的符号链接（后两者按同样规则解析，见 `internal/gitexclude`）。Agent 删掉了 `.git` 时，交付不依赖 Agent 的仓库，照常进行、没有本地排除规则；但记录 Ora 物化技能路径的规则也随之消失，所以这时只要 `.opencode/skills` 或 `.claude/skills` 下有相对起点的改动，交付就失败（`ErrMaterializedUnknown`，记为 `delivery_unavailable`），不冒把技能带进交付的风险。临时仓库不使用 Agent 配置，所以工作目录属性指定的外部过滤器不会执行。提交和对象先在临时仓库生成，大小检查通过后导入镜像，再以只新建的方式发布交付 ref。调用方持有工作目录锁直到 PostgreSQL 写入交付结果。

`DeliveryChanges`、`DeliveryDiff` 和 `DeliveryPatch` 从项目镜像比较交付提交和它的起点。完整 patch 包含二进制修改，超出调用方限制时返回 `ErrTooLarge`。

启动时清理数据根目录下的 `delivery-scratch/`。写入 Git 对象前，交付先按工作目录中变更及未跟踪的普通文件大小检查上限。写入后继续检查相对起点的新增对象大小。

## 删除对话时的检查与删除

`InspectReclaim` 持目录锁按交付的快照规则在临时目录生成树（不写镜像、不写 ref），与给定交付提交（空时起点）的树比对，列出差异（至多 `MaxReclaimChanges` 条，另给总数），并查交付带不走的 Git 状态（`refs/stash`、`refs/remotes/` 之外超出起点与 `HEAD` 的本地 ref、与起点不同的 gitlink、嵌套 `.git`）和有无被忽略规则排除的文件（不算物化的技能），并带出快照树 OID 和 Git 身份摘要供预览的 `revision` 绑定内容。身份摘要绑定 discard 会删掉、交付带不走的全部 Git 相关字节：顶层仓库的 ref/HEAD/stash OID 与 index 内容，每个嵌套仓库整棵子树（含 `.git` 与对象）的内容，gitfile、链接或 `.git` 目录里的 `commondir` 指向 checkout 内的 Git 目录时连同目标整棵（同一目录只算一次）。读取不调用 Git、不跟随链接：先 `Lstat` 只接受普通文件，再以 `O_NOFOLLOW|O_NONBLOCK` 打开并 `fstat` 复核；嵌套 Git 目录（含 `.git/modules/` 与解析后的 Git 目录）内出现符号链接或 `objects/info/alternates` 时同样不跟随，直接将摘要标记为不完整。内容字节共用交付的对象上限、条目至多 10 万；任何超限、特殊文件、读取失败、嵌套 Git 目录内的符号链接或 `alternates`、指向 checkout 外或解析不了的指针都让摘要不完整（`GitIdentityComplete` 为 false），且不会再恢复。整个检查受 2 分钟上限约束。目录内容无法验证时返回 `ErrUnverifiable`，其余错误（磁盘、镜像、Git）可重试。

`RemoveCheckout(root, path)` 只删除位于 checkout 根 `root`（`<项目目录>/w/r`）之下的 `path`：从 `root` 起经目录句柄逐级打开，每级先 `Lstat` 确认是目录而非链接、打开后比对 dev/inode；统计、补属主写权限、删内容（链接只删本身）经 checkout 句柄，最后经父目录句柄删掉 checkout 本身，检查之后不再按路径重开。读不到的条目返回带路径的错误，列出后消失的不计。不安全时返回 `ErrUnsafeCheckout`；路径或根已不存在视为已删除。
