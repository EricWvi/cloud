import { expect, test } from '@playwright/test'
import { signIn } from './login.ts'

const agent = 'official/ora-space.opencode'
const executors = [
  { id: 'foreign', agentId: 'another-agent', agentName: 'Other Agent', modelId: 'foreign', modelName: 'Foreign' },
  { id: 'first', agentId: agent, agentName: 'OpenCode', modelId: 'configured', modelName: '当前模型' },
  { id: 'second', agentId: agent, agentName: 'OpenCode', modelId: 'second', modelName: '第二模型' },
]

for (const [label, model, expected] of [
  ['stale', 'deepseek/deepseek-v4-flash', 'configured'],
  ['blank', '', 'configured'],
  ['available', 'second', 'second'],
]) {
  test(`workflow editor repairs ${label} model on load using the same Agent`, async ({ page }) => {
    // Only the editor catalog is controlled; drafts, saves and publication use the real server.
    await page.route('**/api/v2/tenants/*/chat/executors', (route) => route.fulfill({ json: { items: executors } }))
    await page.goto('/workflows')
    await signIn(page)
    const tid = (await (await page.request.get('/api/v1/me/tenants')).json()).items[0].id
    const base = `/api/v2/tenants/${tid}/workflows`
    const name = `模型加载 ${label} ${Date.now()}`
    const graph = JSON.stringify({ nodes: [
      { id: 'start', type: 'workflow', position: { x: 0, y: 0 }, data: { kind: 'start', title: '开始' } },
      { id: 'agent', type: 'workflow', position: { x: 320, y: 0 }, data: { kind: 'agent', title: '模型检查', agentConfig: { executor: { agentCli: agent, modelId: model }, prompt: 'hello' } } },
    ], edges: [{ id: 'edge', source: 'start', target: 'agent' }] })
    const created = await page.request.post(base, { data: { name, graph } })
    expect(created.status()).toBe(201)
    const wid = (await created.json()).workflow.id
    const savedExecutor = async () => JSON.parse((await (await page.request.get(`${base}/${wid}/draft`)).json()).snapshot.graph).nodes
      .find((node: { id: string }) => node.id === 'agent').data.agentConfig.executor
    expect(await savedExecutor()).toEqual({ agentCli: agent, modelId: model })
    await page.reload()
    await page.getByRole('complementary', { name: '工作流列表' }).getByRole('button', { name, exact: true }).click()
    await page.locator('.react-flow__node[data-id="agent"]').click()
    await expect(page.getByLabel('Agent 模型', { exact: true })).toContainText(expected === 'second' ? '第二模型' : '当前模型')
    await expect(page.getByText('未保存', { exact: true })).toBeHidden()
    await page.screenshot({ path: `../.tmp/screens/model-hydration-${label}.png`, animations: 'disabled' })
    await page.getByRole('button', { name: '发布', exact: true }).click()
    await expect(page.getByRole('alertdialog', { name: '发布工作流' })).toBeVisible()
    expect(await savedExecutor()).toEqual({ agentCli: agent, modelId: expected })
  })
}

test('workflow editor shows empty model placeholder without desktop default when catalog is empty', async ({ page }, testInfo) => {
  await page.route('**/api/v2/tenants/*/chat/executors', (route) => route.fulfill({ json: { items: [] } }))
  await page.goto('/workflows')
  await signIn(page)
  const tid = (await (await page.request.get('/api/v1/me/tenants')).json()).items[0].id
  const base = `/api/v2/tenants/${tid}/workflows`
  const name = `空目录模型 ${Date.now()}`
  const graph = JSON.stringify({ nodes: [
    { id: 'start', type: 'workflow', position: { x: 0, y: 0 }, data: { kind: 'start', title: '开始' } },
  ], edges: [] })
  const created = await page.request.post(base, { data: { name, graph } })
  expect(created.status()).toBe(201)
  const wid = (await created.json()).workflow.id

  await page.reload()
  await page.getByRole('complementary', { name: '工作流列表' }).getByRole('button', { name, exact: true }).click()
  await page.getByRole('button', { name: 'Agent', exact: true }).click()
  const addedNode = page.locator('.react-flow__node').filter({ hasNotText: '开始' }).first()
  await expect(addedNode).toBeVisible()
  await addedNode.click()
  const modelButton = page.getByLabel('Agent 模型', { exact: true })
  await expect(modelButton).toContainText('没有可用 Agent 模型')
  await expect(modelButton).not.toContainText(' · ')
  await expect(modelButton).not.toContainText('gpt-5')
  await page.screenshot({ path: testInfo.outputPath('model-empty-catalog.png'), animations: 'disabled' })

  await page.getByRole('button', { name: '发布', exact: true }).click()
  await expect(page.getByRole('alertdialog', { name: '发布工作流' })).toBeVisible()
  const savedAgent = JSON.parse((await (await page.request.get(`${base}/${wid}/draft`)).json()).snapshot.graph).nodes
    .find((node: { data: { kind: string } }) => node.data.kind === 'agent')
  expect(savedAgent.data.agentConfig.executor).toEqual({ agentCli: '', modelId: '' })
})

test('workflow editor repairs stale model when executor catalog arrives late after workflow open', async ({ page }) => {
  let releaseCatalog!: () => void
  const catalogReady = new Promise<void>((resolve) => {
    releaseCatalog = resolve
  })
  await page.route('**/api/v2/tenants/*/chat/executors', async (route) => {
    await catalogReady
    await route.fulfill({ json: { items: executors } })
  })
  await page.goto('/workflows')
  await signIn(page)
  const tid = (await (await page.request.get('/api/v1/me/tenants')).json()).items[0].id
  const base = `/api/v2/tenants/${tid}/workflows`
  const name = `迟到目录 ${Date.now()}`
  const graph = JSON.stringify({ nodes: [
    { id: 'start', type: 'workflow', position: { x: 0, y: 0 }, data: { kind: 'start', title: '开始' } },
    { id: 'agent', type: 'workflow', position: { x: 320, y: 0 }, data: { kind: 'agent', title: '模型检查', agentConfig: { executor: { agentCli: agent, modelId: 'deepseek/deepseek-v4-flash' }, prompt: 'hello' } } },
  ], edges: [{ id: 'edge', source: 'start', target: 'agent' }] })
  const created = await page.request.post(base, { data: { name, graph } })
  expect(created.status()).toBe(201)
  const wid = (await created.json()).workflow.id

  await page.reload()
  await page.getByRole('complementary', { name: '工作流列表' }).getByRole('button', { name, exact: true }).click()
  await page.locator('.react-flow__node[data-id="agent"]').click()
  const modelButton = page.getByLabel('Agent 模型', { exact: true })
  await expect(modelButton).toContainText('official/ora-space.opencode · deepseek/deepseek-v4-flash')
  await expect(page.getByText('未保存', { exact: true })).toBeHidden()

  releaseCatalog()
  await expect(modelButton).toContainText('当前模型')
  await expect(page.getByText('未保存', { exact: true })).toBeHidden()
  await page.getByRole('button', { name: '发布', exact: true }).click()
  await expect(page.getByRole('alertdialog', { name: '发布工作流' })).toBeVisible()
  const savedAgent = JSON.parse((await (await page.request.get(`${base}/${wid}/draft`)).json()).snapshot.graph).nodes
    .find((node: { id: string }) => node.id === 'agent')
  expect(savedAgent.data.agentConfig.executor).toEqual({ agentCli: agent, modelId: 'configured' })
})
