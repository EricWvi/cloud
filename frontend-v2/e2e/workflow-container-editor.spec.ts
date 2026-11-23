import { expect, test, type Page } from '@playwright/test'
import { signIn } from './login.ts'

const agent = (prompt: string) => ({ executor: { agentCli: 'official/ora-space.opencode', modelId: 'fixture' }, prompt })
const node = (id: string, kind: string, title: string, x: number, y: number, data: Record<string, unknown> = {}, parentId?: string) => ({ id, type: 'workflow', position: { x, y }, parentId, data: { kind, title, description: '', ...data } })
const edge = (source: string, target: string, sourceHandle?: string) => ({ id: `${source}-${target}`, source, target, sourceHandle, type: 'workflow' })
const loop = (x: number, y: number) => [
  node('loop', 'loop', '反馈循环', x, y, { loopConfig: {
    maxIterations: 2, maxIterationsBehavior: 'fail',
    variables: [{ name: 'value', valueType: 'string', initial: { kind: 'constant', value: '' }, feedback: ['writer', 'output'] }],
    until: { logic: 'and', conditions: [{ variableSelector: ['writer', 'output'], operator: 'not_empty' }] },
    outputs: [{ name: 'result', variableSelector: ['writer', 'output'] }],
  } }),
  node('entry', 'start', '轮次开始', 40, 145, { containerId: 'loop' }, 'loop'),
  node('writer', 'agent', '改进', 350, 145, { containerId: 'loop', agentConfig: agent('改进：{{#loop.value#}}') }, 'loop'),
]

/** Creates a workflow from a graph through the API and opens it in the editor. */
async function openGraph(page: Page, title: string, graph: unknown) {
  await page.goto('/workflows')
  await signIn(page)
  const tenant = (await (await page.request.get('/api/v1/me/tenants')).json()).items[0].id as string
  // The e2e database outlives a run, so each run makes its own workflow.
  const name = `${title} ${Date.now().toString(36)}`
  const created = await page.request.post(`/api/v2/tenants/${tenant}/workflows`, { data: { name, graph: JSON.stringify(graph) } })
  expect(created.status()).toBe(201)
  await page.reload()
  await page.getByRole('complementary', { name: '工作流列表' }).getByRole('button', { name, exact: true }).click()
  await expect(page.getByRole('textbox', { name: '工作流名称' })).toHaveValue(name)
}

test('workflow container editor: a frame port connects from anywhere on its dot, not only its outer half', async ({ page }) => {
  await page.setViewportSize({ width: 1920, height: 1080 })
  await openGraph(page, '容器端口', { schemaVersion: 2, nodes: [
    node('start', 'start', '开始', 0, 40, { inputVariables: [{ name: 'items', valueType: 'array[string]', value: ['a'] }] }),
    node('iter', 'iteration', '逐段处理', 320, 0, { iterationConfig: { iteratorSelector: ['start', 'items'], collectSelector: ['worker', 'output'], errorStrategy: 'continue', maxIterations: 50 } }),
    node('worker', 'agent', '处理段落', 120, 90, { agentConfig: agent('处理：{{#iter.item#}}') }, 'iter'),
    node('out', 'output', '迭代输出', 1000, 40, { outputs: [{ name: 'paragraphs', variableSelector: ['iter', 'output'] }] }),
    ...loop(320, 420),
    node('out2', 'output', '循环输出', 1100, 470, { outputs: [{ name: 'result', variableSelector: ['loop', 'result'] }] }),
  ], edges: [edge('start', 'iter'), edge('iter', 'worker', 'iteration-entry'), edge('entry', 'writer'), edge('start', 'loop')] })

  const edges = page.locator('.react-flow__edge')
  await expect(edges).toHaveCount(4)
  for (const [source, target] of [['iter', 'out'], ['loop', 'out2']]) {
    const port = (await page.locator(`[data-workflow-output="${source}"]`).boundingBox())!
    const x = port.x + port.width / 2
    const y = port.y + port.height / 2
    // The 22px target around the dot belongs to the port on both sides of the frame edge; the
    // iteration header used to cover its inner half.
    for (const dx of [-8, -4, 0, 4, 8]) {
      const owner = await page.evaluate(([px, py]) => document.elementFromPoint(px, py)?.closest('[data-workflow-output]')?.getAttribute('data-workflow-output'), [x + dx, y])
      expect(owner, `${source} output port at dx=${dx}`).toBe(source)
    }
    const input = (await page.locator(`[data-workflow-input="${target}"]`).boundingBox())!
    const before = await edges.count()
    await page.mouse.move(x - 4, y)
    await page.mouse.down()
    await page.mouse.move(input.x + input.width / 2, input.y + input.height / 2, { steps: 12 })
    await page.mouse.up()
    await expect(edges).toHaveCount(before + 1)
  }
})

test('workflow container editor: publishing a stale loop.output reference is refused in the dialog, naming the node', async ({ page }) => {
  await openGraph(page, '旧引用', { schemaVersion: 2, nodes: [
    node('start', 'start', '开始', 0, 60),
    ...loop(320, 0),
    node('out', 'output', '汇总输出', 1120, 60, { outputs: [{ name: 'result', variableSelector: ['loop', 'output'] }] }),
  ], edges: [edge('start', 'loop'), edge('entry', 'writer'), edge('loop', 'out')] })

  await page.getByRole('button', { name: '发布', exact: true }).click()
  const publish = page.getByRole('alertdialog', { name: '发布工作流' })
  await publish.getByRole('textbox').fill('v1')
  await publish.getByRole('button', { name: '发布' }).click()
  await expect(publish.getByText('发布被拒绝：节点「汇总输出」的变量引用没有通过发布校验，草稿已保存。', { exact: true })).toBeVisible()
  await expect(publish.getByText(/Loop loop publishes only its named outputs \(result\); loop\.output is not one/)).toBeVisible()
  await expect(page.getByText('暂未发布')).toBeVisible()
})
