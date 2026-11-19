import { expect, test, type Page } from '@playwright/test'
import { signIn } from './login.ts'

/** Executes one explicit selection through the real workflow host, with retry enabled. */
async function startMcpRun(page: Page, base: string, executor: { agentId: string; modelId: string }, mcpId: string, title: string) {
  const graph = JSON.stringify({
    nodes: [
      { id: 'start', type: 'workflow', position: { x: 0, y: 0 }, data: { kind: 'start', title: '开始' } },
      { id: 'agent', type: 'workflow', position: { x: 320, y: 0 }, data: { kind: 'agent', title, agentConfig: {
        executor: { agentCli: executor.agentId, modelId: executor.modelId }, prompt: 'Reply with a short greeting.',
        retry: { enabled: true, maxRetries: 2, initialDelaySeconds: 0 },
        mcps: [{ mcpId, enabled: true }],
      } } },
      { id: 'out', type: 'workflow', position: { x: 640, y: 0 }, data: { kind: 'output', title: '输出', outputs: [{ name: 'answer', variableSelector: ['agent', 'output'] }] } },
    ],
    edges: [{ source: 'start', target: 'agent' }, { source: 'agent', target: 'out' }],
  })
  const created = await page.request.post(`${base}/workflows`, { data: { name: `${title} ${Date.now()}`, graph } })
  expect(created.status()).toBe(201)
  const wid = (await created.json()).workflow.id
  expect((await page.request.post(`${base}/workflows/${wid}/publish`, { data: { version: 'v1' } })).ok()).toBe(true)
  const project = (await (await page.request.get(`${base}/projects?limit=100`)).json()).items[0]
  const run = await page.request.post(`${base}/workflows/${wid}/runs`, { data: { projectId: project.id, name: title, input: 'hello' } })
  expect(run.status()).toBe(201)
  const rid = (await run.json()).run.id
  expect((await page.request.post(`${base}/workflow-runs/${rid}/start`)).ok()).toBe(true)
  return rid as string
}

/** Opens the failed node's real inspector instead of asserting an HTTP payload alone. */
async function openMcpFailure(page: Page, runId: string, title: string) {
  await page.goto(`/chat?run=${runId}`)
  const pane = page.getByRole('region', { name: '工作流运行' })
  await pane.getByRole('button', { name: '全图', exact: true }).click()
  await pane.getByLabel('工作流全图俯瞰').getByText(title, { exact: true }).click()
  const openDetails = pane.getByRole('button', { name: '打开节点详情' })
  if (await openDetails.isVisible()) await openDetails.click()
  const diagnostics = pane.getByLabel('MCP 失败详情').filter({ visible: true })
  await expect(pane.getByText('这类失败不会自动重试').filter({ visible: true })).toBeVisible()
  await diagnostics.scrollIntoViewIfNeeded()
  return diagnostics
}

test('the deployed UI installs a real MCP and preserves its workflow binding after removal', async ({ page }, info) => {
  test.setTimeout(120_000)
  await page.setViewportSize({ width: 1440, height: 1000 })
  await page.goto('/settings/plugins')
  await signIn(page)
  // Only the deployment-mode signal is stubbed; authentication, installation and settings use the real server.
  await page.route('**/auth/providers', (route) => route.fulfill({ json: { providers: ['huawei-idaas'] } }))
  const plugin = page.getByRole('article', { name: 'Tavily', exact: true }).filter({ has: page.getByText('official/ora-space.tavily-search', { exact: true }) })
  const tid = (await (await page.request.get('/api/v1/me/tenants')).json()).items[0].id
  const sid = (await (await page.request.get(`/api/v1/tenants/${tid}/spaces`)).json()).items[0].id
  const mcpPath = `/api/v1/tenants/${tid}/spaces/${sid}/mcp`
  const existing = (await (await page.request.get(`/api/v1/tenants/${tid}/mcp`)).json()).items
  const prior = existing.find((item: { id: string }) => item.id === 'official/ora-space.tavily-search')
  if (prior) expect((await page.request.delete(mcpPath, { data: { identifier: prior.id, revision: prior.revision } })).ok()).toBe(true)
  await page.reload()
  const navigation = page.getByRole('navigation', { name: '设置导航' })
  // Roles are the workspace's server catalog, so a deployed install keeps that page; only the
  // browser-local model connections stay hidden.
  await expect(navigation.getByRole('link', { name: '角色', exact: true })).toBeVisible()
  await expect(navigation.getByRole('link', { name: '模型连接', exact: true })).toHaveCount(0)
  await expect(navigation.getByRole('link', { name: 'Plugins', exact: true })).toBeVisible()
  const installing = page.waitForResponse((response) => response.request().method() === 'POST' && new URL(response.url()).pathname === mcpPath)
  await plugin.getByRole('button', { name: '安装', exact: true }).click()
  const installedResponse = await installing
  expect(installedResponse.status(), await installedResponse.text()).toBe(200)
  await expect(plugin.getByText('已安装', { exact: true })).toBeVisible({ timeout: 90_000 })
  await plugin.getByRole('button', { name: '查看插件 Tavily' }).click()
  await expect(page.getByText('MCP 配置', { exact: true })).toBeVisible()
  const secrets = page.locator('input[type=password]')
  await expect(secrets).toHaveCount(1)
  for (let i = 0; i < await secrets.count(); i++) await secrets.nth(i).fill('mcp-e2e-fixture-token')
  await page.getByRole('button', { name: '保存 MCP 配置', exact: true }).click()
  await expect(page.getByText('MCP 配置已保存')).toBeVisible()
  await expect(secrets.first()).toHaveValue('')
  await page.reload()
  // The detail selection is in the URL and survives refresh.
  await expect(page.getByText('秘密值已配置')).toBeVisible()
  await expect(secrets.first()).toHaveValue('')
  await page.screenshot({ path: info.outputPath('1201-mcp-settings.png'), animations: 'disabled' })
  const base = `/api/v2/tenants/${tid}/workflows`
  const name = `MCP 真实安装-${Date.now()}`
  const response = await page.request.post(base, { data: { name, graph: null } })
  expect(response.status()).toBe(201)
  const workflowId = (await response.json()).workflow.id
  await page.goto('/workflows')
  await page.getByRole('complementary', { name: '工作流列表' }).getByRole('button', { name, exact: true }).click()
  await expect(page.getByLabel('工作流名称', { exact: true })).toHaveValue(name)
  await page.getByRole('toolbar', { name: '添加工作流节点' }).getByRole('button', { name: 'Agent', exact: true }).click()
  const agentNodeId = await page.locator('.react-flow__node.selected').getAttribute('data-id')
  expect(agentNodeId).toBeTruthy()
  await expect(page.getByLabel('角色', { exact: true })).toBeVisible()
  await page.getByRole('button', { name: '添加 MCP', exact: true }).click()
  await page.getByRole('option', { name: /official\/ora-space.tavily-search/ }).click()
  await expect(page.getByRole('switch', { name: /Tavily/ })).toBeChecked()
  // The prior "saved" label can remain visible until the debounced save starts. Confirm
  // this exact binding is durable before removing the package and reloading the editor.
  await expect.poll(async () => {
    const draft = (await (await page.request.get(`${base}/${workflowId}/draft`)).json()).snapshot
    return JSON.parse(draft.graph).nodes.find((node: { id: string }) => node.id === agentNodeId)?.data.agentConfig.mcps
  }).toEqual([{ mcpId: 'official/ora-space.tavily-search', enabled: true }])
  await page.screenshot({ path: info.outputPath('1201-mcp.png'), animations: 'disabled' })
  const installed = (await (await page.request.get(`/api/v1/tenants/${tid}/mcp`)).json()).items.find((item: { id: string }) => item.id === 'official/ora-space.tavily-search')
  expect(JSON.stringify(installed)).not.toContain('mcp-e2e-fixture-token')
  expect((await page.request.delete(mcpPath, { data: { identifier: installed.id, revision: installed.revision } })).ok()).toBe(true)
  await page.reload()
  await page.getByRole('complementary', { name: '工作流列表' }).getByRole('button', { name, exact: true }).click()
  await page.locator(`.react-flow__node[data-id="${agentNodeId}"]`).click()
  await expect(page.getByText('official/ora-space.tavily-search', { exact: true }).first()).toBeVisible()
  await expect(page.getByText('插件未安装或不可用', { exact: true })).toBeVisible()
  await expect(page.getByRole('switch', { name: /official\/ora-space.tavily-search/ })).toBeChecked()
})

test('an enabled unavailable MCP fails with actionable diagnostics', async ({ page }, info) => {
  test.setTimeout(180_000)
  await page.setViewportSize({ width: 1600, height: 1000 })
  await page.goto('/workflows')
  await signIn(page)
  const tid = (await (await page.request.get('/api/v1/me/tenants')).json()).items[0].id
  const base = `/api/v2/tenants/${tid}`
  const executors = (await (await page.request.get(`${base}/chat/executors`)).json()).items
  test.skip(executors.length === 0, 'prepared Node assets are required for Agent execution')
  const rid = await startMcpRun(page, base, executors[0], 'official/ora-space.uninstalled-test-mcp', '缺少 MCP 的 Agent')
  const detail = async () => (await page.request.get(`${base}/workflow-runs/${rid}`)).json()
  await expect.poll(async () => (await detail()).run.status, { timeout: 120_000 }).toBe('failed')
  const node = (await detail()).nodes.find((n: { nodeId: string }) => n.nodeId === 'agent')
  expect(JSON.parse(node.payload).error_detail).toMatchObject({
    code: 'mcp_plugin_missing', auto_retryable: false,
    plugin_id: 'official/ora-space.uninstalled-test-mcp',
  })
  const diagnostics = await openMcpFailure(page, rid, '缺少 MCP 的 Agent')
  await expect(diagnostics).toContainText('mcp_plugin_missing')
  await expect(diagnostics).toContainText('official/ora-space.uninstalled-test-mcp')
  await page.screenshot({ path: info.outputPath('1201-mcp-failure.png'), animations: 'disabled' })
})

test('the Controller rejects a configured MCP before creating a session', async ({ page }, info) => {
  test.setTimeout(180_000)
  await page.setViewportSize({ width: 1600, height: 1000 })
  await page.goto('/settings/plugins')
  await signIn(page)
  const tid = (await (await page.request.get('/api/v1/me/tenants')).json()).items[0].id
  const sid = (await (await page.request.get(`/api/v1/tenants/${tid}/spaces`)).json()).items[0].id
  const base = `/api/v2/tenants/${tid}`
  const executors = (await (await page.request.get(`${base}/chat/executors`)).json()).items
  test.skip(executors.length === 0, 'prepared Node assets are required for the Agent executor catalog')
  // The pinned Node supports session_mcp_lifecycle, but the Controller lifecycle adapter is
  // still gated. Keep rejecting before session creation until that adapter is implemented.
  const identifier = 'official/ora-space.tavily-search'
  const mcpPath = `/api/v1/tenants/${tid}/spaces/${sid}/mcp`
  const list = async () => (await (await page.request.get(`/api/v1/tenants/${tid}/mcp`)).json()).items
  let installation = (await list()).find((item: { id: string }) => item.id === identifier)
  if (!installation) {
    const installed = await page.request.post(mcpPath, { data: { identifier, revision: 0 } })
    expect(installed.status(), await installed.text()).toBe(200)
    installation = (await installed.json()).resource
  }
  expect(installation.state).toBe('installed')
  const secretIds = installation.settings.filter((setting: { secret: boolean }) => setting.secret).map((setting: { id: string }) => setting.id)
  expect(secretIds).toHaveLength(1)
  const values = Object.fromEntries(secretIds.map((id: string) => [id, 'mcp-e2e-lifecycle-fixture-token']))
  const configured = await page.request.put(mcpPath + '/settings', { data: { identifier, revision: installation.revision, values, clear: [] } })
  expect(configured.status()).toBe(200)
  const saved = (await list()).find((item: { id: string }) => item.id === identifier)
  expect(saved.complete).toBe(true)
  expect(saved.configuredSecrets).toEqual(secretIds)
  expect(JSON.stringify(saved)).not.toContain('mcp-e2e-lifecycle-fixture-token')

  try {
    const rid = await startMcpRun(page, base, executors[0], identifier, '已配置 MCP 的生命周期门禁')
    const detail = async () => (await page.request.get(`${base}/workflow-runs/${rid}`)).json()
    await expect.poll(async () => (await detail()).run.status, { timeout: 120_000 }).toBe('failed')
    const result = await detail()
    const node = result.nodes.find((item: { nodeId: string }) => item.nodeId === 'agent')
    const failure = JSON.parse(node.payload).error_detail
    expect(failure).toMatchObject({ code: 'mcp_lifecycle_not_ready', auto_retryable: false, attempt: 1 })
    expect(failure.plugin_id).toBeUndefined()
    expect(failure.transport).toBeUndefined()
    expect(node.sessionId).toBeNull()
    expect(JSON.stringify(result)).not.toContain('mcp-e2e-lifecycle-fixture-token')
    const diagnostics = await openMcpFailure(page, rid, '已配置 MCP 的生命周期门禁')
    await expect(diagnostics).toContainText('mcp_lifecycle_not_ready')
    await expect(diagnostics).toContainText('当前服务尚未支持 MCP 生命周期，暂时无法运行带 MCP 的节点。')
    await expect(diagnostics).not.toContainText('mcp_plugin_missing')
    await page.screenshot({ path: info.outputPath('1201-mcp-lifecycle.png'), animations: 'disabled' })
  } finally {
    expect((await page.request.delete(mcpPath, { data: { identifier, revision: saved.revision } })).ok()).toBe(true)
  }
})
