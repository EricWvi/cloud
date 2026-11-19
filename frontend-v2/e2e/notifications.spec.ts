import { expect, test } from '@playwright/test'
import { signIn } from './login.ts'

test('an assignment reaches the assignee through the bell, and 待验收 reaches the creator', async ({ page, browser }) => {
  await page.goto('/collaboration')
  await signIn(page)
  const tid = (await (await page.request.get('/api/v1/me/tenants')).json()).items[0].id
  const base = `/api/v2/tenants/${tid}`
  const projectId = (await (await page.request.get(`${base}/projects`)).json()).items[0].id
  const suffix = crypto.randomUUID().slice(0, 8)
  const email = `notify-${suffix}@demo.ora-space.dev`
  const invited = await page.request.post(`${base}/members`, { data: { email, displayName: `提醒成员-${suffix}` } })
  expect(invited.ok(), await invited.text()).toBeTruthy()

  const other = await (await browser.newContext()).newPage()
  await other.goto('/collaboration')
  await signIn(other, email)
  const memberId = (await (await other.request.get('/api/v1/me')).json()).id
  const title = `提醒验收 ${suffix}`
  const created = await page.request.post(`${base}/tasks`, { data: { title, projectId, assignee: { type: 'human', id: memberId } } })
  expect(created.status(), await created.text()).toBe(201)
  const taskId = (await created.json()).id

  // The new member's only notification is this assignment; the bell polls, so a reload shows it now.
  await other.reload()
  await other.getByRole('button', { name: '提醒，1 条未读' }).click()
  const list = other.getByRole('list', { name: '提醒列表' })
  await expect(list.getByRole('button')).toHaveCount(1)
  await expect(list.getByRole('button')).toContainText(title)
  await list.getByRole('button').click()
  await expect(other).toHaveURL(new RegExp(`/collaboration/tasks/${taskId}`))
  await expect(other.getByRole('button', { name: '提醒', exact: true })).toBeVisible()

  // The member moves it to 待验收; the owner, who created it, is told.
  let task = await (await other.request.post(`${base}/tasks/${taskId}/status`, { data: { status: 'running', version: 1 } })).json()
  task = await (await other.request.post(`${base}/tasks/${taskId}/status`, { data: { status: 'review', version: task.version } })).json()
  expect(task.status).toBe('review')
  await page.reload()
  await page.getByRole('button', { name: /^提醒，\d+ 条未读$/ }).click()
  await expect(page.getByRole('list', { name: '提醒列表' }).getByRole('button', { name: new RegExp(title) }))
    .toContainText(`提醒成员-${suffix} 把你创建的任务提交待验收`)
})
