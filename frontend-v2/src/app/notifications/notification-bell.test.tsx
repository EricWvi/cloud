import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { createMemoryRouter } from 'react-router'
import { beforeEach, describe, expect, it } from 'vitest'
import type { Notification } from '@/lib/tasks-api/generated'
import { demoUser, fakeBackend } from '@/test/backend'
import { App } from '../app'
import { routes } from '../routes'
import { notificationText } from './notification-text'

const tenantId = '00000000-0000-4000-8000-0000000000aa'
const inbox = `/api/v2/tenants/${tenantId}/notifications`

const notice = (over: Partial<Notification>): Notification => ({
  id: 'n1', kind: 'assigned', taskId: 't1', taskTitle: '修登录页', taskStatus: 'pending',
  actorId: 'u2', actorName: '李雷', commentId: null, excerpt: '', createdAt: '2026-11-19T02:00:00Z', readAt: null, ...over,
})

describe('notification bell', () => {
  let items: Notification[]
  let reads: unknown[]
  let seen: string[]

  beforeEach(() => {
    items = [
      notice({ id: 'n2', kind: 'mentioned', taskId: 't2', taskTitle: '写周报', commentId: 'c1', excerpt: '@你 看下数据' }),
      notice({}),
      notice({ id: 'n3', kind: 'review', taskId: 't3', taskTitle: '旧任务', actorId: null, actorName: null, readAt: '2026-11-18T00:00:00Z' }),
    ]
    reads = []
    const unread = () => items.filter(n => !n.readAt).length
    seen = fakeBackend((c) => {
      if (c.url === '/api/v1/me') return { status: 200, data: demoUser }
      if (c.url === '/api/v1/me/tenants') {
        return { status: 200, data: { items: [{ id: tenantId, name: '演示团队', status: 'active', role: 'member' }], nextCursor: '' } }
      }
      if (c.url === inbox && c.method === 'get') return { status: 200, data: { items, unread: unread() } }
      if (c.url === `${inbox}/read`) {
        const body = JSON.parse(c.data as string) as { ids?: string[]; all?: boolean }
        reads.push(body)
        items = items.map(n => (body.all || body.ids?.includes(n.id)) && !n.readAt ? { ...n, readAt: '2026-11-19T03:00:00Z' } : n)
        return { status: 200, data: { unread: unread() } }
      }
      return undefined
    })
  })

  const renderAt = (path: string) => {
    const router = createMemoryRouter(routes, { initialEntries: [path] })
    render(<App router={router} />)
    return router
  }

  it('shows the unread count and lists who did what', async () => {
    renderAt('/')
    const bell = await screen.findByRole('button', { name: '提醒，2 条未读' })
    expect(bell).toHaveTextContent('2')
    await userEvent.click(bell)
    const list = await screen.findByRole('list', { name: '提醒列表' })
    const rows = within(list).getAllByRole('button')
    expect(rows.map(r => r.textContent)).toEqual([
      expect.stringContaining('李雷 在任务里 @ 了你写周报@你 看下数据'),
      expect.stringContaining('李雷 把任务指派给了你修登录页'),
      expect.stringContaining('你创建的任务已进入待验收旧任务'),
    ])
    expect(within(list).getAllByLabelText('未读')).toHaveLength(2)
  })

  it('opens the task and marks that notification read', async () => {
    const router = renderAt('/')
    await userEvent.click(await screen.findByRole('button', { name: '提醒，2 条未读' }))
    await userEvent.click(within(await screen.findByRole('list', { name: '提醒列表' })).getByRole('button', { name: /修登录页/ }))
    await waitFor(() => expect(router.state.location.pathname).toBe('/collaboration/tasks/t1'))
    expect(reads).toEqual([{ ids: ['n1'] }])
    expect(await screen.findByRole('button', { name: '提醒，1 条未读' })).toBeInTheDocument()
  })

  it('does not mark an already read notification again', async () => {
    const router = renderAt('/')
    await userEvent.click(await screen.findByRole('button', { name: '提醒，2 条未读' }))
    await userEvent.click(within(await screen.findByRole('list', { name: '提醒列表' })).getByRole('button', { name: /旧任务/ }))
    await waitFor(() => expect(router.state.location.pathname).toBe('/collaboration/tasks/t3'))
    expect(reads).toEqual([])
  })

  it('marks everything read at once', async () => {
    renderAt('/')
    await userEvent.click(await screen.findByRole('button', { name: '提醒，2 条未读' }))
    await userEvent.click(await screen.findByRole('button', { name: '全部已读' }))
    expect(await screen.findByRole('button', { name: '提醒' })).toBeInTheDocument()
    expect(reads).toEqual([{ all: true }])
    expect(screen.queryAllByLabelText('未读')).toHaveLength(0)
    expect(screen.getByRole('button', { name: '全部已读' })).toBeDisabled()
  })

  it('says so when there is nothing', async () => {
    items = []
    renderAt('/')
    await userEvent.click(await screen.findByRole('button', { name: '提醒' }))
    expect(await screen.findByText('暂无提醒')).toBeInTheDocument()
    expect(seen).toContain(`GET ${inbox}`)
  })
})

describe('notificationText', () => {
  it('names the actor, or says a run did it', () => {
    expect(notificationText(notice({ kind: 'review', actorName: '韩梅梅' }))).toBe('韩梅梅 把你创建的任务提交待验收')
    expect(notificationText(notice({ kind: 'assigned', actorName: null }))).toBe('Agent 把任务指派给了你')
  })
})
