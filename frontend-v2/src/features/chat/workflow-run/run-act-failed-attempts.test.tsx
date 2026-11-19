import '@/lib/workflows/test/dom'
import { render, screen } from '@testing-library/react'
import { I18nextProvider } from 'react-i18next'
import { describe, expect, it } from 'vitest'
import { createWorkflowI18n, type WorkflowLocale } from '@/lib/workflows/host/i18n'
import type { WorkflowNodeAttemptFailure } from '@/lib/workflows/runtime'
import { RunActFailedAttempts } from './run-act-failed-attempts'
import { workflowRunTranslations } from './translations'

const attempt: WorkflowNodeAttemptFailure = {
  nodeRunId: 'nr-1',
  attempt: 1,
  kind: 'session',
  errorMessage: 'lost the Agent turn',
  sourceChain: [],
  recordedAt: 0,
  sessionId: 'session-1',
  replacedBy: 'automatic_retry',
}

// The instance starts in zh-CN, as the page does; the en-US case switches it.
const view = (locale: WorkflowLocale, attempts: WorkflowNodeAttemptFailure[] = [attempt]) => {
  const i18n = createWorkflowI18n(['zh-CN', 'en-US'], workflowRunTranslations)
  void i18n.changeLanguage(locale)
  return render(
    <I18nextProvider i18n={i18n}>
      <RunActFailedAttempts attempts={attempts} runId="run-1" />
    </I18nextProvider>,
  )
}

describe('RunActFailedAttempts', () => {
  it.each([
    ['zh-CN', '查看会话 · session-1'],
    ['en-US', 'View session · session-1'],
  ] as const)('labels the replaced attempt’s session in %s', (locale, label) => {
    view(locale)
    expect(screen.getByText(label)).toBeTruthy()
  })

  it.each([
    ['zh-CN', '运行的发起人已不可用'],
    ['en-US', 'Run starter unavailable'],
  ] as const)('names an attempt refused because the starter left in %s, not as a session failure', (locale, label) => {
    // The attempt a resume replaced after the starter came back to the workspace.
    view(locale, [{ ...attempt, kind: 'starter_unavailable', sessionId: undefined, replacedBy: 'manual_resume' }])
    expect(screen.getByText(label, { exact: false })).toBeTruthy()
    expect(screen.queryByText('智能体会话失败', { exact: false })).toBeNull()
  })
})
