# workspacefs

只读查看 Agent 的工作副本：一层目录、一个文本文件、相对克隆时 commit 的变更列表和单文件 diff。路由与行为见 [v2-server README 的「Agent 工作区查看」](../../README.md#agent-工作区查看)。

- 页面传入的路径先经 `Clean` 规范化（拒绝绝对路径、反斜杠、NUL 和 `.git`），再在解析符号链接后确认仍在工作副本内。
- 变更和 diff 用 index 的临时副本加 `git add --intent-to-add --all` 让未跟踪文件可见，不写 Agent 的 index。
- 所有读取都有上限（`MaxFileBytes`、`MaxDiffBytes`、`MaxEntries`、`MaxChanges`），超出返回 `ErrTooLarge` 或标记 `Truncated`。

## 回合交付

一次交付使用独立的临时 Git 目录和以登记的起点初始化的临时索引。Git 从工作目录读取 `.gitignore` 和复制进临时仓库的 `info/exclude`。临时仓库不使用 Agent 配置，所以工作目录属性指定的外部过滤器不会执行。提交和对象先在临时仓库生成，大小检查通过后导入镜像，再以只新建的方式发布交付 ref。调用方持有工作目录锁直到 PostgreSQL 写入交付结果。

`DeliveryChanges`、`DeliveryDiff` 和 `DeliveryPatch` 从项目镜像比较交付提交和它的起点。完整 patch 包含二进制修改，超出调用方限制时返回 `ErrTooLarge`。

启动时清理数据根目录下的 `delivery-scratch/`。写入 Git 对象前，交付先按工作目录中变更及未跟踪的普通文件大小检查上限。写入后继续检查相对起点的新增对象大小。
