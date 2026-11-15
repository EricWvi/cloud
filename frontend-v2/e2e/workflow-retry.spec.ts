import { expect, test, type Page } from '@playwright/test'
import { Client } from 'pg'
import { randomUUID } from 'node:crypto'
import { resolve } from 'node:path'
import { signIn } from './login.ts'

// PostgreSQL projection fixture: timer/dispatch behavior is covered through the real store/host
// in retry_test.go. This browser case reads persisted waits/history through the real HTTP server.
/** Publishes a one-Agent workflow and creates a pending run of it through the real HTTP server. */
async function createRun(page: Page): Promise<{ tid: string, rid: string }> {
  await page.goto('/chat')
  await signIn(page)
  const tid = (await (await page.request.get('/api/v1/me/tenants')).json()).items[0].id
  const base = `/api/v2/tenants/${tid}`
  const project = (await (await page.request.get(`${base}/projects?limit=100`)).json()).items[0]
  const graph = JSON.stringify({ nodes: [
    { id: 'start', type: 'workflow', position: { x: 0, y: 0 }, data: { kind: 'start', title: '开始' } },
    { id: 'agent', type: 'workflow', position: { x: 320, y: 0 }, data: { kind: 'agent', title: '重试任务', agentConfig: { prompt: 'retry task' } } },
    { id: 'out', type: 'workflow', position: { x: 640, y: 0 }, data: { kind: 'output', title: '输出' } },
  ], edges: [{ id: 'a', source: 'start', target: 'agent' }, { id: 'b', source: 'agent', target: 'out' }] })
  const created = await page.request.post(`${base}/workflows`, { data: { name: `重试验收 ${Date.now()}`, graph } })
  expect(created.status()).toBe(201)
  const wid = (await created.json()).workflow.id
  expect((await page.request.post(`${base}/workflows/${wid}/publish`, { data: { version: 'v1' } })).ok()).toBe(true)
  const run = await page.request.post(`${base}/workflows/${wid}/runs`, { data: { projectId: project.id, name: '自动重试验收' } })
  expect(run.status()).toBe(201)
  return { tid, rid: (await run.json()).run.id }
}

const database = () => new Client({ connectionString: process.env.E2E_DATABASE_URL ?? 'postgres://ora:ora-local@127.0.0.1:55432/ora_v2_e2e?sslmode=disable' })

test('root retry wait counts down and retains replaced attempt sessions and failure causes', async ({ page }) => {
  const { tid, rid } = await createRun(page)
  const db = database()
  await db.connect()
  try {
    await db.query(`INSERT INTO v2_workflow_node_runs (tenant_id,run_id,scope_id,node_id,node_type,status,output,started_at,finished_at)
      VALUES ($1,$2,$3,'start','start','succeeded','',now(),now())`, [tid, rid, `root:${rid}`])
    const previous = randomUUID(), current = randomUUID(), now = Date.now()
    const session = `retry-history-${previous}`
    const failure = { error_detail: { kind: 'session', message: '模型请求失败', source_chain: ['上游拒绝请求', '连接超时'], attempt: 1, auto_retryable: true, recorded_at: now } }
    await db.query(`INSERT INTO v2_workflow_node_runs (id,tenant_id,run_id,scope_id,node_id,node_type,status,session_id,error,payload,started_at,finished_at,deleted_at)
      VALUES ($1,$2,$3,$4,'agent','agent','failed',$5,'模型请求失败',$6,now(),now(),now())`, [previous, tid, rid, `root:${rid}`, session, JSON.stringify(failure)])
    const payload = { retry_wait: { attempt: 2, max_attempt: 3, retry: 1, max_retries: 2, delay_ms: 120000, scheduled_at: now, due_at: now + 120000, previous_node_run_id: previous }, auto_retry: { retry: 1, max_retries: 2 }, retry_chain: [previous] }
    await db.query(`INSERT INTO v2_workflow_node_runs (id,tenant_id,run_id,scope_id,node_id,node_type,status,payload) VALUES ($1,$2,$3,$4,'agent','agent','running',$5)`, [current, tid, rid, `root:${rid}`, JSON.stringify(payload)])
    await db.query(`UPDATE v2_workflow_runs SET status='running',state='{"current_nodes":["agent"]}',started_at=now() WHERE id=$1`, [rid])
    await page.goto(`/chat?run=${rid}`)
    const pane = page.getByRole('region', { name: '工作流运行' })
    await expect(pane.locator('[data-retry-waiting]').first()).toBeVisible()
    const countdown = pane.locator('[data-retry-waiting]').first()
    const label = await countdown.textContent()
    await expect.poll(() => countdown.textContent()).not.toBe(label)
    await pane.getByRole('button', { name: '全图', exact: true }).click()
    await pane.getByLabel('工作流全图俯瞰').getByText('重试任务', { exact: true }).click()
    await pane.getByRole('button', { name: '打开节点详情' }).click()
    const history = pane.locator('[data-slot="failed-attempts"]').filter({ visible: true })
    await expect(history).toContainText('模型请求失败')
    await expect(history).toContainText('连接超时')
    await expect(history).toContainText(session)
    await page.screenshot({ path: resolve('../.tmp/screens/root-retry-wait.png'), animations: 'disabled', fullPage: true })
    await history.scrollIntoViewIfNeeded()
    await page.screenshot({ path: resolve('../.tmp/screens/root-retry-history.png'), animations: 'disabled', fullPage: true })
    const events = page.waitForResponse((res) => res.url().includes(`/nodes/${previous}/events`))
    await history.getByText(`查看会话 · ${session}`).click()
    // A fixture session is absent from execution; its replaced row was found and ownership is
    // still enforced. A wrong lookup would return workflow_node_run_not_found instead.
    expect((await (await events).json()).code).toBe('workflow_node_session_not_found')
    await page.reload()
    await expect(pane.locator('[data-retry-waiting]').first()).toBeVisible()
  } finally {
    await db.query(`UPDATE v2_workflow_runs SET status='cancelled',finished_at=now() WHERE id=$1`, [rid])
    await db.query(`UPDATE v2_workflow_node_runs SET status='cancelled',payload=payload-'retry_wait' WHERE run_id=$1 AND deleted_at IS NULL`, [rid])
    await db.end()
  }
})

// A wait left behind under a run that already failed can never fire: the page must not count it
// down or call its predecessor "scheduled".
test('a wait left under a failed run shows neither a countdown nor a scheduled retry', async ({ page }) => {
  const { tid, rid } = await createRun(page)
  const db = database()
  await db.connect()
  try {
    const previous = randomUUID(), current = randomUUID(), now = Date.now()
    const failure = { error_detail: { kind: 'session', message: '模型请求失败', source_chain: ['连接超时'], attempt: 1, auto_retryable: true, recorded_at: now } }
    await db.query(`INSERT INTO v2_workflow_node_runs (tenant_id,run_id,scope_id,node_id,node_type,status,output,started_at,finished_at)
      VALUES ($1,$2,$3,'start','start','succeeded','',now(),now())`, [tid, rid, `root:${rid}`])
    await db.query(`INSERT INTO v2_workflow_node_runs (id,tenant_id,run_id,scope_id,node_id,node_type,status,session_id,error,payload,started_at,finished_at,deleted_at)
      VALUES ($1,$2,$3,$4,'agent','agent','failed',$5,'模型请求失败',$6,now(),now(),now())`, [previous, tid, rid, `root:${rid}`, `retry-orphan-${previous}`, JSON.stringify(failure)])
    const payload = { retry_wait: { attempt: 2, max_attempt: 3, retry: 1, max_retries: 2, delay_ms: 120000, scheduled_at: now, due_at: now + 120000, previous_node_run_id: previous }, auto_retry: { retry: 1, max_retries: 2 }, retry_chain: [previous] }
    await db.query(`INSERT INTO v2_workflow_node_runs (id,tenant_id,run_id,scope_id,node_id,node_type,status,payload) VALUES ($1,$2,$3,$4,'agent','agent','running',$5)`, [current, tid, rid, `root:${rid}`, JSON.stringify(payload)])
    await db.query(`UPDATE v2_workflow_runs SET status='failed',error='另一分支失败',state='{"current_nodes":["agent"]}',started_at=now(),finished_at=now() WHERE id=$1`, [rid])
    await page.goto(`/chat?run=${rid}`)
    const pane = page.getByRole('region', { name: '工作流运行' })
    await pane.getByRole('button', { name: '全图', exact: true }).click()
    await pane.getByLabel('工作流全图俯瞰').getByText('重试任务', { exact: true }).click()
    await pane.getByRole('button', { name: '打开节点详情' }).click()
    const history = pane.locator('[data-slot="failed-attempts"]').filter({ visible: true })
    await expect(history).toContainText('模型请求失败')
    await expect(pane.locator('[data-retry-waiting]')).toHaveCount(0)
    await expect(history).not.toContainText('已安排自动重试')
    await history.scrollIntoViewIfNeeded()
    await page.screenshot({ path: resolve('../.tmp/screens/root-retry-abandoned.png'), animations: 'disabled', fullPage: true })
  } finally {
    await db.query(`UPDATE v2_workflow_node_runs SET status='cancelled',payload=payload-'retry_wait' WHERE run_id=$1 AND deleted_at IS NULL AND status='running'`, [rid])
    await db.end()
  }
})
