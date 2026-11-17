import '@/lib/workflows/test/dom'
import { QueryClient, QueryClientProvider, useQuery } from '@tanstack/react-query'
import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { I18nextProvider } from 'react-i18next'
import type { ReactNode } from 'react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { createWorkflowI18n } from '@/lib/workflows/host/i18n'
import { ContractError } from '@/lib/workflows/contracts'
import type { WorkflowNodeInteractiveSession } from '@/lib/workflows/runtime'
import { RunNodeSessionActions, type InteractiveNodeTarget } from './run-node-session-actions'
import { runKeys } from './server-runs'
import { RunStarterContext } from './run-starter'
import { RunTenantContext } from './run-tenant'
import { workflowRunTranslations } from './translations'

const calls = vi.hoisted(() => ({
  // Resolves to the number v2 gave the message: 1 unless a test says otherwise.
  sendNodeTurn: vi.fn(async (): Promise<number> => 1),
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

/** Renders the actions for target; `show` renders a later state of the same node in the same view. */
function view(target: Partial<InteractiveNodeTarget>, starter = true, beside?: ReactNode) {
  const client = new QueryClient()
  const tree = (next: Partial<InteractiveNodeTarget>) => (
    <I18nextProvider i18n={runI18n}>
      <QueryClientProvider client={client}>
        <RunTenantContext.Provider value="tenant-1">
          <RunStarterContext.Provider value={starter}>
            {beside}
            <RunNodeSessionActions target={{ runId: 'run-1', nodeId: 'a', status: 'awaiting_input', session: parked, ...next }} />
          </RunStarterContext.Provider>
        </RunTenantContext.Provider>
      </QueryClientProvider>
    </I18nextProvider>
  )
  const shown = render(tree(target))
  return { ...shown, show: (next: Partial<InteractiveNodeTarget>) => shown.rerender(tree(next)) }
}

/** Sends text from the composer and waits until the send was accepted. */
async function sendFromComposer(text: string) {
  const box = screen.getByRole('textbox', { name: zh['workflowRun.v2.interactive.composerLabel'] }) as HTMLTextAreaElement
  fireEvent.change(box, { target: { value: text } })
  fireEvent.click(screen.getByRole('button', { name: zh['workflowRun.v2.interactive.send'] }))
  await waitFor(() => expect(box.value).toBe(''))
  return box
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

  it('says the last message was not delivered and puts it back for a resend', async () => {
    const shown = view({})
    const box = await sendFromComposer('再改一版')
    expect(screen.queryByRole('status')).toBeNull()

    // v2 accepted the send as the starter's first message; the node then parks again reporting
    // that this message never ran.
    shown.show({ session: { ...parked, sent: 1, lastTurnError: 'not_delivered' } })
    expect(screen.getByRole('status').textContent).toContain(zh['workflowRun.v2.interactive.undelivered'])
    fireEvent.click(screen.getByRole('button', { name: zh['workflowRun.v2.interactive.restoreDraft'] }))
    expect(box.value).toBe('再改一版')
    // With the text back in the composer there is nothing left to restore.
    expect(screen.queryByRole('button', { name: zh['workflowRun.v2.interactive.restoreDraft'] })).toBeNull()

    calls.sendNodeTurn.mockResolvedValueOnce(2)
    fireEvent.click(screen.getByRole('button', { name: zh['workflowRun.v2.interactive.send'] }))
    await waitFor(() => expect(calls.sendNodeTurn).toHaveBeenCalledTimes(2))
    expect(calls.sendNodeTurn).toHaveBeenLastCalledWith('tenant-1', 'run-1', 'a', '再改一版')
    await waitFor(() => expect(box.value).toBe(''))
    // Delivered this time: the node runs the turn and parks without the notice.
    shown.show({ status: 'running', session: { ...parked, sent: 2 } })
    expect(screen.queryByRole('status')).toBeNull()
    shown.show({ session: { ...parked, turns: 2, sent: 2 } })
    expect(screen.queryByRole('status')).toBeNull()
  })

  it('keeps a notice about an earlier message away from the message just sent', async () => {
    // The first message was not delivered; the starter sends another one.
    const first = { ...parked, sent: 1, lastTurnError: 'not_delivered' }
    const shown = view({ session: first })
    expect(screen.getByRole('status')).toBeTruthy()
    calls.sendNodeTurn.mockResolvedValueOnce(2)
    const box = await sendFromComposer('第二条')

    // The run on screen is still the one read before the send, however long that lasts (the
    // next read may be slow or fail): its notice is about the first message. Offering to put
    // "第二条" back would have the starter send twice a message that may well have been delivered.
    shown.show({ session: { ...first } })
    expect(screen.queryByRole('status')).toBeNull()
    expect(screen.queryByRole('button', { name: zh['workflowRun.v2.interactive.restoreDraft'] })).toBeNull()

    // v2 could not deliver the second message either: now the notice is about it.
    shown.show({ session: { ...parked, sent: 2, lastTurnError: 'not_delivered' } })
    fireEvent.click(screen.getByRole('button', { name: zh['workflowRun.v2.interactive.restoreDraft'] }))
    expect(box.value).toBe('第二条')
  })

  it('takes each message\'s number from v2 when the run on screen has not caught up', async () => {
    const shown = view({ session: { ...parked, sent: 0 } })
    await sendFromComposer('第一条')
    // The run has not been read again, and the starter already sends the next message.
    calls.sendNodeTurn.mockResolvedValueOnce(2)
    const box = await sendFromComposer('第二条')
    // A read from between the two sends: the first message was not delivered. The notice is not
    // about the second one, which this view would otherwise put back while it runs.
    shown.show({ session: { ...parked, sent: 1, lastTurnError: 'not_delivered' } })
    expect(screen.queryByRole('status')).toBeNull()
    shown.show({ session: { ...parked, sent: 2, lastTurnError: 'not_delivered' } })
    fireEvent.click(screen.getByRole('button', { name: zh['workflowRun.v2.interactive.restoreDraft'] }))
    expect(box.value).toBe('第二条')
  })

  it('does not take another view\'s undelivered message for the one it sent meanwhile', async () => {
    // Nothing sent yet as far as this view's run goes. The starter sends from a second tab, and
    // then from here before this view reads the run again: v2 accepts this message as the second.
    const shown = view({ session: { ...parked, sent: 0 } })
    calls.sendNodeTurn.mockResolvedValueOnce(2)
    await sendFromComposer('这一条')
    // A read from before v2 claimed this message: the other tab's message, the first, was not
    // delivered. Counting from the screen would call this view's message the first as well and
    // offer to send again a message that is running.
    shown.show({ session: { ...parked, sent: 1, lastTurnError: 'not_delivered' } })
    expect(screen.queryByRole('status')).toBeNull()
    expect(screen.queryByRole('button', { name: zh['workflowRun.v2.interactive.restoreDraft'] })).toBeNull()
    // This view's message was delivered.
    shown.show({ session: { ...parked, turns: 2, sent: 2 } })
    expect(screen.queryByRole('status')).toBeNull()
  })

  it('does not remember a message v2 refused', async () => {
    const shown = view({})
    const box = await sendFromComposer('第一条')
    shown.show({ session: { ...parked, sent: 1, lastTurnError: 'not_delivered' } })
    // The next message is refused outright: it was never the starter's second message.
    calls.sendNodeTurn.mockRejectedValueOnce(new ContractError('workflow_node_not_awaiting_input', {}))
    fireEvent.change(box, { target: { value: '第二条' } })
    fireEvent.click(screen.getByRole('button', { name: zh['workflowRun.v2.interactive.send'] }))
    await waitFor(() => expect(toastError).toHaveBeenCalledTimes(1))
    expect(box.value).toBe('第二条')
    // The notice is still about the first message, so that is the one put back.
    fireEvent.change(box, { target: { value: '' } })
    fireEvent.click(screen.getByRole('button', { name: zh['workflowRun.v2.interactive.restoreDraft'] }))
    expect(box.value).toBe('第一条')
  })

  it('offers nothing to put back when its only send was refused', async () => {
    const shown = view({ session: { ...parked, sent: 0 } })
    calls.sendNodeTurn.mockRejectedValueOnce(new ContractError('workflow_node_not_awaiting_input', {}))
    const box = screen.getByRole('textbox', { name: zh['workflowRun.v2.interactive.composerLabel'] }) as HTMLTextAreaElement
    fireEvent.change(box, { target: { value: '没发出去' } })
    fireEvent.click(screen.getByRole('button', { name: zh['workflowRun.v2.interactive.send'] }))
    await waitFor(() => expect(toastError).toHaveBeenCalledTimes(1))
    fireEvent.change(box, { target: { value: '' } })
    // Another view's first message was not delivered: true, but this view sent nothing.
    shown.show({ session: { ...parked, sent: 1, lastTurnError: 'not_delivered' } })
    expect(screen.getByRole('status').textContent).toContain(zh['workflowRun.v2.interactive.undelivered'])
    expect(screen.queryByRole('button', { name: zh['workflowRun.v2.interactive.restoreDraft'] })).toBeNull()
  })

  it('never puts back a message other than the undelivered one', async () => {
    const shown = view({})
    await sendFromComposer('第一条')
    // This view's message was delivered. The starter then sent another one from elsewhere (a
    // second tab), and that one was not: the notice is true, but this view has the wrong text.
    shown.show({ session: { ...parked, turns: 2, sent: 2, lastTurnError: 'not_delivered' } })
    expect(screen.getByRole('status').textContent).toContain(zh['workflowRun.v2.interactive.undelivered'])
    expect(screen.queryByRole('button', { name: zh['workflowRun.v2.interactive.restoreDraft'] })).toBeNull()
  })

  it('reports an undelivered message without a resend when this view no longer has its text', () => {
    view({ session: { ...parked, sent: 1, lastTurnError: 'not_delivered' } })
    expect(screen.getByRole('status').textContent).toContain(zh['workflowRun.v2.interactive.undelivered'])
    expect(screen.queryByRole('button', { name: zh['workflowRun.v2.interactive.restoreDraft'] })).toBeNull()
    expect(screen.getByRole('textbox', { name: zh['workflowRun.v2.interactive.composerLabel'] })).toBeTruthy()
  })

  it('says an undelivered message cannot be resent once the session has ended', async () => {
    const shown = view({})
    await sendFromComposer('再改一版')
    // The session ended underneath the message, so there is nowhere to send it again.
    shown.show({ session: { ...parked, sent: 1, sessionEnded: true, lastTurnError: 'not_delivered' } })
    expect(screen.getByRole('status').textContent).toContain(zh['workflowRun.v2.interactive.undeliveredEnded'])
    expect(screen.queryByRole('textbox')).toBeNull()
    expect(screen.queryByRole('button', { name: zh['workflowRun.v2.interactive.restoreDraft'] })).toBeNull()
  })

  it('shows no undelivered notice to members or while the node runs a turn', () => {
    view({ session: { ...parked, sent: 1, lastTurnError: 'not_delivered' } }, false)
    expect(screen.queryByRole('status')).toBeNull()
    view({ status: 'running', session: { ...parked, sent: 1, lastTurnError: 'not_delivered' } })
    expect(screen.queryByRole('status')).toBeNull()
  })

  it('reads the run again after a stop, a completion and a refused send', async () => {
    const reads = vi.fn(async () => 0)
    function RunQuery() {
      useQuery({ queryKey: runKeys.run('tenant-1', 'run-1'), queryFn: reads })
      return null
    }
    const shown = view({ status: 'running' }, true, <RunQuery />)
    await waitFor(() => expect(reads).toHaveBeenCalledTimes(1))
    fireEvent.click(screen.getByRole('button', { name: zh['workflowRun.v2.interactive.stop'] }))
    await waitFor(() => expect(reads).toHaveBeenCalledTimes(2))
    shown.show({})
    fireEvent.click(screen.getByRole('button', { name: zh['workflowRun.v2.interactive.complete'] }))
    await waitFor(() => expect(reads).toHaveBeenCalledTimes(3))
    calls.sendNodeTurn.mockRejectedValueOnce(new ContractError('workflow_node_not_awaiting_input', {}))
    const box = screen.getByRole('textbox', { name: zh['workflowRun.v2.interactive.composerLabel'] }) as HTMLTextAreaElement
    fireEvent.change(box, { target: { value: '再改一版' } })
    fireEvent.click(screen.getByRole('button', { name: zh['workflowRun.v2.interactive.send'] }))
    await waitFor(() => expect(reads).toHaveBeenCalledTimes(4))
    // A refused send keeps the text for another try.
    expect(box.value).toBe('再改一版')
  })
})
