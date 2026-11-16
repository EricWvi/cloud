import { expect, test, type Page } from '@playwright/test'
import { resolve } from 'node:path'
import { signIn } from './login.ts'

// 「从失败处继续」 against the real v2-server: Start → 判断 (Condition, succeeds) → 缺执行器的智能体
// (an Agent whose executor v2 does not offer, so it fails as missing_agent_ref with or without Node
// assets) → 输出, beside an unrelated 旁支汇总 (Aggregator) branch. The resume reruns only the
// failed Agent and its successor in the same run; the Agent fails again deterministically, which
// shows the attempt history and the live update without a real model.
const node = (id: string, kind: string, title: string, x: number, data: Record<string, unknown> = {}) => ({ id, type: 'workflow', position: { x, y: 0 }, data: { kind, title, ...data } })
const graph = JSON.stringify({ nodes: [
  node('start', 'start', '开始', 0),
  node('judge', 'condition', '判断输入', 300, { cases: [{ id: 'urgent', logic: 'and', conditions: [{ variableSelector: ['start', 'input'], operator: 'contains', value: '紧急' }] }] }),
  node('agent', 'agent', '缺执行器的智能体', 600, { agentConfig: { executor: { agentCli: 'official/ora-space.missing', modelId: 'fixture' }, prompt: '处理：{{#start.input#}}' } }),
  node('out', 'output', '输出', 900, { outputs: [{ name: 'answer', variableSelector: ['agent', 'output'] }] }),
  { ...node('side', 'aggregator', '旁支汇总', 300, { aggregatorConfig: { variables: [['start', 'input']] } }), position: { x: 300, y: 200 } },
], edges: [
  { id: 'e1', source: 'start', target: 'judge' }, { id: 'e2', source: 'judge', target: 'agent' },
  { id: 'e3', source: 'agent', target: 'out' }, { id: 'e4', source: 'start', target: 'side' },
] })

interface Detail {
  run: { id: string, status: string }
  nodes: { id: string, nodeId: string, status: string, sessionId: string | null, payload: string | null }[]
  failedAttempts: { nodeId: string, nodeRunId: string, attempt: number }[]
}

async function prepare(page: Page, title: string, workflowGraph = graph, projectName?: string) {
  await page.setViewportSize({ width: 1600, height: 1000 })
  await page.goto('/chat')
  const login = page.getByRole('button', { name: '登录', exact: true })
  await expect(login.or(page.getByRole('button', { name: '退出登录', exact: true }))).toBeVisible()
  if (await login.isVisible()) await signIn(page)
  const tid = (await (await page.request.get('/api/v1/me/tenants')).json()).items[0].id as string
  const base = `/api/v2/tenants/${tid}`
  const projects = (await (await page.request.get(`${base}/projects?limit=100`)).json()).items as { id: string, name: string }[]
  const project = projectName === undefined ? projects[0]! : projects.find((p) => p.name === projectName)!
  const name = `${title} ${Date.now().toString(36)}`
  const created = await page.request.post(`${base}/workflows`, { data: { name, graph: workflowGraph } })
  expect(created.status(), await created.text()).toBe(201)
  const wid = (await created.json()).workflow.id as string
  const published = await page.request.post(`${base}/workflows/${wid}/publish`, { data: { version: 'v1' } })
  expect(published.ok(), await published.text()).toBe(true)
  const run = await page.request.post(`${base}/workflows/${wid}/runs`, { data: { projectId: project.id, name } })
  expect(run.status()).toBe(201)
  return { base, wid, rid: (await run.json()).run.id as string, name }
}

const read = async (page: Page, base: string, rid: string) => (await (await page.request.get(`${base}/workflow-runs/${rid}`)).json()) as Detail
const row = (detail: Detail, nodeId: string) => detail.nodes.find((n) => n.nodeId === nodeId)!

async function settled(page: Page) {
  await page.waitForFunction(() => document.getAnimations().filter((a) => a.timeline === document.timeline && a.effect?.getComputedTiming().endTime !== Infinity).every((a) => a.playState !== 'running'))
}

test('resume from failure previews the rerun, keeps files and reruns the failed node in the same run', async ({ page }) => {
  const { base, wid, rid } = await prepare(page, '续跑验收')
  expect((await page.request.patch(`${base}/workflow-runs/${rid}/input`, { data: { input: '整理需求' } })).ok()).toBe(true)
  expect((await page.request.post(`${base}/workflow-runs/${rid}/start`)).ok()).toBe(true)
  await expect.poll(async () => (await read(page, base, rid)).run.status, { timeout: 60_000 }).toBe('failed')
  const before = await read(page, base, rid)
  expect(row(before, 'out')).toBeUndefined()

  await page.goto(`/chat?run=${rid}`)
  const pane = page.getByRole('region', { name: '工作流运行' })
  await expect(pane.getByRole('button', { name: '再跑一次', exact: true })).toBeVisible()
  await pane.getByRole('button', { name: '从失败处继续', exact: true }).click()
  const dialog = page.getByRole('dialog', { name: '从失败处继续' })
  const startsFrom = dialog.getByRole('region', { name: '从这些节点继续' })
  await expect(startsFrom).toContainText('缺执行器的智能体')
  await expect(startsFrom).toContainText('失败')
  await expect(dialog.getByRole('region', { name: '随后重新执行' })).toHaveText(/输出/)
  // Succeeded nodes, the unrelated branch included, are not rerun.
  await expect(dialog).not.toContainText('判断输入')
  await expect(dialog).not.toContainText('旁支汇总')
  await expect(dialog.getByText('工作区文件保持现状，不会回滚；重跑的节点在当前文件上继续工作。')).toBeVisible()
  await settled(page)
  await page.screenshot({ path: resolve('../.tmp/screens/resume-preview.png') })

  await dialog.getByRole('button', { name: '从失败处继续', exact: true }).click()
  await expect(dialog).toBeHidden()
  // The same run went back to running and failed again on a new attempt of the same node.
  await expect.poll(async () => {
    const detail = await read(page, base, rid)
    return detail.run.status === 'failed' && row(detail, 'agent').id !== row(before, 'agent').id
  }, { timeout: 60_000 }).toBe(true)
  const after = await read(page, base, rid)
  for (const kept of ['start', 'judge', 'side']) expect(row(after, kept).id).toBe(row(before, kept).id)
  expect(JSON.parse(row(after, 'agent').payload ?? '{}').error_detail.attempt).toBe(2)
  expect(after.failedAttempts.map((a) => [a.nodeId, a.nodeRunId, a.attempt])).toEqual([['agent', row(before, 'agent').id, 1]])
  expect((await (await page.request.get(`${base}/workflows/${wid}/runs`)).json()).runs).toHaveLength(1)

  // The run view follows: the badge shows the new outcome and the replaced attempt is labelled.
  await expect(pane.getByRole('button', { name: '从失败处继续', exact: true })).toBeVisible()
  await pane.getByRole('button', { name: '全图', exact: true }).click()
  await pane.getByLabel('工作流全图俯瞰').getByText('缺执行器的智能体', { exact: true }).click()
  // A wide stage opens the inspector on its own; a narrow one needs the button.
  const details = pane.getByRole('button', { name: '打开节点详情' })
  if (await details.isVisible()) await details.click()
  await expect(pane.getByText('第 2 次尝试', { exact: true }).filter({ visible: true })).toBeVisible()
  const history = pane.locator('[data-slot="failed-attempts"]').filter({ visible: true })
  await expect(history).toContainText('第 1 次尝试')
  await expect(history).toContainText('已手动续跑')
  // A configuration failure the frozen snapshot cannot change gets no resume advice.
  await expect(pane.getByText('这类失败通常源于工作流本身', { exact: false }).filter({ visible: true })).toBeVisible()
  await expect(pane.getByText(/可在顶部点击「从失败处继续」/).filter({ visible: true })).toHaveCount(0)
  await settled(page)
  await page.screenshot({ path: resolve('../.tmp/screens/resume-after.png') })
})

const reviewGraph = JSON.stringify({ nodes: [
  node('start', 'start', '开始', 0, { inputVariables: [{ name: 'findings', valueType: 'array[object]', value: [{ key: 'disk:root', title: 'Disk usage', description: 'Over threshold' }] }] }),
  node('review', 'form', '人工确认', 300, { formConfig: { title: 'Review', submitLabel: 'Submit', reportSelector: ['start', 'input'], itemsSelector: ['start', 'findings'], fields: [] } }),
  node('tasks', 'createTasks', '创建任务', 600, { createTasksConfig: { formNodeId: 'review' } }),
  node('out', 'output', '输出', 900, { outputs: [{ name: 'tasks', variableSelector: ['tasks', 'structured_output', 'tasks'] }] }),
], edges: [{ id: 'e1', source: 'start', target: 'review' }, { id: 'e2', source: 'review', target: 'tasks' }, { id: 'e3', source: 'tasks', target: 'out' }] })

test('resume from failure is not offered without a stopped node and explains a refusal', async ({ page }) => {
  // Cancelled before it started: nothing failed or was cancelled inside it, so only 「再跑一次」.
  const pending = await prepare(page, '续跑拒绝验收')
  expect((await page.request.post(`${pending.base}/workflow-runs/${pending.rid}/cancel`)).ok()).toBe(true)
  const refused = await page.request.post(`${pending.base}/workflow-runs/${pending.rid}/resume`)
  expect(refused.status()).toBe(409)
  expect(await refused.json()).toMatchObject({ code: 'workflow_run_not_resumable', params: { reason: 'no_failed_node' } })
  await page.goto(`/chat?run=${pending.rid}`)
  let pane = page.getByRole('region', { name: '工作流运行' })
  await expect(pane.getByRole('button', { name: '再跑一次', exact: true })).toBeVisible()
  await expect(pane.getByRole('button', { name: '从失败处继续', exact: true })).toHaveCount(0)

  // Cancelled while its review form waited: the form is frozen and was never submitted.
  const review = await prepare(page, '确认表单续跑验收', reviewGraph)
  expect((await page.request.patch(`${review.base}/workflow-runs/${review.rid}/input`, { data: { input: '巡检报告' } })).ok()).toBe(true)
  expect((await page.request.post(`${review.base}/workflow-runs/${review.rid}/start`)).ok()).toBe(true)
  await expect.poll(async () => row(await read(page, review.base, review.rid), 'review')?.status, { timeout: 60_000 }).toBe('pending')
  expect((await page.request.post(`${review.base}/workflow-runs/${review.rid}/cancel`)).ok()).toBe(true)
  await page.goto(`/chat?run=${review.rid}`)
  pane = page.getByRole('region', { name: '工作流运行' })
  await pane.getByRole('button', { name: '从失败处继续', exact: true }).click()
  const dialog = page.getByRole('dialog', { name: '从失败处继续' })
  await expect(dialog.getByRole('alert')).toHaveText('这次运行停在确认表单上，表单还没有提交，不能从失败处继续；请用「再跑一次」重新运行。')
  await expect(dialog.getByRole('button', { name: '从失败处继续', exact: true })).toBeDisabled()
  await settled(page)
  await page.screenshot({ path: resolve('../.tmp/screens/resume-refused.png') })
})

// Real Agents on the fixture model: Start → 起草 → 审阅 → 发布. The run is stopped while one of the
// Agents executes, then resumed from the page; the stopped Agent runs again for real, the ones that
// had finished keep their rows, and the run succeeds.
const realChain = JSON.stringify({ nodes: [
  node('start', 'start', '开始', 0),
  ...[['draft', '起草', 300], ['review', '审阅', 600], ['ship', '发布', 900]].map(([id, title, x]) =>
    node(id as string, 'agent', title as string, x as number, { agentConfig: { executor: { agentCli: 'official/ora-space.opencode', modelId: 'fixture' }, prompt: `${title}：{{#start.input#}}` } })),
], edges: [{ id: 'e1', source: 'start', target: 'draft' }, { id: 'e2', source: 'draft', target: 'review' }, { id: 'e3', source: 'review', target: 'ship' }] })

test('resume from failure reruns a stopped real Agent in the same run until it succeeds', async ({ page }) => {
  test.skip(!process.env.V2_NODE_ASSETS_DIR, 'needs the prepared Node fixture executor')
  test.setTimeout(240_000)
  const { base, rid, name } = await prepare(page, '真实续跑验收', realChain, 'Demo')
  expect((await page.request.patch(`${base}/workflow-runs/${rid}/input`, { data: { input: '整理需求' } })).ok()).toBe(true)
  expect((await page.request.post(`${base}/workflow-runs/${rid}/start`)).ok()).toBe(true)
  // Stop it as soon as an Agent's turn is executing.
  await expect.poll(async () => (await read(page, base, rid)).nodes.some((n) => n.status === 'running' && n.sessionId !== null), { timeout: 120_000, intervals: [100] }).toBe(true)
  const cancelled = await page.request.post(`${base}/workflow-runs/${rid}/cancel`)
  expect(cancelled.ok(), await cancelled.text()).toBe(true)
  const before = await read(page, base, rid)
  expect(before.run.status).toBe('cancelled')
  const stopped = before.nodes.find((n) => n.status === 'cancelled')!
  const kept = before.nodes.filter((n) => n.status === 'succeeded')

  await page.goto(`/chat?run=${rid}`)
  const pane = page.getByRole('region', { name: '工作流运行' })
  await pane.getByRole('button', { name: '从失败处继续', exact: true }).click()
  const dialog = page.getByRole('dialog', { name: '从失败处继续' })
  await expect(dialog.getByRole('region', { name: '从这些节点继续' })).toContainText('已取消')
  await dialog.getByRole('button', { name: '从失败处继续', exact: true }).click()
  await expect(dialog).toBeHidden()
  await expect.poll(async () => (await read(page, base, rid)).run.status, { timeout: 180_000 }).toBe('succeeded')
  const after = await read(page, base, rid)
  for (const n of kept) expect(row(after, n.nodeId).id).toBe(n.id)
  expect(row(after, stopped.nodeId).id).not.toBe(stopped.id)
  expect(row(after, stopped.nodeId).status).toBe('succeeded')
  expect(after.nodes.filter((n) => n.nodeId !== 'start').every((n) => n.status === 'succeeded')).toBe(true)
  // The page followed the same run to its new outcome.
  await expect(page.getByRole('button', { name: `工作流运行 ${name}（成功）` })).toBeVisible({ timeout: 30_000 })
  await expect(pane.getByRole('button', { name: '从失败处继续', exact: true })).toHaveCount(0)
  await settled(page)
  await page.screenshot({ path: resolve('../.tmp/screens/resume-real-success.png') })
})
