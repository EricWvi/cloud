import { expect, it } from 'vitest'
import { taskOwnedRetryText } from './task-owned-retry'

// D30, D32: each kind of task-owned run points back to the task in its own words; a run no task
// owns has no such text and keeps the retry.
it('names the way back to the task for each task link', () => {
  expect(taskOwnedRetryText('handle')).toBe('这次运行是任务的处理运行，任务已按它的结果结算，不能在这里重试任务创建；请回到任务页重新开始处理。')
  expect(taskOwnedRetryText('decomposition')).toBe('这次运行是任务的拆分运行，拆分已按它的结果结算，不能在这里重试任务创建；请回到任务页重新拆分。')
  expect(taskOwnedRetryText(undefined)).toBeUndefined()
})
