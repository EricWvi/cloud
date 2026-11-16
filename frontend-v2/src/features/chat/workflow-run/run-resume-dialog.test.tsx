import '@/lib/workflows/test/dom'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { render, screen, within } from '@testing-library/react'
import { I18nextProvider } from 'react-i18next'
import { beforeEach, describe, expect, it } from 'vitest'
import type { WorkflowRunDetail } from '@/lib/workflows/api/generated'
import { createWorkflowI18n } from '@/lib/workflows/host/i18n'
import { normalizeWorkflowDefinition, type GraphWorkflowRun, type WorkflowNodeErrorDetail } from '@/lib/workflows/runtime'
import { sampleWorkflowBackend, serveWorkflows } from '@/lib/workflows/test/workflow-server'
import { chatBackend, tenantId } from '../test-backend'
import { hasResumableNode, resumeCanHelp, resumeRefusalOf } from './resume-eligibility'
import { RunResumeDialog } from './run-resume-dialog'
import { runKeys } from './server-runs'
import { serveRuns, type RunServer } from './test-run-server'
import { workflowRunTranslations } from './translations'

const node = (id: string, kind: 'start' | 'agent' | 'loop' | 'output', containerId?: string) =>
  ({ id, type: 'workflow' as const, position: { x: 0, y: 0 }, parentId: containerId, data: { kind, title: `标题-${id}`, description: '', containerId } })

// Start → A → Loop{entry → writer} → Out: A failed inside nothing; the Loop restarts as A's descendant.
const definition = normalizeWorkflowDefinition({
  id: 'wf', name: 'v1', description: '', updatedAt: new Date(0).toISOString(), viewport: { x: 0, y: 0, zoom: 1 },
  nodes: [node('start', 'start'), node('a', 'agent'), node('loop', 'loop'), node('entry', 'start', 'loop'), node('writer', 'agent', 'loop'), node('out', 'output')],
  edges: [{ id: 'e1', source: 'start', target: 'a' }, { id: 'e2', source: 'a', target: 'loop' }, { id: 'e3', source: 'entry', target: 'writer' }, { id: 'e4', source: 'loop', target: 'out' }],
})

const emptyDetail = (id: string): WorkflowRunDetail => ({
  run: { id, workflowId: 'wf', snapshotId: 's', projectId: 'p', name: '运行', status: 'failed', state: '{}', input: null, output: null, error: null, startedAt: 1, finishedAt: 2, createdAt: 1, updatedAt: 2 },
  nodes: [], scopes: [], conditionDecisions: {}, iterationConditionDecisions: {}, variables: [], failedAttempts: [],
})

describe('RunResumeDialog', () => {
  let runs: RunServer
  beforeEach(async () => {
    chatBackend()
    runs = serveRuns(tenantId, serveWorkflows(tenantId, await sampleWorkflowBackend()).backend)
    runs.runs.set('run-1', { detail: emptyDetail('run-1'), graph: '{"nodes":[],"edges":[]}', version: 'v1' })
  })

  const show = () => {
    const i18n = createWorkflowI18n(['zh-CN'], { 'zh-CN': workflowRunTranslations['zh-CN'] })
    return render(
      <I18nextProvider i18n={i18n}>
        <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
          <RunResumeDialog open tenantId={tenantId} runId="run-1" definition={definition} busy={false} onOpenChange={() => {}} onResume={async () => {}} />
        </QueryClientProvider>
      </I18nextProvider>,
    )
  }

  it('lists a restarting container once, without its members, and names the descendants after it', async () => {
    runs.preview = {
      resumable: true, reason: null, runningNodeIds: [], rollback: 'keep',
      failedNodes: [{ nodeId: 'a', nodeRunId: 'nr-a', status: 'failed', containerNodeId: null }],
      restartContainerNodeIds: ['loop'], clearNodeIds: ['a', 'entry', 'loop', 'out', 'writer'],
    }
    show()
    const dialog = await screen.findByRole('dialog', { name: '从失败处继续' })
    expect(await within(dialog).findByRole('region', { name: '从这些节点继续' })).toHaveTextContent('标题-a失败')
    expect(within(dialog).getByText('「标题-loop」将从第一轮重新开始')).toBeInTheDocument()
    expect(within(dialog).getByRole('region', { name: '随后重新执行' })).toHaveTextContent(/^随后重新执行标题-out$/)
  })

  it('names the stuck nodes and sends the user to a new run', async () => {
    runs.preview = { resumable: false, reason: 'node_stuck', runningNodeIds: ['out'], rollback: 'keep', failedNodes: [], restartContainerNodeIds: [], clearNodeIds: [] }
    show()
    const dialog = await screen.findByRole('dialog', { name: '从失败处继续' })
    expect(await within(dialog).findByRole('alert')).toHaveTextContent('节点 标题-out 停在运行中，不会再结束，这次运行无法从失败处继续；请用「再跑一次」重新运行。')
    expect(within(dialog).getByRole('button', { name: '从失败处继续' })).toBeDisabled()
  })

  it('sends a run cancelled at its unsubmitted review form to a new run', async () => {
    runs.preview = { resumable: false, reason: 'review_form', runningNodeIds: [], rollback: 'keep', failedNodes: [], restartContainerNodeIds: [], clearNodeIds: [] }
    show()
    const dialog = await screen.findByRole('dialog', { name: '从失败处继续' })
    expect(await within(dialog).findByRole('alert')).toHaveTextContent('这次运行停在确认表单上，表单还没有提交，不能从失败处继续；请用「再跑一次」重新运行。')
  })
})

describe('RunResumeDialog refusal texts', () => {
  let runs: RunServer
  beforeEach(async () => {
    chatBackend()
    runs = serveRuns(tenantId, serveWorkflows(tenantId, await sampleWorkflowBackend()).backend)
    runs.runs.set('run-1', { detail: emptyDetail('run-1'), graph: '{"nodes":[],"edges":[]}', version: 'v1' })
  })

  it.each([
    ['task_handling', '这次运行是任务的处理运行，任务已按它的结果结算，不能从失败处继续；请回到任务页重新开始处理。'],
    ['task_decomposition', '这次运行是任务的拆分运行，拆分已按它的结果结算，不能从失败处继续；请回到任务页重新拆分。'],
    ['review_form_retry', '要重跑的节点包含已经提交的确认表单，不能从失败处继续；任务创建失败了，请在确认页点击「重试任务创建」重新创建任务。'],
    ['review_form_submitted', '要重跑的节点包含已经提交的确认表单，表单不能重新填写，不能从失败处继续；请用「再跑一次」重新运行。'],
  ])('explains %s', async (reason, text) => {
    runs.preview = { resumable: false, reason, runningNodeIds: [], rollback: 'keep', failedNodes: [], restartContainerNodeIds: [], clearNodeIds: [] }
    const i18n = createWorkflowI18n(['zh-CN'], { 'zh-CN': workflowRunTranslations['zh-CN'] })
    render(
      <I18nextProvider i18n={i18n}>
        <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
          <RunResumeDialog open tenantId={tenantId} runId="run-1" definition={definition} busy={false} onOpenChange={() => {}} onResume={async () => {}} />
        </QueryClientProvider>
      </I18nextProvider>,
    )
    const dialog = await screen.findByRole('dialog', { name: '从失败处继续' })
    expect(await within(dialog).findByRole('alert')).toHaveTextContent(text)
    expect(within(dialog).getByRole('button', { name: '从失败处继续' })).toBeDisabled()
  })

  it('names the review form\'s retry button by its label', () => {
    // The form renders 「重试任务创建」; neither locale may send the user to a button that does not exist.
    expect(workflowRunTranslations['zh-CN']['workflowRun.resume.refused.review_form_retry']).toContain('「重试任务创建」')
    expect(workflowRunTranslations['en-US']['workflowRun.resume.refused.review_form_retry']).toContain('“Retry task creation”')
  })
})

describe('resume eligibility', () => {
  const detail = (over: Partial<WorkflowNodeErrorDetail>): WorkflowNodeErrorDetail =>
    ({ kind: 'session', message: '', sourceChain: [], attempt: 1, resumable: true, recordedAt: 0, ...over })

  it('offers the resume hint only where a resume of the same snapshot can help', () => {
    expect(resumeCanHelp(undefined)).toBe(true)
    expect(resumeCanHelp(detail({}))).toBe(true)
    // A configuration failure the frozen snapshot cannot change.
    expect(resumeCanHelp(detail({ kind: 'missing_agent_ref', resumable: false }))).toBe(false)
    // Not resumable as such, but the next attempt is told about the failure.
    expect(resumeCanHelp(detail({ kind: 'structured_output', resumable: false, injectsPreviousFailure: true }))).toBe(true)
  })

  it('finds a failed or cancelled node at the root, in an Iteration round or in a Loop round', () => {
    const run = (over: Partial<GraphWorkflowRun>) => ({ nodeStates: { a: { status: 'succeeded' } }, roundStates: {}, rounds: [], ...over }) as unknown as GraphWorkflowRun
    expect(hasResumableNode(run({}))).toBe(false)
    expect(hasResumableNode(run({ nodeStates: { a: { status: 'cancelled' } } }))).toBe(true)
    expect(hasResumableNode(run({ roundStates: { w: [{ status: 'succeeded' }, { status: 'failed' }] } } as never))).toBe(true)
    expect(hasResumableNode(run({ rounds: [{ nodeStates: { w: { status: 'failed' } } }] } as never))).toBe(true)
  })

  it('reads a refusal only with a known reason', () => {
    expect(resumeRefusalOf({ reason: 'run_settling', runningNodeIds: [] })).toEqual({ reason: 'run_settling', runningNodeIds: [] })
    expect(resumeRefusalOf({ reason: 'node_running', runningNodeIds: ['d', 3] })).toEqual({ reason: 'node_running', runningNodeIds: ['d'] })
    expect(resumeRefusalOf({ reason: 'something_new' })).toBeUndefined()
  })

  it('keeps the preview under the run key, so every run invalidation reads it again', () => {
    expect(runKeys.resumePreview('t', 'r').slice(0, runKeys.run('t', 'r').length)).toEqual([...runKeys.run('t', 'r')])
  })
})
