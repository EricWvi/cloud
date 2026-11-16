import { render, screen } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { MemoryRouter } from 'react-router'
import { expect, it, vi } from 'vitest'
import type { GraphWorkflowNodeState } from '@/lib/workflows/runtime'
import { RunTasksStage } from './run-tasks-stage'

const api = vi.hoisted(() => ({ read: vi.fn() }))
vi.mock('@/lib/workflows/api/generated', () => ({ getWorkflowForm: api.read }))

const succeeded = (output: string): GraphWorkflowNodeState => ({ status: 'succeeded', output: { summary: output, detail: output } })

function mount(state: GraphWorkflowNodeState, onOpenForm?: () => void, taskLink?: 'handle' | 'decomposition') {
  api.read.mockResolvedValue({
    findings: [{ key: 'api', title: '接口错误率超过阈值' }, { key: 'disk', title: '磁盘占用超过阈值' }],
  })
  const client = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } })
  render(<QueryClientProvider client={client}><MemoryRouter>
    <RunTasksStage tenantId="tenant" runId="run" nodeTitle="创建或关联协作任务" state={state} onOpenForm={onOpenForm} taskLink={taskLink} />
  </MemoryRouter></QueryClientProvider>)
}

it('lists created and reused tasks with their own links', async () => {
  mount(succeeded(JSON.stringify({ tasks: [
    { key: 'api', taskId: 'task-created', action: 'created' },
    { key: 'disk', taskId: 'task-reused', action: 'reused' },
  ] })))
  expect(screen.getByRole('region', { name: '任务处理结果' })).toBeInTheDocument()
  expect(await screen.findByText('接口错误率超过阈值')).toBeInTheDocument()
  expect(screen.getByText('已创建任务')).toBeInTheDocument()
  expect(screen.getByText('已关联现有任务')).toBeInTheDocument()
  expect(screen.getByText('新建 1')).toBeInTheDocument()
  expect(screen.getByText('关联 1')).toBeInTheDocument()
  expect(screen.getAllByRole('link', { name: '打开任务' })).toHaveLength(2)
})

it('does not invent tasks when the form confirmed nothing', () => {
  mount(succeeded(JSON.stringify({ tasks: [] })))
  expect(screen.getByText('没有需要创建的任务')).toBeInTheDocument()
  expect(screen.queryByRole('link', { name: '打开任务' })).not.toBeInTheDocument()
})

it('offers the review node as the only retry when task creation failed', () => {
  const onOpenForm = vi.fn()
  mount({ status: 'failed', errorMessage: '任务创建失败，请核对负责人后重试' }, onOpenForm)
  expect(screen.getByText('任务没有建成')).toBeInTheDocument()
  screen.getByRole('button', { name: '回到确认节点重试' }).click()
  expect(onOpenForm).toHaveBeenCalled()
})

// D30 and D32: a run a task owns cannot retry its task creation, so neither the text (the host's
// stored one or the fallback) nor a button may send the user to that retry.
it.each([
  ['a handling run, the stored failure', 'handle', '任务创建失败，请核对负责人后重试', '这次运行是任务的处理运行，任务已按它的结果结算，不能在这里重试任务创建；请回到任务页重新开始处理。'],
  ['a handling run, the fallback', 'handle', undefined, '这次运行是任务的处理运行，任务已按它的结果结算，不能在这里重试任务创建；请回到任务页重新开始处理。'],
  ['a decomposition run, the stored failure', 'decomposition', '任务创建失败，请核对负责人后重试', '这次运行是任务的拆分运行，拆分已按它的结果结算，不能在这里重试任务创建；请回到任务页重新拆分。'],
  ['a decomposition run, the fallback', 'decomposition', undefined, '这次运行是任务的拆分运行，拆分已按它的结果结算，不能在这里重试任务创建；请回到任务页重新拆分。'],
] as const)('points %s back to its task instead of a retry', (_, taskLink, errorMessage, text) => {
  mount({ status: 'failed', ...(errorMessage === undefined ? {} : { errorMessage }) }, vi.fn(), taskLink)
  expect(screen.getByText(`确认已经保存。${text}`)).toBeInTheDocument()
  expect(screen.queryByText(/重试任务创建，不需要重新判定|请核对负责人后重试/)).not.toBeInTheDocument()
  expect(screen.queryByRole('button', { name: '回到确认节点重试' })).not.toBeInTheDocument()
})

it('keeps the fallback retry text for a run no task handles', () => {
  mount({ status: 'failed' }, vi.fn())
  expect(screen.getByText('确认已经保存；回到确认节点可以重试任务创建，不需要重新判定。')).toBeInTheDocument()
  expect(screen.getByRole('button', { name: '回到确认节点重试' })).toBeInTheDocument()
})
