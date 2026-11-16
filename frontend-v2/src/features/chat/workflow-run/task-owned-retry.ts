/** How a task owns a run (`run.taskLink`). */
export type RunTaskLink = 'handle' | 'decomposition'

const taskOwnedRetryTexts: Record<RunTaskLink, string> = {
  handle: '这次运行是任务的处理运行，任务已按它的结果结算，不能在这里重试任务创建；请回到任务页重新开始处理。',
  decomposition: '这次运行是任务的拆分运行，拆分已按它的结果结算，不能在这里重试任务创建；请回到任务页重新拆分。',
}

/**
 * Why a run a task owns offers no task creation retry, pointing back to the task: its link settled
 * with the run (owner decisions D30 for a handling run, D32 for a decomposition's run). Undefined
 * for a run no task owns, which keeps the retry.
 */
export const taskOwnedRetryText = (taskLink: RunTaskLink | undefined): string | undefined =>
  taskLink === undefined ? undefined : taskOwnedRetryTexts[taskLink]
