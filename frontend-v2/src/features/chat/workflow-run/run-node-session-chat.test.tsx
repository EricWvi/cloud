import '@/lib/workflows/test/dom'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { fireEvent, render, screen } from '@testing-library/react'
import { I18nextProvider } from 'react-i18next'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { type Conversation, emptyConversation } from '@/components/agent-thread/model/types'
import { TooltipProvider } from '@/components/ui/tooltip'
import { createWorkflowI18n } from '@/lib/workflows/host/i18n'
import type { GraphWorkflowNodeStatus } from '@/lib/workflows/runtime'
import { RunNodeSessionChat } from './run-node-session-chat'
import { RunStarterContext } from './run-starter'
import { workflowRunTranslations } from './translations'

const stream = vi.hoisted(() => ({ conversation: null as Conversation | null, refused: false }))
vi.mock('./node-session', () => ({ useNodeSession: () => stream }))

const runI18n = createWorkflowI18n(['zh-CN'], { 'zh-CN': workflowRunTranslations['zh-CN'] })
const view = (status: GraphWorkflowNodeStatus) => render(
  <I18nextProvider i18n={runI18n}>
    <TooltipProvider>
      <RunNodeSessionChat conversation={[]} status={status} session={{ runId: 'run-1', nodeRunId: 'node-run-1' }} />
    </TooltipProvider>
  </I18nextProvider>,
)
const prompt = (status: Conversation['turns'][number]['status']): Conversation['turns'][number] => ({
  id: 'turn-1', prompt: { text: '审查这个改动', attachments: [], plugins: [] }, items: [], status, startedAt: 0,
})
const stages: NonNullable<Conversation['preparation']>['stages'] = [
  { id: 'runtime', startedAt: '2026-11-05T00:00:00.000Z', endedAt: '2026-11-05T00:00:02.000Z' },
  { id: 'plugin', startedAt: '2026-11-05T00:00:02.000Z' },
]

describe('RunNodeSessionChat before the first turn', () => {
  beforeEach(() => {
    stream.conversation = null
    stream.refused = false
  })

  it('shows the wait until the first preparation stage arrives', () => {
    view('running')
    expect(screen.getByText('正在准备…')).toBeTruthy()
    expect(screen.queryByText('正在等待 Agent 的会话…')).toBeNull()
  })

  it('shows the stages of a running node with no turn yet', () => {
    stream.conversation = { ...emptyConversation, preparation: { stages } }
    view('running')
    expect(screen.getByText('正在安装插件…（第 3/5 步）')).toBeTruthy()
    expect(screen.getByText('2.0 秒')).toBeTruthy()
  })

  it('keeps one row of stages once the turn reaches the history', () => {
    stream.conversation = { ...emptyConversation, turns: [prompt('streaming')], preparation: { stages } }
    view('running')
    expect(screen.getByText('审查这个改动')).toBeTruthy()
    expect(screen.getAllByText('正在安装插件…（第 3/5 步）')).toHaveLength(1)
  })

  it('says the session cannot be read instead of preparing', () => {
    stream.refused = true
    view('running')
    expect(screen.getByText(workflowRunTranslations['zh-CN']['workflowRun.v2.sessionRefused'])).toBeTruthy()
    expect(screen.queryByText('正在准备…')).toBeNull()
  })

  it('never shows a node that has stopped working as preparing', () => {
    view('retry_waiting')
    expect(screen.queryByText('正在准备…')).toBeNull()
  })
})

describe('RunNodeSessionChat on an interactive node', () => {
  it('starts the starter\'s composer afresh for a new attempt of the node', () => {
    const client = new QueryClient()
    const attempt = (nodeRunId: string) => (
      <I18nextProvider i18n={runI18n}>
        <QueryClientProvider client={client}>
          <TooltipProvider>
            <RunStarterContext.Provider value={true}>
              <RunNodeSessionChat
                conversation={[]}
                status="awaiting_input"
                session={{ runId: 'run-1', nodeRunId }}
                interactive={{ runId: 'run-1', nodeId: 'a', status: 'awaiting_input', session: { turns: 1, sessionEnded: false, completable: true, blockers: [] } }}
              />
            </RunStarterContext.Provider>
          </TooltipProvider>
        </QueryClientProvider>
      </I18nextProvider>
    )
    const composer = () => screen.getByRole('textbox', { name: workflowRunTranslations['zh-CN']['workflowRun.v2.interactive.composerLabel'] }) as HTMLTextAreaElement
    const shown = render(attempt('node-run-1'))
    fireEvent.change(composer(), { target: { value: '上一次尝试里没发出的话' } })
    // The same attempt keeps what the starter typed; a new attempt is a new conversation.
    shown.rerender(attempt('node-run-1'))
    expect(composer().value).toBe('上一次尝试里没发出的话')
    shown.rerender(attempt('node-run-2'))
    expect(composer().value).toBe('')
  })
})
