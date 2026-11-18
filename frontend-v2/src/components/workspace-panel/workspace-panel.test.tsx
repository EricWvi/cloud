import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { fakeBackend } from '@/test/backend'
import { type OpenRequest, WorkspacePanel } from './workspace-panel'

const base = '/api/v2/tenants/t1/chat/conversations/c1/workspace'
const source = { kind: 'chat' as const, tenantId: 't1', conversationId: 'c1' }

const changes = {
  baseCommit: 'a'.repeat(40),
  truncated: false,
  files: [
    { path: 'README.md', status: 'modified', additions: 1, deletions: 0, binary: false },
    { path: 'src/app.go', status: 'added', additions: 2, deletions: 0, binary: false },
  ],
}

const renderPanel = (props: { revision?: string; request?: OpenRequest } = {}) => {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  const ui = (p: typeof props) => (
    <QueryClientProvider client={client}>
      <WorkspacePanel source={source} revision={p.revision ?? '0'} request={p.request} onClose={() => {}} />
    </QueryClientProvider>
  )
  const view = render(ui(props))
  return { rerender: (p: typeof props) => view.rerender(ui(p)) }
}

describe('workspace panel', () => {
  let seen: string[]
  let ready: boolean
  beforeEach(() => {
    ready = true
    seen = fakeBackend((config) => {
      if (!ready) return { status: 404, data: { code: 'workspace_not_ready', params: {}, requestId: 'r' } }
      const path = (config.params as { path?: string } | undefined)?.path ?? ''
      switch (config.url) {
        case `${base}/changes`:
          return { status: 200, data: changes }
        case `${base}/diff`:
          return { status: 200, data: { path, patch: `diff --git a/${path} b/${path}\n--- a/${path}\n+++ b/${path}\n@@ -1 +1,2 @@\n hello\n+agent\n` } }
        case `${base}/entries`:
          return path === ''
            ? { status: 200, data: { path: '', truncated: false, entries: [{ name: 'src', path: 'src', kind: 'directory' }, { name: 'README.md', path: 'README.md', kind: 'file' }] } }
            : { status: 200, data: { path, truncated: false, entries: [{ name: 'app.go', path: 'src/app.go', kind: 'file' }] } }
        case `${base}/file`:
          return { status: 200, data: { path, content: 'package src\n', sizeBytes: 12 } }
      }
    })
  })

  it('expands several diffs at once and opens a file in full', async () => {
    renderPanel()
    const list = await screen.findByLabelText('改动的文件')
    await userEvent.click(within(list).getByText('README.md'))
    expect(await screen.findByRole('region', { name: 'README.md 的改动' })).toHaveTextContent('agent')
    await userEvent.click(within(list).getByText('app.go'))
    expect(await screen.findByRole('region', { name: 'src/app.go 的改动' })).toBeInTheDocument()
    // Opening the second left the first open.
    expect(screen.getByRole('region', { name: 'README.md 的改动' })).toBeInTheDocument()
    await userEvent.click(within(screen.getByRole('region', { name: 'README.md 的改动' }).parentElement!).getByRole('button', { name: '查看文件' }))
    expect(await screen.findByRole('region', { name: 'README.md 的内容' })).toHaveTextContent('package src')
    await userEvent.click(screen.getByRole('button', { name: '查看改动' }))
    expect(await screen.findByRole('region', { name: 'README.md 的改动' })).toBeInTheDocument()
  })

  it('browses the tree and reads a file', async () => {
    renderPanel()
    await userEvent.click(screen.getByRole('button', { name: '文件' }))
    const tree = await screen.findByRole('tree', { name: '工作区文件' })
    await userEvent.click(await within(tree).findByText('src'))
    await userEvent.click(await within(tree).findByText('app.go'))
    expect(await screen.findByRole('region', { name: 'src/app.go 的内容' })).toHaveTextContent('package src')
    expect(seen).toContain(`GET ${base}/file`)
  })

  it('opens the diff an outside request names, resolving an absolute Agent path', async () => {
    const view = renderPanel()
    await screen.findByLabelText('改动的文件')
    view.rerender({ request: { path: '/data/nodes/x/w/r/c-1/src/app.go', nonce: 1 } })
    expect(await screen.findByRole('region', { name: 'src/app.go 的改动' })).toBeInTheDocument()
  })

  it('says the workspace is not ready, then reads it again on a new revision', async () => {
    ready = false
    const view = renderPanel()
    expect(await screen.findByText(/工作区在第一轮开始后才会准备好/)).toBeInTheDocument()
    ready = true
    view.rerender({ revision: '1' })
    expect(await screen.findByLabelText('改动的文件')).toBeInTheDocument()
  })
})

describe('workspace panel deliveries', () => {
  const delivery = (id: string, status: 'delivered' | 'empty' | 'failed', createdAt: string) => ({
    id,
    status,
    turnId: `turn-${id}`,
    turnPosition: 1,
    createdAt,
    ...(status === 'failed' ? { reasonCode: 'changes_too_large', reason: '改动过大' } : { baseCommit: 'a'.repeat(40) }),
    ...(status === 'delivered' ? { deliveryCommit: 'b'.repeat(40) } : {}),
  })
  const d1 = delivery('d1', 'delivered', '2026-11-18T10:00:00Z')
  let state: Record<string, unknown>
  let files: typeof changes.files
  let patch: () => { status: number; data?: unknown }
  let seen: string[]
  let diffs: (string | undefined)[]
  let saved: string[]

  beforeEach(() => {
    files = changes.files
    patch = () => ({ status: 200, data: new Blob(['diff --git a/README.md b/README.md\n']) })
    diffs = []
    saved = []
    URL.createObjectURL = vi.fn(() => 'blob:patch')
    URL.revokeObjectURL = vi.fn()
    vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(function (this: HTMLAnchorElement) {
      saved.push(this.download)
    })
    seen = fakeBackend((config) => {
      const params = config.params as { path?: string; deliveryId?: string } | undefined
      switch (config.url) {
        case `${base}/changes`:
          return { status: 200, data: { ...changes, files, deliveryState: state } }
        case `${base}/diff`:
          diffs.push(params?.deliveryId)
          return { status: 200, data: { path: params?.path, patch: '@@ -1 +1,2 @@\n hello\n+agent\n', deliveryState: state } }
        case `${base}/deliveries/d1/patch`:
          return patch()
      }
    })
  })
  afterEach(() => vi.restoreAllMocks())

  it('says a turn is running and pins diffs to the previous delivery it shows', async () => {
    state = { mode: 'delivery', inProgress: true, viewDelivery: d1, latestDelivery: d1 }
    renderPanel()
    expect(await screen.findByText('本轮进行中，显示的是上一轮的交付')).toHaveAttribute('role', 'status')
    await userEvent.click(within(await screen.findByLabelText('改动的文件')).getByText('README.md'))
    expect(await screen.findByRole('region', { name: 'README.md 的改动' })).toHaveTextContent('agent')
    expect(diffs).toEqual(['d1'])
  })

  it('says the first turn is running before any delivery', async () => {
    state = { mode: 'delivery', inProgress: true }
    files = []
    renderPanel()
    expect(await screen.findByText('本轮进行中，结束后显示改动')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '下载 patch' })).toBeNull()
  })

  it('downloads the shown delivery as delivery-<id>.patch', async () => {
    state = { mode: 'delivery', inProgress: false, viewDelivery: d1, latestDelivery: d1 }
    renderPanel()
    await userEvent.click(await screen.findByRole('button', { name: '下载 patch' }))
    await vi.waitFor(() => expect(saved).toEqual(['delivery-d1.patch']))
    expect(seen).toContain(`GET ${base}/deliveries/d1/patch`)
    expect(screen.queryByRole('status')).toBeNull()
  })

  it('offers no download for an empty delivery', async () => {
    const empty = delivery('d1', 'empty', '2026-11-18T11:00:00Z')
    state = { mode: 'delivery', inProgress: false, viewDelivery: empty, latestDelivery: empty }
    files = []
    renderPanel()
    expect(await screen.findByText('本轮没有改动。')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '下载 patch' })).toBeNull()
  })

  it('shows the server message for a patch over the download cap, and a generic one otherwise', async () => {
    state = { mode: 'delivery', inProgress: false, viewDelivery: d1, latestDelivery: d1 }
    const fault = { code: 'patch_too_large', params: { message: '改动过大，无法在线下载', maxBytes: 16 }, requestId: 'r' }
    patch = () => ({ status: 413, data: new Blob([JSON.stringify(fault)], { type: 'application/json' }) })
    renderPanel()
    await userEvent.click(await screen.findByRole('button', { name: '下载 patch' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('改动过大，无法在线下载')
    patch = () => ({ status: 503, data: new Blob([JSON.stringify({ code: 'workspace_unavailable', params: {}, requestId: 'r' })]) })
    await userEvent.click(screen.getByRole('button', { name: '下载 patch' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('下载失败，请稍后重试。')
    expect(saved).toEqual([])
  })

  it('shows a newer failed delivery above the last good one', async () => {
    const failed = delivery('d2', 'failed', '2026-11-18T12:00:00Z')
    state = { mode: 'delivery', inProgress: false, viewDelivery: d1, latestDelivery: failed, latestFailedDelivery: failed }
    renderPanel()
    expect(await screen.findByRole('alert')).toHaveTextContent('本轮交付失败：改动过大')
    expect(within(screen.getByLabelText('改动的文件')).getByText('README.md')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '下载 patch' })).toBeEnabled()
  })

  it('shows a failure with no good delivery, and hides one an earlier success superseded', async () => {
    const failed = delivery('d2', 'failed', '2026-11-18T09:00:00Z')
    state = { mode: 'delivery', inProgress: false, latestDelivery: failed, latestFailedDelivery: failed }
    files = []
    const view = renderPanel()
    expect(await screen.findByRole('alert')).toHaveTextContent('本轮交付失败：改动过大')
    files = changes.files
    state = { mode: 'delivery', inProgress: false, viewDelivery: d1, latestDelivery: d1, latestFailedDelivery: failed }
    view.rerender({ revision: '1' })
    expect(await screen.findByLabelText('改动的文件')).toBeInTheDocument()
    expect(screen.queryByRole('alert')).toBeNull()
  })

  it('keeps a legacy working-directory view without delivery notes or download', async () => {
    state = { mode: 'working_directory', inProgress: false }
    renderPanel()
    await userEvent.click(within(await screen.findByLabelText('改动的文件')).getByText('README.md'))
    expect(await screen.findByRole('region', { name: 'README.md 的改动' })).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '下载 patch' })).toBeNull()
    expect(screen.queryByRole('alert')).toBeNull()
    expect(screen.queryByRole('status')).toBeNull()
    expect(diffs).toEqual([undefined])
  })
})
