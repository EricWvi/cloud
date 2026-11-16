import { useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { useTranslation } from 'react-i18next'
import { ContractError } from '@/lib/workflows/contracts'
import { localizeContractError } from '@/lib/workflows/host/contract-error'
import { Button, Textarea, toast } from '@/lib/workflows/host/ui'
import type { GraphWorkflowNodeStatus, WorkflowNodeInteractiveSession } from '@/lib/workflows/runtime'
import { useIsRunStarter } from './run-starter'
import { useRunTenant } from './run-tenant'
import { completeNode, runKeys, sendNodeTurn, stopNodeTurn } from './server-runs'
import { workflowRunTranslations } from './translations'

/** The longest message v2-server accepts for one turn. */
const maxTurnLength = 16000

/** The parked interactive node the actions act on. */
export interface InteractiveNodeTarget {
  runId: string
  nodeId: string
  status: GraphWorkflowNodeStatus
  session?: WorkflowNodeInteractiveSession
}

const runErrorKeys: Readonly<Record<string, string>> = workflowRunTranslations['zh-CN']

/**
 * Refusals in the run's own words: the interactive codes live in the run view's strings, which
 * the shared contract-error helper does not read.
 */
function refusal(error: unknown, t: (key: string, options?: Record<string, unknown>) => string): string {
  if (error instanceof ContractError) {
    if (error.code === 'workflow_node_incomplete' && typeof error.params.reason === 'string') {
      const key = `workflowRun.v2.interactive.blocker.${error.params.reason}`
      if (key in runErrorKeys) return t(key)
    }
    const key = `errors.${error.code}`
    if (key in runErrorKeys) return t(key)
  }
  return localizeContractError(error, t as never)
}

/**
 * The starter's controls for an interactive Agent node, below its session: a composer and "完成"
 * while the node waits for input, "停止本轮" while it runs a turn. v2 implements desktop's legacy
 * end_turn completion only, so the starter confirms completion and the note says so; stopping a
 * turn ends the node's session (the pinned Node has no other way to stop), so the note warns that
 * the conversation cannot continue after it. Everyone else reads the transcript.
 */
export function RunNodeSessionActions({ target }: { target: InteractiveNodeTarget }) {
  const { t } = useTranslation()
  const tenantId = useRunTenant()
  const starter = useIsRunStarter()
  const queryClient = useQueryClient()
  const [draft, setDraft] = useState('')
  const [busy, setBusy] = useState(false)
  const { runId, nodeId, status, session } = target

  if (!starter) {
    return <p className="px-1 py-2 text-xs text-muted-foreground">{t('workflowRun.v2.interactive.readOnly')}</p>
  }

  const act = async (action: () => Promise<void>): Promise<boolean> => {
    setBusy(true)
    try {
      await action()
      return true
    } catch (error) {
      toast.error(refusal(error, t))
      return false
    } finally {
      setBusy(false)
      void queryClient.invalidateQueries({ queryKey: runKeys.run(tenantId, runId) })
    }
  }

  const awaiting = status === 'awaiting_input'
  const ended = session?.sessionEnded === true
  const text = draft.trim()
  const canSend = awaiting && !ended && !busy && text !== '' && draft.length <= maxTurnLength
  const send = async () => {
    if (!canSend) return
    if (await act(() => sendNodeTurn(tenantId, runId, nodeId, draft))) setDraft('')
  }
  const blocker = session?.blocker
  // Once the session has ended nobody can ask the Agent for another answer, so a blocker that
  // would otherwise say "keep talking" says what is left instead.
  const blockerKey = blocker !== undefined && ended && blocker.reason === 'structured_output' ? 'structured_output_ended' : blocker?.reason
  const blockerText = blocker === undefined ? undefined : t(`workflowRun.v2.interactive.blocker.${blockerKey}`, { defaultValue: blocker.detail })

  return (
    <div className="flex w-full flex-col gap-2 border-t pt-2" data-interactive-actions="">
      <p className="text-[11px] leading-4 text-muted-foreground">{t('workflowRun.v2.interactive.partialNote')}</p>
      {awaiting && !ended && (
        <div className="flex items-end gap-2">
          <Textarea
            aria-label={t('workflowRun.v2.interactive.composerLabel')}
            placeholder={t('workflowRun.v2.interactive.composerPlaceholder')}
            className="max-h-40 min-h-16 flex-1 resize-none text-sm"
            value={draft}
            maxLength={maxTurnLength}
            disabled={busy}
            onChange={(event) => setDraft(event.target.value)}
            onKeyDown={(event) => {
              event.stopPropagation()
              if (event.key === 'Enter' && !event.shiftKey && !event.nativeEvent.isComposing) {
                event.preventDefault()
                void send()
              }
            }}
          />
          <Button type="button" size="sm" disabled={!canSend} onClick={() => void send()}>
            {t('workflowRun.v2.interactive.send')}
          </Button>
        </div>
      )}
      {awaiting && ended && (
        <p className="text-xs text-muted-foreground">
          {t(session?.completable === true ? 'workflowRun.v2.interactive.sessionEnded' : 'workflowRun.v2.interactive.sessionEndedBlocked')}
        </p>
      )}
      {awaiting && blockerText !== undefined && <p className="text-xs text-warning">{blockerText}</p>}
      <div className="flex items-center justify-end gap-2">
        {status === 'running' && (
          <Button type="button" variant="outline" size="sm" disabled={busy} onClick={() => void act(() => stopNodeTurn(tenantId, runId, nodeId))}>
            {t('workflowRun.v2.interactive.stop')}
          </Button>
        )}
        {awaiting && (
          <Button
            type="button"
            size="sm"
            variant={session?.completable === true ? 'default' : 'outline'}
            disabled={busy || session?.completable !== true}
            onClick={() => void act(() => completeNode(tenantId, runId, nodeId))}
          >
            {t('workflowRun.v2.interactive.complete')}
          </Button>
        )}
      </div>
    </div>
  )
}
