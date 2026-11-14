import { expect, test, type Page } from '@playwright/test'
import { signIn } from './login.ts'

// An interactive Agent needs a real Agent session to talk to: with prepared Node assets
// (`task v2:node:prepare`, then V2_NODE_ASSETS_DIR) v2-server runs it on the fixture model. Without
// them, as in CI, the spec skips.
const realAgents = !!process.env.V2_NODE_ASSETS_DIR

/** Start → 起草 (interactive) → 审阅 → 输出: 审阅 reads what completing 起草 accepted. */
const chain = JSON.stringify({
  nodes: [
    { id: 'start', type: 'workflow', position: { x: 0, y: 0 }, data: { kind: 'start', title: '开始' } },
    { id: 'draft', type: 'workflow', position: { x: 320, y: 0 }, data: { kind: 'agent', title: '起草', agentConfig: { executor: { agentCli: 'official/ora-space.opencode', modelId: 'fixture' }, prompt: '根据需求起草方案：{{#start.input#}}', interactive: true } } },
    { id: 'review', type: 'workflow', position: { x: 640, y: 0 }, data: { kind: 'agent', title: '审阅', agentConfig: { executor: { agentCli: 'official/ora-space.opencode', modelId: 'fixture' }, prompt: '审阅上一步的方案：{{#draft.output#}}' } } },
    { id: 'out', type: 'workflow', position: { x: 960, y: 0 }, data: { kind: 'output', title: '输出', outputs: [{ name: 'review', variableSelector: ['review', 'output'] }] } },
  ],
  edges: [
    { id: 'e1', source: 'start', target: 'draft' },
    { id: 'e2', source: 'draft', target: 'review' },
    { id: 'e3', source: 'review', target: 'out' },
  ],
})

/** Start → 起草 (interactive) and Start → 旁支: the review's repro of a sibling starting its Agent. */
const siblings = JSON.stringify({
  nodes: [
    { id: 'start', type: 'workflow', position: { x: 0, y: 0 }, data: { kind: 'start', title: '开始' } },
    { id: 'draft', type: 'workflow', position: { x: 320, y: 0 }, data: { kind: 'agent', title: '起草', agentConfig: { executor: { agentCli: 'official/ora-space.opencode', modelId: 'fixture' }, prompt: '根据需求起草方案：{{#start.input#}}', interactive: true } } },
    { id: 'side', type: 'workflow', position: { x: 320, y: 200 }, data: { kind: 'agent', title: '旁支', agentConfig: { executor: { agentCli: 'official/ora-space.opencode', modelId: 'fixture' }, prompt: '另写一份说明' } } },
  ],
  edges: [
    { id: 'e1', source: 'start', target: 'draft' },
    { id: 'e2', source: 'start', target: 'side' },
  ],
})

/** Publishes graph, creates a run of it in Demo from the chat entry and starts it. */
async function startRun(page: Page, graph: string, label: string) {
  await page.goto('/chat')
  await signIn(page)
  const tenantId = ((await (await page.request.get('/api/v1/me/tenants')).json()) as { items: { id: string }[] }).items[0]!.id
  const suffix = Date.now().toString(36)
  const name = `${label} ${suffix}`
  const runName = `${label}运行 ${suffix}`
  const created = await page.request.post(`/api/v2/tenants/${tenantId}/workflows`, { data: { name, graph } })
  expect(created.status()).toBe(201)
  const workflowId = ((await created.json()) as { draft: { workflowId: string } }).draft.workflowId
  expect((await page.request.post(`/api/v2/tenants/${tenantId}/workflows/${workflowId}/publish`, { data: { version: 'v1' } })).ok()).toBe(true)

  await page.reload()
  await page.getByRole('button', { name: '选择项目' }).click()
  await page.getByRole('option', { name: 'Demo', exact: true }).click()
  const demo = page.getByRole('complementary', { name: '聊天记录' }).getByRole('region', { name: '项目 Demo' })
  await page.getByRole('button', { name: '运行工作流', exact: true }).click()
  await page.getByRole('menuitem', { name: new RegExp(name) }).click()
  const dialog = page.getByRole('dialog', { name: '运行工作流' })
  await dialog.getByLabel('运行名称').fill(runName)
  await dialog.getByRole('button', { name: '创建运行' }).click()
  const pane = page.getByRole('region', { name: '工作流运行' })
  await expect(pane.getByRole('heading', { name: runName })).toBeVisible()
  const row = (status: string) => demo.getByRole('button', { name: `工作流运行 ${runName}（${status}）` })
  await pane.getByRole('button', { name: '启动' }).click()
  const start = page.getByRole('dialog')
  await start.getByLabel('初始 Prompt').fill('给支付模块加一个对账接口')
  await start.getByRole('button', { name: '启动' }).click()
  const runId = async () => {
    const runs = (await (await page.request.get(`/api/v2/tenants/${tenantId}/workflows/${workflowId}/runs`)).json()) as { runs: { id: string; name: string }[] }
    return runs.runs.find((r) => r.name === runName)!.id
  }
  return { tenantId, pane, row, runId }
}

const partialNote = '当前为简化版：由发起人确认完成。停止本轮会结束这个节点的会话，停止后不能继续对话；运行里另一个 Agent 开始执行时，这个会话也会结束。'

async function screenshot(page: Page, path: string) {
  await page.waitForFunction(() =>
    document
      .getAnimations()
      .filter((a) => a.timeline === document.timeline && a.effect?.getComputedTiming().endTime !== Infinity)
      .every((a) => a.playState !== 'running'),
  )
  await page.screenshot({ path })
}

test('interactive Agent: the starter talks to the parked node in its session, then completes it', async ({ page }, testInfo) => {
  test.skip(!realAgents, 'prepared Node assets are required for a real Agent session')
  test.setTimeout(240_000)
  const { tenantId, pane, row, runId: readRunId } = await startRun(page, chain, '交互起草')

  // After its first turn 起草 parks: the run waits for input and the rail says so.
  const draftChip = pane.locator('[data-path-node="draft"]')
  await expect(draftChip).toContainText('等待输入', { timeout: 150_000 })
  await expect(pane.getByText('等待输入：打开会话继续对话，或完成这一步')).toBeVisible()
  await screenshot(page, testInfo.outputPath('1-awaiting-input.png'))

  // The starter replies in the node's own session; the answer arrives over its events stream.
  await pane.getByRole('button', { name: '查看节点会话' }).click()
  await expect(pane.getByText(partialNote)).toBeVisible()
  const composer = pane.getByRole('textbox', { name: '回复 Agent' })
  await composer.fill('请把接口命名改成 reconcile')
  await composer.press('Enter')
  await expect(pane.getByText('请把接口命名改成 reconcile').first()).toBeVisible({ timeout: 30_000 })
  await screenshot(page, testInfo.outputPath('2-follow-up.png'))
  await expect(draftChip).toContainText('等待输入', { timeout: 120_000 })
  await expect(pane.getByText(/V2 fixture complete\./).last()).toBeVisible()
  await screenshot(page, testInfo.outputPath('3-parked-again.png'))

  // Completing writes 起草's output; 审阅 reads it and the run finishes.
  await pane.getByRole('button', { name: '完成', exact: true }).click()
  await expect(row('成功')).toBeVisible({ timeout: 150_000 })
  await screenshot(page, testInfo.outputPath('4-result.png'))
  const runId = await readRunId()
  const detail = (await (await page.request.get(`/api/v2/tenants/${tenantId}/workflow-runs/${runId}`)).json()) as { nodes: { nodeId: string; status: string; output: string | null; payload: string | null }[] }
  const draft = detail.nodes.find((n) => n.nodeId === 'draft')!
  expect(draft.status).toBe('succeeded')
  expect(JSON.parse(draft.payload!).interactive.turns).toBe(2)
  expect(detail.nodes.find((n) => n.nodeId === 'review')!.status).toBe('succeeded')
})

test('interactive Agent: a sibling starting its Agent ends the parked session, which can still complete', async ({ page }, testInfo) => {
  test.skip(!realAgents, 'prepared Node assets are required for a real Agent session')
  test.setTimeout(240_000)
  const { tenantId, pane, row, runId: readRunId } = await startRun(page, siblings, '交互旁支')

  // 起草 parks after its first turn; 旁支 then starts on the run's directory, and the pinned Node
  // holds one Agent session per checkout, so 起草's session ends and the node says so.
  const draftChip = pane.locator('[data-path-node="draft"]')
  await expect(draftChip).toContainText('等待输入', { timeout: 150_000 })
  const runId = await readRunId()
  const draftRow = async () => {
    const detail = (await (await page.request.get(`/api/v2/tenants/${tenantId}/workflow-runs/${runId}`)).json()) as { nodes: { nodeId: string; status: string; payload: string | null }[] }
    return { draft: detail.nodes.find((n) => n.nodeId === 'draft'), side: detail.nodes.find((n) => n.nodeId === 'side') }
  }
  await expect.poll(async () => (await draftRow()).side?.status, { timeout: 150_000 }).toBe('succeeded')
  const { draft } = await draftRow()
  expect(draft!.status).toBe('pending')
  expect(JSON.parse(draft!.payload!).interactive).toMatchObject({ turns: 1, session_ended: true, completable: true })

  // A follow-up is refused instead of accepted and lost.
  const turn = await page.request.post(`/api/v2/tenants/${tenantId}/workflow-runs/${runId}/nodes/draft/turns`, { data: { content: '还在吗' } })
  expect(turn.status()).toBe(409)
  expect(((await turn.json()) as { code: string }).code).toBe('workflow_node_session_ended')

  await draftChip.click()
  await pane.getByRole('button', { name: '查看节点会话' }).click()
  await expect(pane.getByText('会话已结束（停止了本轮、Agent 断开，或运行里另一个 Agent 开始执行），不能继续对话；可以用最后一次正常结束的回答完成这一步。')).toBeVisible()
  await expect(pane.getByRole('textbox', { name: '回复 Agent' })).toHaveCount(0)
  await screenshot(page, testInfo.outputPath('5-sibling-ended-session.png'))
  await pane.getByRole('button', { name: '完成', exact: true }).click()
  await expect(row('成功')).toBeVisible({ timeout: 60_000 })
  expect((await draftRow()).draft!.status).toBe('succeeded')
})
