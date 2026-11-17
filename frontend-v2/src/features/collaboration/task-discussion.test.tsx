import { render, screen } from '@testing-library/react'
import { MemoryRouter } from 'react-router'
import { describe, expect, it } from 'vitest'
import { TaskDiscussion } from './task-discussion'
import type { TaskComment } from './tasks-api/generated'

describe('deleted comment workflow runs', () => {
  it.each(['succeeded', 'failed', 'cancelled'] as const)('hides stale output and error for %s runs', status => {
    const comment: TaskComment = {
      id: 'comment', taskId: 'task', authorId: 'member', authorName: '成员',
      body: '请检查', mentions: [], runId: '', createdAt: '2026-11-17T00:00:00Z',
      workflowRun: {
        id: 'run', workflowId: 'workflow', workflowName: '检查流程', version: 'v1',
        status, deleted: true, output: 'private output', error: 'private error',
        createdAt: '2026-11-17T00:00:00Z',
      },
    }
    render(<MemoryRouter><TaskDiscussion taskId="task" comments={[comment]} runs={[]} onRun={() => {}} /></MemoryRouter>)
    expect(screen.getByText('运行已删除')).toBeInTheDocument()
    expect(screen.queryByRole('link', { name: '查看运行' })).not.toBeInTheDocument()
    expect(screen.queryByLabelText('工作流输出')).not.toBeInTheDocument()
    expect(screen.queryByText('private output')).not.toBeInTheDocument()
    expect(screen.queryByText('private error')).not.toBeInTheDocument()
    expect(screen.queryByText('已完成，无输出内容。')).not.toBeInTheDocument()
  })
})
