import '@/lib/workflows/test/dom'
import { render, screen } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { I18nextProvider } from 'react-i18next'
import { MemoryRouter } from 'react-router'
import { describe, expect, it, vi } from 'vitest'
import { TooltipProvider } from '@/components/ui/tooltip'
import { createWorkflowI18n } from '@/lib/workflows/host/i18n'
import type { GraphWorkflowRun } from '@/lib/workflows/runtime'
import { RunTheater } from './run-theater'
import { workflowRunTranslations } from './translations'

// The review form is its own module with its own tests; here only what the theater hands it counts.
vi.mock('./run-form', () => ({
  RunForm: ({ runStatus, taskLink }: { runStatus: string; taskLink?: string }) => <p>{`确认表单 ${runStatus} ${taskLink ?? '无任务'}`}</p>,
}))

vi.mock('./run-tasks-stage', () => ({
  RunTasksStage: ({ taskLink }: { taskLink?: string }) => <p>{`任务阶段 ${taskLink ?? '无任务'}`}</p>,
}))

const i18n = createWorkflowI18n(['zh-CN'], { 'zh-CN': workflowRunTranslations['zh-CN'] })

function formRun(taskLink?: 'handle' | 'decomposition'): GraphWorkflowRun {
  const at = '2026-11-16T00:00:00Z'
  return {
    id: 'run-1', projectId: 'p', definitionId: 'w', name: '巡检', status: 'failed', taskLink,
    ...(taskLink === undefined ? {} : { taskId: 'task-1' }),
    definitionSnapshot: {
      id: 'w', name: '巡检', description: '', updatedAt: at, viewport: { x: 0, y: 0, zoom: 1 },
      nodes: [
        { id: 'review', type: 'form', position: { x: 0, y: 0 }, data: { kind: 'form', title: '确认问题' } },
        { id: 'tasks', type: 'createTasks', position: { x: 200, y: 0 }, data: { kind: 'createTasks', title: '创建任务' } },
      ],
      edges: [],
    },
    nodeStates: { review: { status: 'succeeded' }, tasks: { status: 'failed' } },
    openHitls: [], createdAt: at, updatedAt: at,
  } as unknown as GraphWorkflowRun
}

describe('RunTheater', () => {
  // D30: the form and the task stage drop the task creation retry for a handling run only if the
  // theater tells them which task link owns the run.
  const view = (taskLink: 'handle' | 'decomposition' | undefined, focusNodeId: string) => render(
    <I18nextProvider i18n={i18n}>
      <QueryClientProvider client={new QueryClient()}>
        <MemoryRouter>
          <TooltipProvider>
            <RunTheater tenantId="t" run={formRun(taskLink)} focusNodeId={focusNodeId} onFocusNode={() => {}} artifacts={[]}
              conversationByNodeId={new Map()} revealedArtifactId={null} onShowOverview={() => {}} />
          </TooltipProvider>
        </MemoryRouter>
      </QueryClientProvider>
    </I18nextProvider>,
  )

  it.each([['handle'], ['decomposition'], [undefined]] as const)('hands the review form the run\'s task link: %s', (taskLink) => {
    view(taskLink, 'review')
    expect(screen.getByText(`确认表单 failed ${taskLink ?? '无任务'}`)).toBeInTheDocument()
  })

  it.each([['handle'], ['decomposition'], [undefined]] as const)('hands the task stage the run\'s task link: %s', (taskLink) => {
    view(taskLink, 'tasks')
    expect(screen.getByText(`任务阶段 ${taskLink ?? '无任务'}`)).toBeInTheDocument()
  })
})
