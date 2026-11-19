import '@/lib/workflows/test/dom'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { render, screen } from '@testing-library/react'
import { createRef, type ReactElement } from 'react'
import { I18nextProvider } from 'react-i18next'
import { describe, expect, it, vi } from 'vitest'
import type { WorkflowNodeRun, WorkflowRunDetail } from '@/lib/workflows/api/generated'
import { createWorkflowI18n } from '@/lib/workflows/host/i18n'
import { normalizeWorkflowDefinition, type GraphWorkflowNodeState, type WorkflowNodeData } from '@/lib/workflows/runtime'
import { RunStarterContext } from './run-starter'
import { RunTheaterActCard } from './run-theater-act-card'
import { RunTheaterPathRail } from './run-theater-path-rail'
import { toGraphWorkflowRun } from './server-run-adapter'
import { workflowRunTranslations } from './translations'

// The card names the node's Agent from the catalog; this test needs no executor directory.
vi.mock('./demo-host', async (original) => ({ ...(await original<typeof import('./demo-host')>()), useAgentCatalog: () => [] }))

const zh = workflowRunTranslations['zh-CN']
const runI18n = createWorkflowI18n(['zh-CN'], { 'zh-CN': zh })
const view = (ui: ReactElement) =>
  render(
    <I18nextProvider i18n={runI18n}>
      <QueryClientProvider client={new QueryClient()}>
        <RunStarterContext.Provider value>{ui}</RunStarterContext.Provider>
      </QueryClientProvider>
    </I18nextProvider>,
  )

const agent: WorkflowNodeData = {
  kind: 'agent',
  title: '起草',
  description: '',
  agentConfig: {
    schemaVersion: 3,
    executor: { agentCli: 'official/ora-space.opencode', modelId: 'fixture' },
    roleId: '',
    skills: [],
    mcps: [],
    prompt: '起草',
    interactive: true,
    retry: { enabled: false, maxRetries: 0, initialDelaySeconds: 0 },
  },
}

const summary = { turns: 1, sessionEnded: false, completable: true, blockers: [], answer: '初稿' }

function card(state: GraphWorkflowNodeState) {
  return view(
    <RunTheaterActCard data={agent} state={state} runId="run" nodeId="a" live conversationEnabled conversationOpen />,
  )
}

describe('RunTheaterActCard for interactive nodes', () => {
  it('offers the starter 停止本轮 and the partial note only when the server reports an interactive session', () => {
    card({ status: 'running', sessionId: 'session-1', nodeRunId: 'nr', interactiveSession: { ...summary, completable: false } })
    expect(screen.getByRole('button', { name: zh['workflowRun.v2.interactive.stop'] })).toBeTruthy()
    expect(screen.getByText(zh['workflowRun.v2.interactive.partialNote'])).toBeTruthy()
  })

  it('treats a Loop member with interactive mode as the automatic node v2 runs it as', () => {
    // v2 runs a Loop member automatically and records no session summary for it, whatever its
    // agentConfig says, so stopping a turn would only be refused.
    card({ status: 'running', sessionId: 'session-1', nodeRunId: 'nr' })
    expect(screen.queryByRole('button', { name: zh['workflowRun.v2.interactive.stop'] })).toBeNull()
    expect(screen.queryByText(zh['workflowRun.v2.interactive.partialNote'])).toBeNull()
  })

  it('labels a parked session as awaiting input', () => {
    card({ status: 'awaiting_input', sessionId: 'session-1', nodeRunId: 'nr', interactiveSession: summary })
    expect(screen.getAllByText(zh['workflowRun.v2.interactive.awaitingBadge']).length).toBeGreaterThan(0)
    expect(screen.getByRole('button', { name: zh['workflowRun.v2.interactive.complete'] })).toBeTruthy()
  })
})

const definition = normalizeWorkflowDefinition({
  id: 'wf',
  name: 'v1',
  description: '',
  updatedAt: new Date(0).toISOString(),
  viewport: { x: 0, y: 0, zoom: 1 },
  nodes: [
    { id: 'start', type: 'workflow', position: { x: 0, y: 0 }, data: { kind: 'start', title: '开始', description: '' } },
    { id: 'a', type: 'workflow', position: { x: 1, y: 0 }, data: agent },
  ],
  edges: [{ id: 'e1', source: 'start', target: 'a' }],
})

function nodeRun(over: Partial<WorkflowNodeRun>): WorkflowNodeRun {
  return {
    id: 'nr', runId: 'run', scopeId: 'root:run', nodeId: 'start', nodeType: 'start', sessionId: null, status: 'succeeded',
    input: null, output: null, error: null, payload: null, iteration: null, startedAt: 1_000, finishedAt: 2_000, createdAt: 1_000, updatedAt: 2_000,
    ...over,
  }
}

function rail(a: Partial<WorkflowNodeRun>) {
  const detail: WorkflowRunDetail = {
    run: {
      id: 'run', workflowId: 'wf', snapshotId: 'snap', projectId: 'p', name: '运行', status: 'awaitingInput', startedByName: 'Alice', state: '{"current_nodes":["a"]}',
      input: '需求', output: null, error: null, startedAt: 1_000, finishedAt: null, createdAt: 500, updatedAt: 2_000,
    },
    nodes: [nodeRun({ id: 'start-run' }), nodeRun({ id: 'a-run', nodeId: 'a', nodeType: 'agent', sessionId: 'session-1', finishedAt: null, ...a })],
    scopes: [],
    conditionDecisions: {},
    variables: [],
    failedAttempts: [],
    iterationConditionDecisions: {},
    startedBy: 'user-1',
  }
  const run = toGraphWorkflowRun(detail, definition)
  return view(
    <RunTheaterPathRail
      run={run}
      primaryId="a"
      activeIds={['a']}
      openHitls={[]}
      artifactCountByNode={{}}
      showResultAct={false}
      pathRailRef={createRef()}
      onFocusNode={() => {}}
      onExpandHitl={() => {}}
    />,
  )
}

describe('RunTheaterPathRail for interactive nodes', () => {
  it('marks a parked interactive node as awaiting input', () => {
    rail({ status: 'pending', payload: JSON.stringify({ interactive: { turns: 1, session_ended: false, completable: true, answer: '初稿' } }) })
    expect(screen.getByText(zh['workflowRun.v2.interactive.awaitingBadge'])).toBeTruthy()
    expect(screen.getByLabelText(`起草: ${zh['workflowRun.v2.interactive.awaitingBadge']}`)).toBeTruthy()
  })

  it('does not mark a running node without a session summary', () => {
    rail({ status: 'running' })
    expect(screen.queryByText(zh['workflowRun.v2.interactive.awaitingBadge'])).toBeNull()
  })
})
