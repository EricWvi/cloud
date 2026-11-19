import { expect, test, type Page } from '@playwright/test'
import { randomUUID } from 'node:crypto'
import { Client } from 'pg'
import { ownerName, signIn, signInAsInvitedMember } from './login.ts'

const database = () => new Client({ connectionString: process.env.E2E_DATABASE_URL ?? 'postgres://ora:ora-local@127.0.0.1:55432/ora_v2_e2e?sslmode=disable' })

// With prepared Node assets (`task v2:node:prepare`, then V2_NODE_ASSETS_DIR) v2-server runs real
// Agents on the fixture model; without them, as in CI, it has no Agent to run.
const realAgents = !!process.env.V2_NODE_ASSETS_DIR

/** Start → 起草 → 审阅 → 输出: 审阅 reads 起草's answer through the variable pool. */
const chain = JSON.stringify({
  nodes: [
    { id: 'start', type: 'workflow', position: { x: 0, y: 0 }, data: { kind: 'start', title: '开始' } },
    { id: 'draft', type: 'workflow', position: { x: 320, y: 0 }, data: { kind: 'agent', title: '起草', agentConfig: { executor: { agentCli: 'official/ora-space.opencode', modelId: 'fixture' }, prompt: '根据需求起草方案：{{#start.input#}}' } } },
    { id: 'review', type: 'workflow', position: { x: 640, y: 0 }, data: { kind: 'agent', title: '审阅', agentConfig: { executor: { agentCli: 'official/ora-space.opencode', modelId: 'fixture' }, prompt: '审阅上一步的方案：{{#draft.output#}}' } } },
    { id: 'out', type: 'workflow', position: { x: 960, y: 0 }, data: { kind: 'output', title: '输出', outputs: [{ name: 'draft', variableSelector: ['draft', 'output'] }, { name: 'review', variableSelector: ['review', 'output'] }] } },
  ],
  edges: [
    { id: 'e1', source: 'start', target: 'draft' },
    { id: 'e2', source: 'draft', target: 'review' },
    { id: 'e3', source: 'review', target: 'out' },
  ],
})

/**
 * Screenshots wait for the theater's enter and expand animations, or they catch a half-open card.
 * Endless ones (status pulses) and scroll-driven ones (scroll fades) never settle and are skipped.
 */
async function screenshot(page: Page, path: string) {
  await page.waitForFunction(() =>
    document
      .getAnimations()
      .filter((a) => a.timeline === document.timeline && a.effect?.getComputedTiming().endTime !== Infinity)
      .every((a) => a.playState !== 'running'),
  )
  await page.screenshot({ path })
}

test('workflow runs: v2-server executes a published workflow started from chat', async ({ page }, testInfo) => {
  test.setTimeout(180_000)
  await page.goto('/chat')
  await signIn(page)

  // Publish the chain on v2-server; the workflow and its run take names no earlier run of this spec used.
  const tenantId = ((await (await page.request.get('/api/v1/me/tenants')).json()) as { items: { id: string }[] }).items[0]!.id
  const suffix = Date.now().toString(36)
  const name = `两段 Agent ${suffix}`
  const runName = `对账接口方案 ${suffix}`
  const created = await page.request.post(`/api/v2/tenants/${tenantId}/workflows`, { data: { name, graph: chain } })
  expect(created.status()).toBe(201)
  const workflowId = ((await created.json()) as { draft: { workflowId: string } }).draft.workflowId
  expect((await page.request.post(`/api/v2/tenants/${tenantId}/workflows/${workflowId}/publish`, { data: { version: 'v1' } })).ok()).toBe(true)

  // The visible input-area entry offers published workflows; the run opens pending, and
  // follows the run's invalidation stream while it has not ended.
  await page.reload()
  await page.getByRole('button', { name: '选择项目' }).click()
  await page.getByRole('option', { name: 'Demo', exact: true }).click()
  const runEvents = page.waitForResponse((r) => /\/workflow-runs\/[^/]+\/events$/.test(new URL(r.url()).pathname))
  const demo = page.getByRole('complementary', { name: '聊天记录' }).getByRole('region', { name: '项目 Demo' })
  await page.getByRole('button', { name: '运行工作流', exact: true }).click()
  await screenshot(page, testInfo.outputPath('workflow-entry.png'))
  await page.getByRole('menuitem', { name: new RegExp(name) }).click()
  const dialog = page.getByRole('dialog', { name: '运行工作流' })
  await dialog.getByLabel('运行名称').fill(runName)
  const injectLastFailure = dialog.getByRole('checkbox', { name: '节点重跑时把上次失败原因告诉智能体' })
  await expect(injectLastFailure).toBeVisible()
  await expect(injectLastFailure).toBeChecked()
  await screenshot(page, testInfo.outputPath('run-dialog-inject-default.png'))
  await dialog.getByRole('button', { name: '创建运行' }).click()
  const pane = page.getByRole('region', { name: '工作流运行' })
  await expect(pane.getByRole('heading', { name: runName })).toBeVisible()
  await expect(pane.getByText('演示运行')).toBeHidden()
  const row = (status: string) => demo.getByRole('button', { name: `工作流运行 ${runName}（${status}）` })
  await expect(row('未运行')).toBeVisible()
  expect((await runEvents).headers()['content-type']).toBe('text/event-stream')

  // 再跑一次 creates a pending run of the same version; the sidebar lists it at once, although
  // every run it held had ended and it had stopped polling. The pending run is deleted again: one
  // left behind would keep the list polling and hide this check from the next spec run.
  async function runAgain() {
    await pane.getByRole('button', { name: '再跑一次' }).click()
    await expect(row('未运行')).toBeVisible({ timeout: 3_000 })
    await screenshot(page, testInfo.outputPath('rerun-sidebar.png'))
    const pending = row('未运行').locator('..')
    await pending.hover()
    await pending.getByRole('button', { name: `${runName} 的更多操作` }).click()
    await page.getByRole('menuitem', { name: '删除' }).click()
    const confirm = page.getByRole('dialog', { name: /删除运行/ })
    await confirm.getByRole('button', { name: '删除' }).click()
    await expect(confirm).toBeHidden()
    await expect(row('未运行')).toBeHidden()
  }

  // Start sends the kickoff input to v2-server, which executes the run; the page follows it.
  await pane.getByRole('button', { name: '启动' }).click()
  const start = page.getByRole('dialog')
  await start.getByLabel('初始 Prompt').fill('给支付模块加一个对账接口')
  await start.getByRole('button', { name: '启动' }).click()
  await expect(pane.getByRole('button', { name: '舞台', exact: true })).toHaveAttribute('aria-pressed', 'true')

  if (!realAgents) {
    // No Agent is installed, so the first Agent node fails and the run with it, as v2-server reports.
    await expect(row('失败')).toBeVisible({ timeout: 30_000 })
    await pane.getByRole('button', { name: '全图', exact: true }).click()
    await pane.getByLabel('工作流全图俯瞰').getByText('起草', { exact: true }).click()
    await pane.getByRole('button', { name: '打开节点详情' }).click()
    await expect(pane.getByText(/not available in v2/).filter({ visible: true }).first()).toBeVisible()
    // Missing executors are not retryable; the root retry policy is supported.
    await expect(pane.getByLabel('v2 未应用的设置')).toHaveCount(0)
    // The Start node shows what this run was started with, not the frozen graph's empty prompt.
    await pane.getByRole('button', { name: '开始: 成功' }).click()
    await expect(pane.getByLabel('阶段详情').getByText('给支付模块加一个对账接口')).toBeVisible()
    await runAgain()
    return
  }

  await expect(row('成功')).toBeVisible({ timeout: 150_000 })
  await expect(pane.getByText('运行完成')).toBeVisible()
  await screenshot(page, testInfo.outputPath('1-result.png'))

  // 审阅 was asked with 起草's answer filled in, and answered in turn.
  await pane.getByRole('button', { name: '全图', exact: true }).click()
  await screenshot(page, testInfo.outputPath('2-overview.png'))
  await pane.getByLabel('工作流全图俯瞰').getByText('审阅', { exact: true }).click()
  await pane.getByRole('button', { name: '查看节点会话' }).click()
  await expect(pane.getByText(/审阅上一步的方案：V2 fixture complete\./)).toBeVisible()
  await screenshot(page, testInfo.outputPath('3-review-session.png'))
  // It is the Agent's real session, read from its events stream: the whole prompt the Agent was
  // given, with the workflow context, then its turn down to the answer.
  await expect(pane.getByText('<workspace_root>', { exact: false }).first()).toBeVisible()
  const toEnd = pane.getByRole('button', { name: '滚动到底部' })
  if (await toEnd.isVisible()) await toEnd.click()
  // The fixture Agent ran a shell command: a tool call only the real session carries.
  await expect(pane.getByText(/v2 fixture tool executed/).first()).toBeVisible()
  await screenshot(page, testInfo.outputPath('3-review-session-end.png'))

  // Its details name the executor and the model it actually ran on.
  await pane.getByRole('button', { name: '打开节点详情' }).click()
  await expect(pane.getByLabel('实际执行').filter({ visible: true }).first()).toContainText('· Fixture')
  await expect(pane.getByLabel('v2 未应用的设置')).toHaveCount(0)
  await screenshot(page, testInfo.outputPath('4-review-details.png'))
  await runAgain()
})

/** Start → 判断 → 加急处理 | 常规处理 → 汇合 → 输出: the Condition takes one branch by the kickoff text. */
const branches = JSON.stringify({
  nodes: [
    { id: 'start', type: 'workflow', position: { x: 0, y: 120 }, data: { kind: 'start', title: '开始' } },
    {
      id: 'triage', type: 'workflow', position: { x: 300, y: 120 },
      data: { kind: 'condition', title: '判断', cases: [{ id: 'urgent', logic: 'and', conditions: [{ variableSelector: ['start', 'input'], operator: 'contains', value: '紧急' }] }] },
    },
    { id: 'rush', type: 'workflow', position: { x: 620, y: 0 }, data: { kind: 'agent', title: '加急处理', agentConfig: { executor: { agentCli: 'official/ora-space.opencode', modelId: 'fixture' }, prompt: '加急处理：{{#start.input#}}' } } },
    { id: 'normal', type: 'workflow', position: { x: 620, y: 240 }, data: { kind: 'agent', title: '常规处理', agentConfig: { executor: { agentCli: 'official/ora-space.opencode', modelId: 'fixture' }, prompt: '常规处理：{{#start.input#}}' } } },
    { id: 'join', type: 'workflow', position: { x: 940, y: 120 }, data: { kind: 'aggregator', title: '汇合', aggregatorConfig: { variables: [['rush', 'output'], ['normal', 'output']] } } },
    { id: 'out', type: 'workflow', position: { x: 1240, y: 120 }, data: { kind: 'output', title: '输出', outputs: [{ name: 'result', variableSelector: ['join', 'output'] }] } },
  ],
  edges: [
    { id: 'e1', source: 'start', target: 'triage' },
    { id: 'e2', source: 'triage', sourceHandle: 'urgent', target: 'rush' },
    { id: 'e3', source: 'triage', sourceHandle: 'else', target: 'normal' },
    { id: 'e4', source: 'rush', target: 'join' },
    { id: 'e5', source: 'normal', target: 'join' },
    { id: 'e6', source: 'join', target: 'out' },
  ],
})

test('workflow runs: a Condition takes one branch and the other shows as inactive', async ({ page }, testInfo) => {
  test.setTimeout(180_000)
  await page.goto('/chat')
  await signIn(page)
  const tenantId = ((await (await page.request.get('/api/v1/me/tenants')).json()) as { items: { id: string }[] }).items[0]!.id
  const suffix = Date.now().toString(36)
  const name = `分支汇合 ${suffix}`
  const runName = `工单分流 ${suffix}`
  const created = await page.request.post(`/api/v2/tenants/${tenantId}/workflows`, { data: { name, graph: branches } })
  expect(created.status()).toBe(201)
  const workflowId = ((await created.json()) as { draft: { workflowId: string } }).draft.workflowId
  expect((await page.request.post(`/api/v2/tenants/${tenantId}/workflows/${workflowId}/publish`, { data: { version: 'v1' } })).ok()).toBe(true)

  await page.reload()
  const demo = page.getByRole('complementary', { name: '聊天记录' }).getByRole('region', { name: '项目 Demo' })
  await demo.getByRole('button', { name: '在 Demo 新建对话' }).click()
  await page.getByRole('menuitem', { name: '运行工作流' }).hover()
  await page.getByRole('menuitem', { name: new RegExp(name) }).click()
  const dialog = page.getByRole('dialog', { name: '运行工作流' })
  await dialog.getByLabel('运行名称').fill(runName)
  const injectLastFailure = dialog.getByRole('checkbox', { name: '节点重跑时把上次失败原因告诉智能体' })
  await expect(injectLastFailure).toBeVisible()
  await expect(injectLastFailure).toBeChecked()
  await screenshot(page, testInfo.outputPath('run-dialog-inject-default.png'))
  await dialog.getByRole('button', { name: '创建运行' }).click()
  const pane = page.getByRole('region', { name: '工作流运行' })
  await expect(pane.getByRole('heading', { name: runName })).toBeVisible()
  const row = (status: string) => demo.getByRole('button', { name: `工作流运行 ${runName}（${status}）` })

  // The kickoff text is not urgent, so the Condition takes else: 常规处理 runs, 加急处理 never does.
  await pane.getByRole('button', { name: '启动' }).click()
  const start = page.getByRole('dialog')
  await start.getByLabel('初始 Prompt').fill('客户反馈导出按钮偶尔无响应')
  await start.getByRole('button', { name: '启动' }).click()
  await expect(row(realAgents ? '成功' : '失败')).toBeVisible({ timeout: 150_000 })
  await expect(pane.getByText(`由 ${ownerName} 发起`)).toBeVisible()
  await expect(row(realAgents ? '成功' : '失败')).toContainText(`由 ${ownerName} 发起`)

  await pane.getByRole('button', { name: '全图', exact: true }).click()
  const overview = pane.getByLabel('工作流全图俯瞰')
  const card = (title: string) => overview.locator('.react-flow__node').filter({ has: page.getByText(title, { exact: true }) })
  await expect(card('加急处理').getByText('未激活分支')).toBeVisible()
  await expect(card('判断').getByText('成功', { exact: true })).toBeVisible()
  if (!realAgents) {
    // No Agent is installed: the taken branch's Agent fails, and the join after it never runs.
    await expect(card('常规处理').getByText('失败', { exact: true })).toBeVisible()
    // Its details still say which configured settings v2 does not apply.
    await card('常规处理').getByText('常规处理', { exact: true }).click()
    await pane.getByRole('button', { name: '打开节点详情' }).click()
    await expect(pane.getByLabel('v2 未应用的设置')).toHaveCount(0)
    return
  }
  await expect(card('常规处理').getByText('成功', { exact: true })).toBeVisible()
  await expect(card('汇合').getByText('成功', { exact: true })).toBeVisible()
  await screenshot(page, testInfo.outputPath('s2-branch-overview.png'))
  await pane.getByRole('button', { name: '舞台', exact: true }).click()
  await expect(pane.getByText('运行完成')).toBeVisible()
  await screenshot(page, testInfo.outputPath('s2-branch-result.png'))

  // The branch that ran says what its Agent actually ran on, beside the branch that did not.
  await pane.getByRole('button', { name: '全图', exact: true }).click()
  await card('常规处理').getByText('常规处理', { exact: true }).click()
  await pane.getByRole('button', { name: '打开节点详情' }).click()
  await expect(pane.getByLabel('实际执行').filter({ visible: true }).first()).toContainText('· Fixture')
  await screenshot(page, testInfo.outputPath('s2-branch-agent-details.png'))
})

test('workflow runs: another workspace member sees starter, full live and replayed node session, and cross-tenant stays 404', async ({ browser, page }, testInfo) => {
  test.setTimeout(240_000)
  await page.goto('/chat')
  await signIn(page)

  const tenantId = ((await (await page.request.get('/api/v1/me/tenants')).json()) as { items: { id: string }[] }).items[0]!.id
  const suffix = Date.now().toString(36)
  const name = `双人协作 ${suffix}`
  const runName = `双人可见运行 ${suffix}`
  const created = await page.request.post(`/api/v2/tenants/${tenantId}/workflows`, { data: { name, graph: chain } })
  expect(created.status()).toBe(201)
  const workflowId = ((await created.json()) as { draft: { workflowId: string } }).draft.workflowId
  expect((await page.request.post(`/api/v2/tenants/${tenantId}/workflows/${workflowId}/publish`, { data: { version: 'v1' } })).ok()).toBe(true)

  await page.reload()
  await page.getByRole('button', { name: '选择项目' }).click()
  await page.getByRole('option', { name: 'Demo', exact: true }).click()
  const createdRunResponse = page.waitForResponse((r) => r.request().method() === 'POST' && new RegExp(`/workflows/${workflowId}/runs$`).test(new URL(r.url()).pathname))
  const demo = page.getByRole('complementary', { name: '聊天记录' }).getByRole('region', { name: '项目 Demo' })
  await page.getByRole('button', { name: '运行工作流', exact: true }).click()
  await page.getByRole('menuitem', { name: new RegExp(name) }).click()
  const dialog = page.getByRole('dialog', { name: '运行工作流' })
  await dialog.getByLabel('运行名称').fill(runName)
  await dialog.getByRole('button', { name: '创建运行' }).click()
  const runId = ((await (await createdRunResponse).json()) as { run: { id: string } }).run.id

  const pane = page.getByRole('region', { name: '工作流运行' })
  await expect(pane.getByRole('heading', { name: runName })).toBeVisible()
  await expect(pane.getByText(/由 .+ 发起/)).toHaveCount(0)

  const colleagueContext = await browser.newContext()
  const colleague = await colleagueContext.newPage()
  try {
    await signInAsInvitedMember(colleague, `/chat?run=${encodeURIComponent(runId)}`)
    const colleaguePane = colleague.getByRole('region', { name: '工作流运行' })
    await expect(colleaguePane.getByRole('heading', { name: runName })).toBeVisible({ timeout: 30_000 })
    const colleagueDemo = colleague.getByRole('complementary', { name: '聊天记录' }).getByRole('region', { name: '项目 Demo' })
    const colleagueRow = (status: string) => colleagueDemo.getByRole('button', { name: `工作流运行 ${runName}（${status}）` })
    await expect(colleagueRow('未运行')).toBeVisible()

    // Owner starts the run while colleague is watching, so colleague observes both live and finished states.
    await pane.getByRole('button', { name: '启动' }).click()
    const start = page.getByRole('dialog')
    await start.getByLabel('初始 Prompt').fill('请检查双人节点会话可见性')
    await start.getByRole('button', { name: '启动' }).click()

    await expect(pane.getByText(`由 ${ownerName} 发起`)).toBeVisible()
    await expect(demo.getByRole('button', { name: new RegExp(`工作流运行 ${runName}（`) })).toContainText(`由 ${ownerName} 发起`)

    await expect(colleaguePane.getByText(`由 ${ownerName} 发起`)).toBeVisible({ timeout: 15_000 })
    await expect(colleagueDemo.getByRole('button', { name: new RegExp(`工作流运行 ${runName}（`) })).toContainText(`由 ${ownerName} 发起`)

    let draftNodeRunId = ''
    let seededSessionId = ''
    let otherTenantId = ''
    const db = database()
    try {
      if (!realAgents) {
        await expect(colleagueRow('失败')).toBeVisible({ timeout: 60_000 })
        await expect(colleagueRow('失败')).toContainText(`由 ${ownerName} 发起`)

        // Seed a persisted execution session owned by the run starter so CI mode (without Node assets)
        // still exercises starter-scoped session replay for another workspace member.
        await db.connect()
        const runRow = await db.query<{ started_by: string | null }>('SELECT started_by FROM v2_workflow_runs WHERE id = $1', [runId])
        const starterId = runRow.rows[0]?.started_by
        expect(starterId).toBeTruthy()
        const nodeRow = await db.query<{ id: string }>(
          `SELECT id FROM v2_workflow_node_runs WHERE run_id = $1 AND node_id = 'draft' AND deleted_at IS NULL`,
          [runId],
        )
        draftNodeRunId = nodeRow.rows[0]?.id ?? ''
        expect(draftNodeRunId).toBeTruthy()

        seededSessionId = `e2e-team-session-${randomUUID()}`
        const seededTurnId = `e2e-team-turn-${randomUUID()}`
        const starterPrompt = `发起人节点会话专属提示词 ${suffix}`
        const starterAnswer = `发起人节点会话回答 ${suffix}`
        const nowIso = new Date().toISOString()
        const caller = { Kind: 'workflow', ID: runId, NodeID: 'draft', AttemptID: 'e2e-ci' }
        const turnResult = { Status: 'completed', Answer: starterAnswer, AnswerComplete: true, StopReason: 'end_turn' }
        const historyRecords = [
          {
            sequence: 1,
            data: { at: nowIso, seq: 1, type: 'update', update: { sessionUpdate: 'user_message_chunk', content: { type: 'text', text: starterPrompt } } },
          },
          {
            sequence: 2,
            data: { at: nowIso, seq: 2, type: 'update', update: { sessionUpdate: 'agent_message_chunk', content: { type: 'text', text: starterAnswer } } },
          },
          { sequence: 3, data: { at: nowIso, seq: 3, type: 'turnEnded', stop_reason: 'end_turn' } },
        ]
        await db.query(
          `INSERT INTO v2_execution_sessions (id, tenant_id, user_id, data, ended_at, event_sequence, turn_position)
           VALUES ($1, $2, $3, $4::jsonb, now(), 3, 1)`,
          [seededSessionId, tenantId, starterId, JSON.stringify({ ID: seededSessionId, Model: { ID: 'fixture', Name: 'Fixture', BindingVersion: 'v1' }, CreatedAt: nowIso, EndedAt: nowIso })],
        )
        await db.query(
          `INSERT INTO v2_execution_turns (id, session_id, tenant_id, user_id, caller, position, data, ended_at)
           VALUES ($1, $2, $3, $4, $5::jsonb, 1, $6::jsonb, now())`,
          [
            seededTurnId,
            seededSessionId,
            tenantId,
            starterId,
            JSON.stringify(caller),
            JSON.stringify({
              Ref: { SessionID: seededSessionId, TurnID: seededTurnId },
              Caller: caller,
              Prompt: [{ type: 'text', text: starterPrompt }],
              StartedAt: nowIso,
              EndedAt: nowIso,
              Result: turnResult,
            }),
          ],
        )
        await db.query(
          `INSERT INTO v2_execution_events (session_id, sequence, data) VALUES ($1, 1, $2::jsonb), ($1, 2, $3::jsonb), ($1, 3, $4::jsonb)`,
          [
            seededSessionId,
            JSON.stringify({ Ref: { SessionID: seededSessionId, TurnID: seededTurnId }, Sequence: 1, OccurredAt: nowIso, Kind: 'history_snapshot', History: { records: historyRecords, incomplete: false } }),
            JSON.stringify({ Ref: { SessionID: seededSessionId, TurnID: seededTurnId }, Sequence: 2, OccurredAt: nowIso, Kind: 'turn_ended', Result: turnResult }),
            JSON.stringify({ Ref: { SessionID: seededSessionId, TurnID: '' }, Sequence: 3, OccurredAt: nowIso, Kind: 'session_ended' }),
          ],
        )
        await db.query(`UPDATE v2_workflow_node_runs SET session_id = $1 WHERE id = $2`, [seededSessionId, draftNodeRunId])

        await colleague.reload()
        await expect(colleaguePane.getByRole('heading', { name: runName })).toBeVisible({ timeout: 30_000 })
        await expect(colleaguePane.getByText(`由 ${ownerName} 发起`)).toBeVisible()
        await expect(colleagueRow('失败')).toContainText(`由 ${ownerName} 发起`)
        await colleaguePane.getByRole('button', { name: '全图', exact: true }).click()
        await colleaguePane.getByLabel('工作流全图俯瞰').getByText('起草', { exact: true }).click()
        const nodeStreamResponse = colleague.waitForResponse(
          (r) => new RegExp(`/workflow-runs/${runId}/nodes/${draftNodeRunId}/events$`).test(new URL(r.url()).pathname),
        )
        await colleaguePane.getByRole('button', { name: '查看节点会话' }).click()
        const streamRes = await nodeStreamResponse
        expect(streamRes.status()).toBe(200)
        expect(streamRes.headers()['content-type']).toBe('text/event-stream')
        await expect(colleaguePane.getByText(starterPrompt)).toBeVisible({ timeout: 15_000 })
        await expect(colleaguePane.getByText(starterAnswer)).toBeVisible({ timeout: 15_000 })

        const rawStream = await colleague.request.get(`/api/v2/tenants/${tenantId}/workflow-runs/${runId}/nodes/${draftNodeRunId}/events`)
        expect(rawStream.status()).toBe(200)
        const streamBody = await rawStream.text()
        expect(streamBody).toContain(starterPrompt)
        expect(streamBody).toContain(starterAnswer)
        expect(streamBody).toContain('event: end')
      } else {
        // Wait for 起草's session stream to become active while the workflow runs or finishes, and read full session as colleague.
        await colleaguePane.getByRole('button', { name: '全图', exact: true }).click()
        await expect(colleaguePane.getByLabel('工作流全图俯瞰').getByText('起草', { exact: true })).toBeVisible()
        await colleaguePane.getByLabel('工作流全图俯瞰').getByText('起草', { exact: true }).click()
        await colleaguePane.getByRole('button', { name: '查看节点会话' }).click()
        await expect(colleaguePane.getByText(/根据需求起草方案：请检查双人节点会话可见性/)).toBeVisible({ timeout: 60_000 })
        await expect(colleaguePane.getByText('<workspace_root>', { exact: false }).first()).toBeVisible({ timeout: 60_000 })
        await screenshot(colleague, testInfo.outputPath('member-live-session.png'))

        await expect(colleagueRow('成功')).toBeVisible({ timeout: 150_000 })
        await screenshot(colleague, testInfo.outputPath('member-finished-header-sidebar.png'))
        await colleaguePane.getByRole('button', { name: '全图', exact: true }).click()
        await colleaguePane.getByLabel('工作流全图俯瞰').getByText('审阅', { exact: true }).click()
        await colleaguePane.getByRole('button', { name: '查看节点会话' }).click()
        await expect(colleaguePane.getByText(/审阅上一步的方案：V2 fixture complete\./)).toBeVisible({ timeout: 30_000 })
        const toEnd = colleaguePane.getByRole('button', { name: '滚动到底部' })
        if (await toEnd.isVisible()) await toEnd.click()
        await expect(colleaguePane.getByText(/v2 fixture tool executed/).first()).toBeVisible()

        // Reloading the page keeps starter display and full session replay for the non-starter colleague.
        await colleague.reload()
        await expect(colleaguePane.getByRole('heading', { name: runName })).toBeVisible({ timeout: 30_000 })
        await expect(colleaguePane.getByText(`由 ${ownerName} 发起`)).toBeVisible()
        await expect(colleagueRow('成功')).toContainText(`由 ${ownerName} 发起`)
        await colleaguePane.getByRole('button', { name: '全图', exact: true }).click()
        await colleaguePane.getByLabel('工作流全图俯瞰').getByText('审阅', { exact: true }).click()
        await colleaguePane.getByRole('button', { name: '查看节点会话' }).click()
        await expect(colleaguePane.getByText(/审阅上一步的方案：V2 fixture complete\./)).toBeVisible({ timeout: 30_000 })
        const toEndAfterReload = colleaguePane.getByRole('button', { name: '滚动到底部' })
        if (await toEndAfterReload.isVisible()) await toEndAfterReload.click()
        await expect(colleaguePane.getByText(/v2 fixture tool executed/).first()).toBeVisible()
        await screenshot(colleague, testInfo.outputPath('member-reloaded-session.png'))

        const detail = (await (await colleague.request.get(`/api/v2/tenants/${tenantId}/workflow-runs/${runId}`)).json()) as {
          nodes: Array<{ id: string; nodeId: string }>
        }
        draftNodeRunId = detail.nodes.find((n) => n.nodeId === 'draft')?.id ?? ''
        expect(draftNodeRunId).toBeTruthy()
        await db.connect()
      }

      // Cross-tenant requests from a tenant the colleague DOES belong to reach the workflow handlers
      // and must still be rejected with 404 workflow_run_not_found.
      const colleagueMe = (await (await colleague.request.get('/api/v1/me')).json()) as { id: string }
      const otherTenantRow = await db.query<{ id: string }>(
        `INSERT INTO tenants (name) VALUES ($1) RETURNING id::text`,
        [`跨租户隔离检查-${suffix}`],
      )
      otherTenantId = otherTenantRow.rows[0]!.id
      await db.query(`INSERT INTO memberships (tenant_id, user_id, role) VALUES ($1, $2, 'member')`, [otherTenantId, colleagueMe.id])

      const otherTenantRun = await colleague.request.get(`/api/v2/tenants/${otherTenantId}/workflow-runs/${runId}`)
      expect(otherTenantRun.status()).toBe(404)
      expect((await otherTenantRun.json()).code).toBe('workflow_run_not_found')
      const otherTenantEvents = await colleague.request.get(
        `/api/v2/tenants/${otherTenantId}/workflow-runs/${runId}/nodes/${draftNodeRunId}/events`,
      )
      expect(otherTenantEvents.status()).toBe(404)
      expect((await otherTenantEvents.json()).code).toBe('workflow_run_not_found')

      // Requests targeting a tenant the colleague does not belong to stay 404 as well.
      const crossRun = await colleague.request.get(`/api/v2/tenants/00000000-0000-0000-0000-000000000000/workflow-runs/${runId}`)
      expect(crossRun.status()).toBe(404)
      const crossEvents = await colleague.request.get(`/api/v2/tenants/00000000-0000-0000-0000-000000000000/workflow-runs/${runId}/nodes/${draftNodeRunId}/events`)
      expect(crossEvents.status()).toBe(404)
    } finally {
      if (seededSessionId) {
        await db.query(`UPDATE v2_workflow_node_runs SET session_id = NULL WHERE id = $1`, [draftNodeRunId])
        await db.query(`DELETE FROM v2_execution_events WHERE session_id = $1`, [seededSessionId])
        await db.query(`DELETE FROM v2_execution_turns WHERE session_id = $1`, [seededSessionId])
        await db.query(`DELETE FROM v2_execution_sessions WHERE id = $1`, [seededSessionId])
      }
      if (otherTenantId) {
        await db.query(`DELETE FROM memberships WHERE tenant_id = $1`, [otherTenantId])
        await db.query(`DELETE FROM tenants WHERE id = $1`, [otherTenantId])
      }
      await db.end().catch(() => undefined)
    }
  } finally {
    await colleagueContext.close()
  }
})

test('workflow runs: unchecking injectLastFailure sends false on create and resets on reopen', async ({ page }) => {
  await page.goto('/chat')
  await signIn(page)
  const tenantId = ((await (await page.request.get('/api/v1/me/tenants')).json()) as { items: { id: string }[] }).items[0]!.id
  const suffix = Date.now().toString(36)
  const name = `失败注入开关 ${suffix}`
  const created = await page.request.post(`/api/v2/tenants/${tenantId}/workflows`, { data: { name, graph: chain } })
  expect(created.status()).toBe(201)
  const workflowId = ((await created.json()) as { draft: { workflowId: string } }).draft.workflowId
  expect((await page.request.post(`/api/v2/tenants/${tenantId}/workflows/${workflowId}/publish`, { data: { version: 'v1' } })).ok()).toBe(true)

  await page.reload()
  await page.getByRole('button', { name: '运行工作流', exact: true }).click()
  await page.getByRole('menuitem', { name: new RegExp(name) }).click()
  const dialog = page.getByRole('dialog', { name: '运行工作流' })
  const checkbox = dialog.getByRole('checkbox', { name: '节点重跑时把上次失败原因告诉智能体' })
  await expect(checkbox).toBeChecked()
  await checkbox.uncheck()
  await expect(checkbox).not.toBeChecked()

  const createRequest = page.waitForRequest((req) => req.url().includes(`/workflows/${workflowId}/runs`) && req.method() === 'POST')
  const createResponse = page.waitForResponse((res) => res.url().includes(`/workflows/${workflowId}/runs`) && res.request().method() === 'POST')
  await dialog.getByLabel('运行名称').fill(`关闭注入 ${suffix}`)
  await dialog.getByRole('button', { name: '创建运行' }).click()
  expect((await createRequest).postDataJSON()).toMatchObject({ injectLastFailure: false })
  const createdRun = (await (await createResponse).json()) as { run: { id: string } }
  expect((await page.request.delete(`/api/v2/tenants/${tenantId}/workflow-runs/${createdRun.run.id}`)).ok()).toBe(true)

  await page.getByRole('button', { name: '新建对话', exact: true }).click()
  await page.getByRole('button', { name: '运行工作流', exact: true }).click()
  await page.getByRole('menuitem', { name: new RegExp(name) }).click()
  await expect(page.getByRole('dialog', { name: '运行工作流' }).getByRole('checkbox', { name: '节点重跑时把上次失败原因告诉智能体' })).toBeChecked()
})
