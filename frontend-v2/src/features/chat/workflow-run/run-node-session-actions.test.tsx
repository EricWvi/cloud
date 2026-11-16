import '@/lib/workflows/test/dom'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { I18nextProvider } from 'react-i18next'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { createWorkflowI18n } from '@/lib/workflows/host/i18n'
import { ContractError } from '@/lib/workflows/contracts'
import type { WorkflowNodeInteractiveSession } from '@/lib/workflows/runtime'
import { RunNodeSessionActions, type InteractiveNodeTarget } from './run-node-session-actions'
import { RunStarterContext } from './run-starter'
import { RunTenantContext } from './run-tenant'
import { workflowRunTranslations } from './translations'

const calls = vi.hoisted(() => ({
  sendNodeTurn: vi.fn(async () => {}),
  stopNodeTurn: vi.fn(async () => {}),
  completeNode: vi.fn(async () => {}),
}))
vi.mock('./server-runs', async (original) => ({ ...(await original<typeof import('./server-runs')>()), ...calls }))
const toastError = vi.hoisted(() => vi.fn())
vi.mock('@/lib/workflows/host/ui', async (original) => {
  const ui = await original<typeof import('@/lib/workflows/host/ui')>()
  return { ...ui, toast: { ...ui.toast, error: toastError } }
})

const zh = workflowRunTranslations['zh-CN']
const runI18n = createWorkflowI18n(['zh-CN'], { 'zh-CN': zh })
const parked: WorkflowNodeInteractiveSession = { turns: 1, sessionEnded: false, completable: true, answer: '初稿' }

function view(target: Partial<InteractiveNodeTarget>, starter = true) {
  const full: InteractiveNodeTarget = { runId: 'run-1', nodeId: 'a', status: 'awaiting_input', session: parked, ...target }
  return render(
    <I18nextProvider i18n={runI18n}>
      <QueryClientProvider client={new QueryClient()}>
        <RunTenantContext.Provider value="tenant-1">
          <RunStarterContext.Provider value={starter}>
            <RunNodeSessionActions target={full} />
          </RunStarterContext.Provider>
        </RunTenantContext.Provider>
      </QueryClientProvider>
    </I18nextProvider>,
  )
}

describe('RunNodeSessionActions', () => {
  beforeEach(() => {
    for (const fn of Object.values(calls)) fn.mockClear()
    toastError.mockClear()
  })

  it('lets the starter reply in the node session and says the completion is simplified', async () => {
    view({})
    expect(screen.getByText(zh['workflowRun.v2.interactive.partialNote'])).toBeTruthy()
    const box = screen.getByRole('textbox', { name: zh['workflowRun.v2.interactive.composerLabel'] })
    fireEvent.change(box, { target: { value: '再改一版' } })
    fireEvent.keyDown(box, { key: 'Enter' })
    await waitFor(() => expect(calls.sendNodeTurn).toHaveBeenCalledWith('tenant-1', 'run-1', 'a', '再改一版'))
    await waitFor(() => expect((box as HTMLTextAreaElement).value).toBe(''))
    expect(screen.queryByRole('button', { name: zh['workflowRun.v2.interactive.stop'] })).toBeNull()
  })

  it('completes a completable node', async () => {
    view({})
    fireEvent.click(screen.getByRole('button', { name: zh['workflowRun.v2.interactive.complete'] }))
    await waitFor(() => expect(calls.completeNode).toHaveBeenCalledWith('tenant-1', 'run-1', 'a'))
  })

  it('offers only "停止本轮" while a turn runs', async () => {
    view({ status: 'running' })
    expect(screen.queryByRole('textbox')).toBeNull()
    expect(screen.queryByRole('button', { name: zh['workflowRun.v2.interactive.complete'] })).toBeNull()
    fireEvent.click(screen.getByRole('button', { name: zh['workflowRun.v2.interactive.stop'] }))
    await waitFor(() => expect(calls.stopNodeTurn).toHaveBeenCalledWith('tenant-1', 'run-1', 'a'))
  })

  it('keeps completion closed and explains why while the node is blocked', () => {
    view({ session: { turns: 1, sessionEnded: false, completable: false, blocker: { reason: 'structured_output', detail: 'missing approved' } } })
    expect(screen.getByText(zh['workflowRun.v2.interactive.blocker.structured_output'])).toBeTruthy()
    expect((screen.getByRole('button', { name: zh['workflowRun.v2.interactive.complete'] }) as HTMLButtonElement).disabled).toBe(true)
  })

  it('takes no more messages once the session was stopped', () => {
    view({ session: { turns: 2, sessionEnded: true, completable: true, answer: '初稿' } })
    expect(screen.queryByRole('textbox')).toBeNull()
    expect(screen.getByText(zh['workflowRun.v2.interactive.sessionEnded'])).toBeTruthy()
    expect((screen.getByRole('button', { name: zh['workflowRun.v2.interactive.complete'] }) as HTMLButtonElement).disabled).toBe(false)
  })

  it('does not offer completion or another message once the session ended without an acceptable answer', () => {
    view({ session: { turns: 1, sessionEnded: true, completable: false, blocker: { reason: 'no_completed_turn', detail: 'no turn ended normally' } } })
    expect(screen.getByText(zh['workflowRun.v2.interactive.sessionEndedBlocked'])).toBeTruthy()
    expect(screen.queryByText(zh['workflowRun.v2.interactive.sessionEnded'])).toBeNull()
    expect(screen.getByText(zh['workflowRun.v2.interactive.blocker.no_completed_turn'])).toBeTruthy()
    expect((screen.getByRole('button', { name: zh['workflowRun.v2.interactive.complete'] }) as HTMLButtonElement).disabled).toBe(true)
  })

  it('does not tell the starter to keep talking about a structured output once the session ended', () => {
    view({ session: { turns: 2, sessionEnded: true, completable: false, answer: '初稿', blocker: { reason: 'structured_output', detail: 'missing approved' } } })
    expect(screen.getByText(zh['workflowRun.v2.interactive.blocker.structured_output_ended'])).toBeTruthy()
    expect(screen.queryByText(zh['workflowRun.v2.interactive.blocker.structured_output'])).toBeNull()
    expect(screen.queryByRole('textbox')).toBeNull()
  })

  it('shows other members the transcript read-only', () => {
    view({}, false)
    expect(screen.getByText(zh['workflowRun.v2.interactive.readOnly'])).toBeTruthy()
    expect(screen.queryByRole('textbox')).toBeNull()
    expect(screen.queryByRole('button')).toBeNull()
  })

  it('reports a refusal in the run view\'s words', async () => {
    calls.completeNode.mockRejectedValueOnce(new ContractError('run_owned', {}))
    view({})
    fireEvent.click(screen.getByRole('button', { name: zh['workflowRun.v2.interactive.complete'] }))
    await waitFor(() => expect(toastError).toHaveBeenCalledWith(zh['errors.run_owned']))
  })
})
