import { act, render, screen, waitFor, within } from '@testing-library/react'
import { focusManager } from '@tanstack/react-query'
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
  let inboxStatus: number
  let pageSize: number
  let inboxReads: string[]
  let pageStatus: Record<string, number>
  let holdPage: Record<string, Promise<void>>

  beforeEach(() => {
    items = [
      notice({ id: 'n2', kind: 'mentioned', taskId: 't2', taskTitle: '写周报', commentId: 'c1', excerpt: '@你 看下数据' }),
      notice({}),
      notice({ id: 'n3', kind: 'review', taskId: 't3', taskTitle: '旧任务', actorId: null, actorName: null, readAt: '2026-11-18T00:00:00Z' }),
    ]
    reads = []
    inboxStatus = 200
    pageSize = 30
    inboxReads = []
    pageStatus = {}
    holdPage = {}
    const unread = () => items.filter(n => !n.readAt).length
    const readInbox = async (params: { cursor?: string } | undefined) => {
      const key = params?.cursor ?? 'first'
      inboxReads.push(key)
      await holdPage[key]
      const status = pageStatus[key] ?? inboxStatus
      if (status !== 200) return { status, data: { code: 'not_found', params: {}, requestId: 'r' } }
      // The cursor is opaque to the page; the fake uses the index of the next item.
      const start = Number(params?.cursor ?? 0)
      const page = items.slice(start, start + pageSize)
      const nextCursor = start + pageSize < items.length ? String(start + pageSize) : ''
      return { status: 200, data: { items: page, unread: unread(), nextCursor } }
    }
    seen = fakeBackend((c) => {
      if (c.url === '/api/v1/me') return { status: 200, data: demoUser }
      if (c.url === '/api/v1/me/tenants') {
        return { status: 200, data: { items: [{ id: tenantId, name: '演示团队', status: 'active', role: 'member' }], nextCursor: '' } }
      }
      if (c.url === inbox && c.method === 'get') return readInbox(c.params as { cursor?: string } | undefined)
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

  it('reaches an unread notification beyond the first page', async () => {
    pageSize = 2
    items = [
      notice({ id: 'r1', readAt: '2026-11-18T00:00:00Z', taskTitle: '已读一' }),
      notice({ id: 'r2', readAt: '2026-11-18T00:00:00Z', taskTitle: '已读二' }),
      notice({ id: 'old', taskId: 't9', taskTitle: '很早的未读' }),
    ]
    const router = renderAt('/')
    await userEvent.click(await screen.findByRole('button', { name: '提醒，1 条未读' }))
    const list = await screen.findByRole('list', { name: '提醒列表' })
    expect(within(list).queryByText('很早的未读')).not.toBeInTheDocument()
    await userEvent.click(within(list).getByRole('button', { name: '加载更多' }))
    await userEvent.click(await within(list).findByRole('button', { name: /很早的未读/ }))
    await waitFor(() => expect(router.state.location.pathname).toBe('/collaboration/tasks/t9'))
    expect(reads).toEqual([{ ids: ['old'] }])
  })

  it('hides the bell and its cached list once access to the workspace is lost', async () => {
    renderAt('/')
    await userEvent.click(await screen.findByRole('button', { name: '提醒，2 条未读' }))
    expect(await screen.findByRole('list', { name: '提醒列表' })).toBeInTheDocument()
    // Removed from the team: the next poll answers 404, and nothing cached may stay on screen.
    inboxStatus = 404
    act(() => {
      focusManager.setFocused(false)
      focusManager.setFocused(true)
    })
    await waitFor(() => expect(screen.queryByRole('button', { name: /^提醒/ })).not.toBeInTheDocument())
    expect(screen.queryByRole('list', { name: '提醒列表' })).not.toBeInTheDocument()
    expect(screen.queryByText('修登录页')).not.toBeInTheDocument()

    // A later failure of another kind must not bring the old notifications back.
    inboxStatus = 503
    const before = inboxReads.length
    act(() => {
      focusManager.setFocused(false)
      focusManager.setFocused(true)
    })
    // The refetch fails for good only after the default three retries (1 s, 2 s, 4 s); until then
    // the previous state stands, so look only once the last attempt has answered.
    await waitFor(() => expect(inboxReads.length).toBe(before + 4), { timeout: 12_000 })
    await new Promise(resolve => setTimeout(resolve, 50))
    expect(screen.queryByRole('button', { name: /^提醒/ })).not.toBeInTheDocument()
    expect(screen.queryByText('修登录页')).not.toBeInTheDocument()

    // Only a successful read shows the bell again.
    inboxStatus = 200
    act(() => {
      focusManager.setFocused(false)
      focusManager.setFocused(true)
    })
    expect(await screen.findByRole('button', { name: '提醒，2 条未读' })).toBeInTheDocument()
    act(() => focusManager.setFocused(undefined))
  }, 20_000)

  it('polls only the newest page once the list is closed', async () => {
    pageSize = 1
    renderAt('/')
    const bell = await screen.findByRole('button', { name: '提醒，2 条未读' })
    await userEvent.click(bell)
    const list = await screen.findByRole('list', { name: '提醒列表' })
    await userEvent.click(within(list).getByRole('button', { name: '加载更多' }))
    await userEvent.click(await within(list).findByRole('button', { name: '加载更多' }))
    await within(list).findByText('旧任务')
    await userEvent.keyboard('{Escape}')
    await waitFor(() => expect(screen.queryByRole('list', { name: '提醒列表' })).not.toBeInTheDocument())

    inboxReads = []
    act(() => {
      focusManager.setFocused(false)
      focusManager.setFocused(true)
    })
    await waitFor(() => expect(inboxReads).toEqual(['first']))
    await new Promise(resolve => setTimeout(resolve, 50))
    expect(inboxReads).toEqual(['first'])

    // Reopened, the older pages can still be loaded.
    await userEvent.click(screen.getByRole('button', { name: '提醒，2 条未读' }))
    const reopened = await screen.findByRole('list', { name: '提醒列表' })
    expect(within(reopened).queryByText('旧任务')).not.toBeInTheDocument()
    await userEvent.click(within(reopened).getByRole('button', { name: '加载更多' }))
    expect(await within(reopened).findByText('修登录页')).toBeInTheDocument()
    act(() => focusManager.setFocused(undefined))
  })

  it('hides everything when an older page is refused, until the newest page loads again', async () => {
    pageSize = 1
    pageStatus = { '1': 404 }
    renderAt('/')
    await userEvent.click(await screen.findByRole('button', { name: '提醒，2 条未读' }))
    const list = await screen.findByRole('list', { name: '提醒列表' })
    expect(within(list).getByText('写周报')).toBeInTheDocument()
    // Removed from the team, and the first refusal comes from the next page, not the newest one.
    await userEvent.click(await within(list).findByRole('button', { name: '加载更多' }))
    await waitFor(() => expect(screen.queryByRole('button', { name: /^提醒/ })).not.toBeInTheDocument())
    expect(screen.queryByText('写周报')).not.toBeInTheDocument()

    // A failing poll of the newest page does not bring it back.
    inboxStatus = 503
    const before = inboxReads.length
    act(() => {
      focusManager.setFocused(false)
      focusManager.setFocused(true)
    })
    await waitFor(() => expect(inboxReads.length).toBeGreaterThan(before))
    await new Promise(resolve => setTimeout(resolve, 50))
    expect(screen.queryByRole('button', { name: /^提醒/ })).not.toBeInTheDocument()
    expect(screen.queryByText('写周报')).not.toBeInTheDocument()

    // Only a successful read of the newest page does.
    inboxStatus = 200
    act(() => {
      focusManager.setFocused(false)
      focusManager.setFocused(true)
    })
    expect(await screen.findByRole('button', { name: '提醒，2 条未读' }, { timeout: 10_000 })).toBeInTheDocument()
    act(() => focusManager.setFocused(undefined))
  }, 20_000)

  it('drops an older page that arrives after the list closed', async () => {
    pageSize = 1
    let release = () => {}
    holdPage = { '1': new Promise<void>(resolve => { release = resolve }) }
    renderAt('/')
    await userEvent.click(await screen.findByRole('button', { name: '提醒，2 条未读' }))
    const list = await screen.findByRole('list', { name: '提醒列表' })
    await userEvent.click(await within(list).findByRole('button', { name: '加载更多' }))
    await waitFor(() => expect(inboxReads).toContain('1'))
    // Closed while the second page is still on its way; it answers only afterwards.
    await userEvent.keyboard('{Escape}')
    await waitFor(() => expect(screen.queryByRole('list', { name: '提醒列表' })).not.toBeInTheDocument())
    act(() => release())
    await new Promise(resolve => setTimeout(resolve, 50))

    inboxReads = []
    act(() => {
      focusManager.setFocused(false)
      focusManager.setFocused(true)
    })
    await waitFor(() => expect(inboxReads).toEqual(['first']))
    await new Promise(resolve => setTimeout(resolve, 50))
    expect(inboxReads).toEqual(['first'])

    // Reopened, the list starts from the newest page again.
    await userEvent.click(screen.getByRole('button', { name: '提醒，2 条未读' }))
    const reopened = await screen.findByRole('list', { name: '提醒列表' })
    expect(within(reopened).queryByText('修登录页')).not.toBeInTheDocument()
    expect(within(reopened).getByRole('button', { name: '加载更多' })).toBeEnabled()
    act(() => focusManager.setFocused(undefined))
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
