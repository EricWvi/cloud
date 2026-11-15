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

test('a member who did not start the run stops it from the task tab', async ({ page, browser }) => {
  test.setTimeout(120_000)
  await page.goto('/collaboration')
  await signIn(page)
  const tid = (await (await page.request.get('/api/v1/me/tenants')).json()).items[0].id
  const owner = (await (await page.request.get('/api/v1/me')).json()).id
  const base = `/api/v2/tenants/${tid}`
  const projectId = (await (await page.request.get(`${base}/projects`)).json()).items[0].id
  const suffix = crypto.randomUUID().slice(0, 8)
  const email = `stop-${suffix}@demo.ora-space.dev`
  const invited = await page.request.post(`${base}/members`, { data: { email, displayName: `停止成员-${suffix}` } })
  expect(invited.ok(), await invited.text()).toBeTruthy()
  const created = await page.request.post(`${base}/tasks`, { data: { title: `他人停止 ${suffix}`, projectId, assignee: { type: 'human', id: owner } } })
  expect(created.status(), await created.text()).toBe(201)
  const taskId = (await created.json()).id
  const workflowId = await publish(page, base, `他人停止流程 ${suffix}`, graph([
    { id: 'start', data: { kind: 'start', inputVariables: [{ name: 'findings', valueType: 'array[object]', value: [{ key: suffix, title: '核对', description: '等待' }] }] } },
    { id: 'review', data: { kind: 'form', formConfig: { title: '等待确认', submitLabel: '确认', reportSelector: ['start', 'input'], itemsSelector: ['start', 'findings'], fields: [] } } },
    { id: 'out', data: { kind: 'output', outputs: [{ name: 'result', variableSelector: ['start', 'input'] }] } },
  ]))
  await page.goto(`/collaboration/tasks/${taskId}?tab=workflows`)
  await page.getByRole('button', { name: '用工作流处理', exact: true }).click()
  const dialog = page.getByRole('dialog')
  await dialog.getByLabel('工作流').selectOption(workflowId)
  await dialog.getByLabel('需求').fill('等待确认。')
  await dialog.getByRole('button', { name: '启动工作流' }).click()
  await expect(dialog).toBeHidden()
  const runId = new URL(page.url()).searchParams.get('workflow')!
  await expect.poll(async () => (await (await page.request.get(`${base}/workflow-runs/${runId}`)).json()).run.status, { timeout: 60_000 }).toBe('awaitingInput')

  const other = await (await browser.newContext()).newPage()
  await other.goto(`/collaboration/tasks/${taskId}?tab=workflows`)
  await signIn(other, email)
  await other.goto(`/collaboration/tasks/${taskId}?tab=workflows`)
  await expect(other.locator('.mc-task-bar .mc-status')).toHaveText('进行中')
  // The manual move is refused while the run is live, for the non-starter too.
  const manual = await other.request.post(`${base}/tasks/${taskId}/status`, { data: { status: 'review', version: (await (await other.request.get(`${base}/tasks/${taskId}`)).json()).version } })
  expect(manual.status()).toBe(409)
  await other.screenshot({ path: '../.tmp/screens/nonstarter-live.png', fullPage: true })
  const cancelled = other.waitForResponse(r => r.request().method() === 'POST' && r.url().endsWith(`/workflow-runs/${runId}/cancel`))
  await other.getByRole('button', { name: '停止', exact: true }).click()
  expect((await cancelled).status()).toBe(200)
  await expect(other.locator('.mc-task-bar .mc-status')).toHaveText('待处理', { timeout: 5000 })
  await expect(other.getByRole('button', { name: '停止', exact: true })).toHaveCount(0)
  await other.screenshot({ path: '../.tmp/screens/nonstarter-stopped.png', fullPage: true })
  await other.context().close()
  // The starter's page converges too.
  await page.reload()
  await expect(page.locator('.mc-task-bar .mc-status')).toHaveText('待处理')
  const runs = (await (await page.request.get(`${base}/tasks/${taskId}/workflow-runs`)).json()).items
  expect(runs[0]).toMatchObject({ kind: 'handle', status: 'cancelled', startedBy: owner })
})
