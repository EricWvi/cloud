import { expect, test, type Page } from '@playwright/test'
import { signIn } from './login.ts'

const graph = (nodes: { id: string; data: Record<string, unknown> }[]) => JSON.stringify({
  nodes: nodes.map((n, i) => ({ ...n, type: 'workflow', position: { x: i * 340, y: 100 }, data: { title: n.id, ...n.data } })),
  edges: nodes.slice(1).map((n, i) => ({ id: `${nodes[i]!.id}-${n.id}`, source: nodes[i]!.id, target: n.id })),
})

async function publish(page: Page, base: string, name: string, definition: string) {
  const created = await page.request.post(`${base}/workflows`, { data: { name, graph: definition } })
  expect(created.status(), await created.text()).toBe(201)
  const id = (await created.json()).workflow.id as string
  const published = await page.request.post(`${base}/workflows/${id}/publish`, { data: { version: 'v1' } })
  expect(published.ok(), await published.text()).toBe(true)
  return id
}

async function noHorizontalScroll(page: Page) {
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true)
  expect(await page.locator('.mc-task-page').evaluate(el => el.scrollWidth <= el.clientWidth)).toBe(true)
  expect(await page.locator('.mc-task-main-col').evaluate(el => el.clientWidth)).toBeGreaterThan(280)
}

test('real task links, three workflow kinds, explicit handling and a readable 390px workspace', async ({ page }) => {
  test.setTimeout(90_000)
  await page.setViewportSize({ width: 1440, height: 1000 })
  await page.goto('/collaboration')
  await signIn(page)
  const tid = (await (await page.request.get('/api/v1/me/tenants')).json()).items[0].id
  const user = (await (await page.request.get('/api/v1/me')).json()).id
  const base = `/api/v2/tenants/${tid}`
  const projectId = (await (await page.request.get(`${base}/projects`)).json()).items[0].id
  const suffix = crypto.randomUUID().slice(0, 8)
  const finding = `task-workflow-${suffix}`
  const title = `核对磁盘增长来源与清理策略 ${suffix}`
  const sourceWorkflow = await publish(page, base, `巡检来源 ${suffix}`, graph([
    { id: 'start', data: { kind: 'start', inputVariables: [{ name: 'findings', valueType: 'array[object]', value: [{ key: finding, title, description: '核对磁盘增长来源，给出可复核的清理方案。', evidence: '86% > 80%', severity: '中', module: '存储' }] }] } },
    { id: 'review', data: { kind: 'form', formConfig: { title: '巡检问题确认', submitLabel: '确认并生成任务', reportSelector: ['start', 'input'], itemsSelector: ['start', 'findings'], fields: [] } } },
    { id: 'tasks', data: { kind: 'createTasks', createTasksConfig: { formNodeId: 'review' } } },
    { id: 'output', data: { kind: 'output', outputs: [{ name: 'tasks', variableSelector: ['tasks', 'structured_output', 'tasks'] }] } },
  ]))
  const createdSource = await page.request.post(`${base}/workflows/${sourceWorkflow}/runs`, { data: { projectId, input: '巡检报告：磁盘使用率为 86%，请确认异常并创建处理任务。' } })
  expect(createdSource.status()).toBe(201)
  const sourceId = (await createdSource.json()).run.id
  expect((await page.request.post(`${base}/workflow-runs/${sourceId}/start`)).ok()).toBe(true)
  const formPath = `${base}/workflow-runs/${sourceId}/form`
  await expect.poll(async () => (await page.request.get(formPath)).status()).toBe(200)
  const form = await (await page.request.get(formPath)).json()
  const submitted = await page.request.post(formPath, {
    headers: { 'Idempotency-Key': `form-${suffix}` },
    data: { version: form.version, answers: [{ key: finding, decision: 'confirmed', assignee: { type: 'human', id: user }, values: {} }] },
  })
  expect(submitted.ok(), await submitted.text()).toBe(true)
  await expect.poll(async () => (await (await page.request.get(formPath)).json()).results.length).toBe(1)
  const taskId = (await (await page.request.get(formPath)).json()).results[0].taskId
  const taskPath = `${base}/tasks/${taskId}`
  const split = await publish(page, base, `拆分检查 ${suffix}`, graph([
    { id: 'start', data: { kind: 'start', inputVariables: [{ name: 'tasks', valueType: 'array[object]', value: [{ title: '核对增长曲线', description: '记录观测结果' }] }] } },
    { id: 'output', data: { kind: 'output', outputs: [{ name: 'tasks', variableSelector: ['start', 'tasks'] }] } },
  ]))
  const decomposed = await page.request.post(`${taskPath}/decompositions`, { data: { workflowId: split }, headers: { 'Idempotency-Key': `split-${suffix}` } })
  expect(decomposed.status(), await decomposed.text()).toBe(201)
  const handling = await publish(page, base, `处理方案 ${suffix}`, graph([
    { id: 'start', data: { kind: 'start', inputVariables: [{ name: 'branch', valueType: 'string', value: 'main' }] } },
    { id: 'output', data: { kind: 'output', outputs: [{ name: 'result', variableSelector: ['start', 'branch'] }] } },
  ]))
  await page.goto(`/collaboration/tasks/${taskId}?tab=workflows`)
  await page.getByRole('button', { name: '用工作流处理', exact: true }).click()
  const dialog = page.getByRole('dialog')
  await expect(dialog.getByLabel('工作流')).toHaveValue('')
  await expect(dialog.getByRole('button', { name: '启动工作流' })).toBeDisabled()
  await dialog.getByLabel('工作流').selectOption(handling)
  await dialog.getByLabel('需求').fill('核对日志增长来源，先形成清理方案，由负责人验收。')
  await dialog.getByLabel('branch', { exact:true }).fill('feature/task-link')
  await page.screenshot({ path: '../.tmp/screens/task-workflow-dialog.png', fullPage: true })
  await dialog.getByRole('button', { name: '启动工作流' }).click()
  await expect(dialog).toBeHidden()
  await expect(page.getByRole('region', { name: '工作流运行' })).toBeVisible()
  await expect(page.getByRole('link', { name: '返回任务', exact: true })).toHaveAttribute('href', new RegExp(`/collaboration/tasks/${taskId}`))
  await page.getByRole('button', { name: '返回工作流列表' }).click()
  const list = page.getByRole('list', { name: '工作流运行记录' })
  await expect(list.getByRole('listitem')).toHaveCount(3)
  await expect(list).toContainText('巡检来源 · v1')
  await expect(list).toContainText('拆分 · v1')
  await expect(list).toContainText('处理 · v1')
  await expect.poll(async () => (await (await page.request.get(taskPath)).json()).status).toBe('review')
  const linked = (await (await page.request.get(`${taskPath}/workflow-runs`)).json()).items.find((r: { kind: string }) => r.kind === 'handle')
  const detail = await (await page.request.get(`${base}/workflow-runs/${linked.workflowRunId}`)).json()
  expect(JSON.parse(detail.run.output)).toEqual({ result: 'feature/task-link' })
  await page.screenshot({ path: '../.tmp/screens/task-workflows-three-kinds.png', fullPage: true })

  // Same project and title, but a distinct persisted task identity.
  const second = await page.request.post(`${base}/tasks`, { data: { title, projectId, assignee: { type: 'human', id: user } } })
  expect(second.status()).toBe(201)
  const secondId = (await second.json()).id
  const secondStart = await page.request.post(`${base}/tasks/${secondId}/workflow-runs`, { data: { workflowId: handling, input: '另一个任务' }, headers: { 'Idempotency-Key': `second-${suffix}` } })
  expect(secondStart.status(), await secondStart.text()).toBe(201)
  const secondRun = (await secondStart.json()).workflowRunId
  const secondRuns = (await (await page.request.get(`${base}/tasks/${secondId}/workflow-runs`)).json()).items
  expect(secondRuns.map((r: { workflowRunId: string }) => r.workflowRunId)).toEqual([secondRun])
  const firstRuns = (await (await page.request.get(`${taskPath}/workflow-runs`)).json()).items
  expect(firstRuns).toHaveLength(3)
  expect(firstRuns.some((r: { workflowRunId: string }) => r.workflowRunId === secondRun)).toBe(false)

  await page.setViewportSize({ width: 390, height: 844 })
  await noHorizontalScroll(page)
  const navigation=page.getByRole('navigation',{name:'任务视图'})
  for(const link of await navigation.getByRole('link').all()){
    await expect(link).toBeInViewport({ratio:1})
    expect(await link.evaluate(el=>el.scrollWidth<=el.clientWidth)).toBe(true)
  }
  const toggleLabel=page.locator('.mc-composer-toggle-label')
  await expect(toggleLabel).toBeVisible()
  expect(await toggleLabel.evaluate(el=>el.clientHeight<=parseFloat(getComputedStyle(el).lineHeight)+1)).toBe(true)
  for(const time of await list.locator('time').all()){
    expect(await time.evaluate(el=>el.scrollWidth<=el.clientWidth)).toBe(true)
    expect(await time.textContent()).toMatch(/\d{4}/)
  }
  await expect(page.locator('.mc-description')).not.toContainText('/chat?run=')
  await page.screenshot({ path: '../.tmp/screens/task-workflows-390.png', fullPage: true })
  await toggleLabel.scrollIntoViewIfNeeded()
  await page.screenshot({ path: '../.tmp/screens/task-composer-390.png', fullPage: true })
  await list.getByRole('button', { name: /查看工作流运行 巡检来源/ }).click()
  await expect(page).toHaveURL(new RegExp(`/collaboration/tasks/${taskId}.*workflow=${sourceId}`))
  await expect(page.getByRole('region', { name: '报告与问题确认' })).toBeVisible()
  await noHorizontalScroll(page)
  await page.screenshot({ path: '../.tmp/screens/task-source-390.png', fullPage: true })
  await page.getByRole('button', { name: '工作区', exact: true }).click()
  const workspace = page.getByRole('complementary', { name: '工作区' })
  await expect(workspace).toBeVisible()
  expect(await workspace.evaluate(el => el.clientWidth)).toBeGreaterThan(280)
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true)
  await page.screenshot({ path: '../.tmp/screens/task-workspace-390.png', fullPage: true })
})

test('the starter stops real handling from the task tab and can immediately move the refreshed task', async ({ page }) => {
  test.setTimeout(180_000)
  await page.goto('/collaboration')
  await signIn(page)
  const tid = (await (await page.request.get('/api/v1/me/tenants')).json()).items[0].id
  const user = (await (await page.request.get('/api/v1/me')).json()).id
  const base = `/api/v2/tenants/${tid}`
  const projectId = (await (await page.request.get(`${base}/projects`)).json()).items[0].id
  const suffix = crypto.randomUUID().slice(0, 8)
  const created = await page.request.post(`${base}/tasks`, { data: { title: `可停止的处理 ${suffix}`, projectId, assignee: { type: 'human', id: user } } })
  expect(created.status()).toBe(201)
  const taskId = (await created.json()).id
  const withAgent = !!process.env.V2_NODE_ASSETS_DIR
  const nodes = [
    { id: 'start', data: { kind: 'start', inputVariables: [{ name: 'findings', valueType: 'array[object]', value: [{ key: suffix, title: '核对结果', description: '等待负责人确认' }] }] } },
    ...(withAgent ? [{ id: 'agent', data: { kind: 'agent', agentConfig: { executor: { agentCli: 'official/ora-space.opencode', modelId: 'fixture' }, prompt: 'Reply with a short review summary.' } } }] : []),
    { id: 'review', data: { kind: 'form', formConfig: { title: '等待处理确认', submitLabel: '确认结果', reportSelector: ['start', 'input'], itemsSelector: ['start', 'findings'], fields: [] } } },
    { id: 'out', data: { kind: 'output', outputs: [{ name: 'result', variableSelector: ['start', 'input'] }] } },
  ]
  const workflowId = await publish(page, base, `可停止流程 ${suffix}`, graph(nodes))
  await page.goto(`/collaboration/tasks/${taskId}?tab=workflows`)
  await page.getByRole('button', { name: '用工作流处理', exact: true }).click()
  const dialog = page.getByRole('dialog')
  await dialog.getByLabel('工作流').selectOption(workflowId)
  await dialog.getByLabel('需求').fill('检查并等待负责人确认。')
  await dialog.getByRole('button', { name: '启动工作流' }).click()
  await expect(dialog).toBeHidden()
  await expect(page.locator('.mc-task-bar .mc-status')).toHaveText('进行中')
  const runId = new URL(page.url()).searchParams.get('workflow')!
  await expect.poll(async () => {
    const detail = await (await page.request.get(`${base}/workflow-runs/${runId}`)).json()
    expect(['failed', 'cancelled', 'interrupted']).not.toContain(detail.run.status)
    return detail.run.status
  }, { timeout: 120_000 }).toBe('awaitingInput')
  if (withAgent) {
    const detail = await (await page.request.get(`${base}/workflow-runs/${runId}`)).json()
    const agent = detail.nodes.find((n: { nodeId: string }) => n.nodeId === 'agent')
    expect(agent.status).toBe('succeeded')
    expect(agent.sessionId).toBeTruthy()
  }
  await page.screenshot({ path: '../.tmp/screens/task-handling-live.png', fullPage: true })
  const cancelled = page.waitForResponse(r => r.request().method() === 'POST' && r.url().endsWith(`/workflow-runs/${runId}/cancel`))
  await page.getByRole('button', { name: '停止', exact: true }).click()
  expect((await cancelled).status()).toBe(200)
  await expect(page.locator('.mc-task-bar .mc-status')).toHaveText('待处理', { timeout: 3000 })
  await expect(page.getByRole('button', { name: '停止', exact: true })).toHaveCount(0)
  await page.screenshot({ path: '../.tmp/screens/task-handling-stopped.png', fullPage: true })
  const move = page.waitForResponse(r => r.request().method() === 'POST' && r.url().endsWith(`/tasks/${taskId}/status`))
  await page.getByRole('button', { name: '开始处理', exact: true }).click()
  expect((await move).status()).toBe(200)
  await expect(page.locator('.mc-task-bar .mc-status')).toHaveText('进行中')
  // A stopped handling run cannot resume (owner decision D25): its task settled on it, so the task
  // starts handling again instead. The server refuses, and the run page does not offer it.
  const preview = await (await page.request.get(`${base}/workflow-runs/${runId}/resume-preview`)).json()
  expect(preview).toMatchObject({ resumable: false, reason: 'task_handling' })
  const resumed = await page.request.post(`${base}/workflow-runs/${runId}/resume`)
  expect(resumed.status()).toBe(409)
  expect(await resumed.json()).toMatchObject({ code: 'workflow_run_not_resumable', params: { reason: 'task_handling' } })
  await page.goto(`/chat?run=${runId}`)
  const pane = page.getByRole('region', { name: '工作流运行' })
  await expect(pane.getByRole('link', { name: '返回任务', exact: true })).toBeVisible()
  await expect(pane.getByRole('button', { name: '再跑一次', exact: true })).toBeVisible()
  await expect(pane.getByRole('button', { name: '从失败处继续', exact: true })).toHaveCount(0)
  await page.screenshot({ path: '../.tmp/screens/resume-task-handling.png' })
})
