import { readFile } from 'node:fs/promises'
import { expect, test } from '@playwright/test'
import { signIn } from './login.ts'

// A delivery needs a project-backed turn, which only a real Node and Agent produce (prepared Node
// assets via V2_NODE_ASSETS_DIR). The fixture model has the Agent write ora-v2-result.txt into the
// checkout, so the turn's delivery adds that file.
const realAgents = !!process.env.V2_NODE_ASSETS_DIR

test('workspace: a chat turn is delivered, its change is shown and its patch downloads', async ({ page }) => {
  test.skip(!realAgents, 'needs V2_NODE_ASSETS_DIR for a real Node and Agent')
  // The first conversation in a project installs the Agent plugin in the project's Node.
  test.setTimeout(300_000)
  const composer = page.getByRole('textbox', { name: '消息' })

  await page.goto('/chat')
  await signIn(page)
  await page.getByRole('button', { name: '选择项目' }).click()
  await page.getByRole('option', { name: 'Demo' }).click()
  await composer.fill(`写一个文件 ${Date.now().toString(36)}`)
  await composer.press('Enter')
  await expect(page.getByText('V2 fixture complete. Real Agent executed the shell tool.')).toBeVisible({ timeout: 240_000 })

  const toggle = page.getByRole('button', { name: '工作区', exact: true })
  if ((await toggle.getAttribute('aria-pressed')) !== 'true') await toggle.click()
  const panel = page.getByRole('complementary', { name: '工作区' })
  const list = panel.getByLabel('改动的文件')
  await expect(list.getByText('ora-v2-result.txt')).toBeVisible({ timeout: 60_000 })
  await expect(panel.getByText(/本轮进行中/)).toHaveCount(0)

  await list.getByText('ora-v2-result.txt').click()
  await expect(panel.getByRole('region', { name: 'ora-v2-result.txt 的改动' }).getByText('v2 fixture tool executed')).toBeVisible()

  const download = page.waitForEvent('download')
  await panel.getByRole('button', { name: '下载 patch' }).click()
  const file = await download
  expect(file.suggestedFilename()).toMatch(/^delivery-[0-9a-f-]{36}\.patch$/)
  const patch = await readFile((await file.path())!, 'utf8')
  expect(patch).toContain('diff --git a/ora-v2-result.txt b/ora-v2-result.txt')
  expect(patch).toContain('+v2 fixture tool executed')
  await page.screenshot({ path: test.info().outputPath('workspace-delivery.png') })
})
