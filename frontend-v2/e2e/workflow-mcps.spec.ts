import { expect, test } from '@playwright/test'
import { signIn } from './login.ts'

test('installs a real MCP and preserves its workflow binding after removal', async ({ page }, info) => {
  test.setTimeout(120_000)
  await page.setViewportSize({ width: 1440, height: 1000 })
  await page.goto('/settings/plugins')
  await signIn(page)
  const plugin = page.getByRole('article', { name: 'Tavily', exact: true }).filter({ has: page.getByText('official/ora-space.tavily-search', { exact: true }) })
  const tid = (await (await page.request.get('/api/v1/me/tenants')).json()).items[0].id
  const sid = (await (await page.request.get(`/api/v1/tenants/${tid}/spaces`)).json()).items[0].id
  const mcpPath = `/api/v1/tenants/${tid}/spaces/${sid}/mcp`
  const existing = (await (await page.request.get(`/api/v1/tenants/${tid}/mcp`)).json()).items
  const prior = existing.find((item: { id: string }) => item.id === 'official/ora-space.tavily-search')
  if (prior) expect((await page.request.delete(mcpPath, { data: { identifier: prior.id, revision: prior.revision } })).ok()).toBe(true)
  await page.reload()
  await plugin.getByRole('button', { name: '安装', exact: true }).click()
  await expect(plugin.getByText('已安装', { exact: true })).toBeVisible({ timeout: 90_000 })
  await plugin.getByRole('button', { name: '查看插件 Tavily' }).click()
  await expect(page.getByText('MCP 配置', { exact: true })).toBeVisible()
  const secrets = page.locator('input[type=password]')
  for (let i = 0; i < await secrets.count(); i++) await secrets.nth(i).fill('mcp-e2e-fixture-token')
  if (await secrets.count()) {
    await page.getByRole('button', { name: '保存 MCP 配置', exact: true }).click()
    await expect(page.getByText('MCP 配置已保存')).toBeVisible()
    await expect(secrets.first()).toHaveValue('')
  }
  const base = `/api/v2/tenants/${tid}/workflows`
  const name = `MCP 真实安装-${Date.now()}`
  const response = await page.request.post(base, { data: { name, graph: null } })
  expect(response.status()).toBe(201)
  await page.goto('/workflows')
  await page.getByRole('complementary', { name: '工作流列表' }).getByRole('button', { name, exact: true }).click()
  await expect(page.getByLabel('工作流名称', { exact: true })).toHaveValue(name)
  await page.getByRole('toolbar', { name: '添加工作流节点' }).getByRole('button', { name: 'Agent', exact: true }).click()
  await page.getByRole('button', { name: '添加 MCP', exact: true }).click()
  await page.getByRole('option', { name: /official\/ora-space.tavily-search/ }).click()
  await expect(page.getByRole('switch', { name: /Tavily/ })).toBeChecked()
  await expect(page.getByText(/已实时保存/)).toBeVisible()
  await page.screenshot({ path: info.outputPath('1201-mcp.png'), animations: 'disabled' })
  const installed = (await (await page.request.get(`/api/v1/tenants/${tid}/mcp`)).json()).items.find((item: { id: string }) => item.id === 'official/ora-space.tavily-search')
  expect(JSON.stringify(installed)).not.toContain('mcp-e2e-fixture-token')
  expect((await page.request.delete(mcpPath, { data: { identifier: installed.id, revision: installed.revision } })).ok()).toBe(true)
  await page.reload()
  await expect(page.getByText('official/ora-space.tavily-search', { exact: true })).toBeVisible()
})

test('an enabled unavailable MCP fails with actionable diagnostics', async ({ page }) => {
  test.setTimeout(180_000)
  await page.goto('/workflows')
  await signIn(page)
  const tid = (await (await page.request.get('/api/v1/me/tenants')).json()).items[0].id
  const base = `/api/v2/tenants/${tid}`
  const executors = (await (await page.request.get(`${base}/chat/executors`)).json()).items
  test.skip(executors.length === 0, 'prepared Node assets are required for Agent execution')
  const executor = executors[0]
  const graph = JSON.stringify({
    nodes: [
      { id: 'start', data: { kind: 'start' } },
      { id: 'agent', data: { kind: 'agent', agentConfig: {
        executor: { agentCli: executor.agentId, modelId: executor.modelId },
        prompt: 'Reply with a short greeting.',
        mcps: [{ mcpId: 'official/ora-space.uninstalled-test-mcp', enabled: true }],
      } } },
      { id: 'out', data: { kind: 'output', outputs: [{ name: 'answer', variableSelector: ['agent', 'output'] }] } },
    ],
    edges: [{ source: 'start', target: 'agent' }, { source: 'agent', target: 'out' }],
  })
  const created = await page.request.post(`${base}/workflows`, { data: { name: `MCP unavailable ${Date.now()}`, graph } })
  expect(created.status()).toBe(201)
  const wid = (await created.json()).workflow.id
  expect((await page.request.post(`${base}/workflows/${wid}/publish`, { data: { version: 'v1' } })).ok()).toBe(true)
  const project = (await (await page.request.get(`${base}/projects?limit=100`)).json()).items[0]
  const run = await page.request.post(`${base}/workflows/${wid}/runs`, { data: { projectId: project.id, name: 'MCP unavailable', input: 'hello' } })
  expect(run.status()).toBe(201)
  const rid = (await run.json()).run.id
  expect((await page.request.post(`${base}/workflow-runs/${rid}/start`)).ok()).toBe(true)
  const detail = async () => (await page.request.get(`${base}/workflow-runs/${rid}`)).json()
  await expect.poll(async () => (await detail()).run.status, { timeout: 120_000 }).toBe('failed')
  const node = (await detail()).nodes.find((n: { nodeId: string }) => n.nodeId === 'agent')
  expect(JSON.parse(node.payload).error_detail).toMatchObject({
    code: 'mcp_plugin_missing', auto_retryable: false,
    plugin_id: 'official/ora-space.uninstalled-test-mcp',
  })
})
