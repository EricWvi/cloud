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

type PanelSource = Parameters<typeof WorkspacePanel>[0]['source']

const renderPanel = (props: { revision?: string; request?: OpenRequest } = {}, from: PanelSource = source) => {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  const ui = (p: typeof props) => (
    <QueryClientProvider client={client}>
      <WorkspacePanel source={from} revision={p.revision ?? '0'} request={p.request} onClose={() => {}} />
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
  const delivery = (id: string, status: 'delivered' | 'empty' | 'failed', createdAt: string, nodeId?: string) => ({
    id,
    status,
    turnId: `turn-${id}`,
    turnPosition: 1,
    createdAt,
    ...(nodeId ? { nodeId, nodeRunId: '00000000-0000-4000-8000-00000000000' + id.slice(-1) } : {}),
    ...(status === 'failed' ? { reasonCode: 'changes_too_large', reason: '改动过大' } : { baseCommit: 'a'.repeat(40) }),
    ...(status === 'delivered' ? { deliveryCommit: 'b'.repeat(40) } : {}),
  })
  const d1 = delivery('d1', 'delivered', '2026-11-18T10:00:00Z')
  const d2 = delivery('d2', 'delivered', '2026-11-18T11:00:00Z')
  const runSource = { kind: 'run' as const, tenantId: 't1', runId: 'r1' }
  let state: Record<string, unknown>
  let files: typeof changes.files
  let patch: () => { status: number; data?: unknown } | Promise<{ status: number; data?: unknown }>
  let seen: string[]
  let diffs: (string | undefined)[]
  let saved: string[]
  /** How many revocations had happened right after each click() returned, before any timer ran. */
  let revokedAfterClick: number[]

  beforeEach(() => {
    files = changes.files
    patch = () => ({ status: 200, data: new Blob(['diff --git a/README.md b/README.md\n']) })
    diffs = []
    saved = []
    revokedAfterClick = []
    URL.createObjectURL = vi.fn(() => 'blob:patch')
    const revoke = vi.fn()
    URL.revokeObjectURL = revoke
    vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(function (this: HTMLAnchorElement) {
      saved.push(this.download)
      queueMicrotask(() => revokedAfterClick.push(revoke.mock.calls.length))
    })
    seen = fakeBackend((config) => {
      const params = config.params as { path?: string; deliveryId?: string } | undefined
      const route = config.url?.replace(/^.*\/workspace/, '')
      if (route === '/changes') return { status: 200, data: { ...changes, files, deliveryState: state } }
      if (route === '/diff') {
        diffs.push(params?.deliveryId)
        // Each delivery has its own content; an unpinned diff reads whatever the server has now.
        const from = params?.deliveryId ?? (state.viewDelivery as { id?: string } | undefined)?.id ?? 'live'
        return { status: 200, data: { path: params?.path, patch: `@@ -1 +1,2 @@\n hello\n+from ${from}\n`, deliveryState: state } }
      }
      if (route === '/deliveries/d1/patch') return patch()
    })
  })
  afterEach(() => vi.restoreAllMocks())

  it('says a turn is running and pins diffs to the previous delivery it shows', async () => {
    state = { mode: 'delivery', inProgress: true, viewDelivery: d1, latestDelivery: d1 }
    renderPanel()
    expect(await screen.findByText('本轮进行中，显示的是上一轮的交付')).toHaveAttribute('role', 'status')
    await userEvent.click(within(await screen.findByLabelText('改动的文件')).getByText('README.md'))
    expect(await screen.findByRole('region', { name: 'README.md 的改动' })).toHaveTextContent('from d1')
    expect(diffs).toEqual(['d1'])
  })

  it('keeps an open diff on the delivery the list shows, and follows the list to a newer one', async () => {
    state = { mode: 'delivery', inProgress: false, viewDelivery: d1, latestDelivery: d1 }
    const view = renderPanel()
    const list = await screen.findByLabelText('改动的文件')
    await userEvent.click(within(list).getByText('README.md'))
    expect(await screen.findByRole('region', { name: 'README.md 的改动' })).toHaveTextContent('from d1')
    // d2 lands on the server; until the list reads it, a newly opened diff still reads d1.
    state = { mode: 'delivery', inProgress: false, viewDelivery: d2, latestDelivery: d2 }
    await userEvent.click(within(list).getByText('app.go'))
    expect(await screen.findByRole('region', { name: 'src/app.go 的改动' })).toHaveTextContent('from d1')
    view.rerender({ revision: '1' })
    await vi.waitFor(() => expect(screen.getByRole('region', { name: 'README.md 的改动' })).toHaveTextContent('from d2'))
    expect(screen.getByRole('region', { name: 'src/app.go 的改动' })).toHaveTextContent('from d2')
    expect(diffs.filter((id) => id !== 'd1' && id !== 'd2')).toEqual([])
  })

  it('says the first turn is running before any delivery', async () => {
    state = { mode: 'delivery', inProgress: true }
    files = []
    renderPanel()
    expect(await screen.findByText('本轮进行中，结束后显示改动')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '下载 patch' })).toBeNull()
  })

  it('downloads the shown delivery as delivery-<id>.patch and revokes its URL only after the click', async () => {
    state = { mode: 'delivery', inProgress: false, viewDelivery: d1, latestDelivery: d1 }
    renderPanel()
    await userEvent.click(await screen.findByRole('button', { name: '下载 patch' }))
    await vi.waitFor(() => expect(saved).toEqual(['delivery-d1.patch']))
    // Still alive once click() returned; revoked later, from a timer.
    expect(revokedAfterClick).toEqual([0])
    await vi.waitFor(() => expect(URL.revokeObjectURL).toHaveBeenCalledWith('blob:patch'))
    expect(seen.filter((r) => r.endsWith('/patch'))).toEqual([`GET ${base}/deliveries/d1/patch`])
  })

  it('disables the button while a download runs', async () => {
    state = { mode: 'delivery', inProgress: false, viewDelivery: d1, latestDelivery: d1 }
    let answer!: (reply: { status: number; data?: unknown }) => void
    patch = () => new Promise((resolve) => (answer = resolve))
    renderPanel()
    await userEvent.click(await screen.findByRole('button', { name: '下载 patch' }))
    expect(await screen.findByRole('button', { name: '正在下载…' })).toBeDisabled()
    answer({ status: 200, data: new Blob(['x']) })
    expect(await screen.findByRole('button', { name: '下载 patch' })).toBeEnabled()
    expect(saved).toEqual(['delivery-d1.patch'])
  })

  it('saves nothing for a 204 or an empty body', async () => {
    state = { mode: 'delivery', inProgress: false, viewDelivery: d1, latestDelivery: d1 }
    patch = () => ({ status: 204, data: new Blob([]) })
    renderPanel()
    await userEvent.click(await screen.findByRole('button', { name: '下载 patch' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('本轮没有改动。')
    expect(saved).toEqual([])
  })

  it('offers no download for an empty delivery, and says which turn it was while one runs', async () => {
    const empty = delivery('d1', 'empty', '2026-11-18T11:00:00Z')
    state = { mode: 'delivery', inProgress: false, viewDelivery: empty, latestDelivery: empty }
    files = []
    const view = renderPanel()
    expect(await screen.findByText('本轮没有改动。')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '下载 patch' })).toBeNull()
    state = { ...state, inProgress: true }
    view.rerender({ revision: '1' })
    expect(await screen.findByText('上一轮没有改动。')).toBeInTheDocument()
    expect(screen.queryByText('本轮没有改动。')).toBeNull()
  })

  it('shows the server message for 413, the failure for 409 and a generic one otherwise', async () => {
    state = { mode: 'delivery', inProgress: false, viewDelivery: d1, latestDelivery: d1 }
    const blob = (body: unknown) => new Blob([JSON.stringify(body)], { type: 'application/json' })
    // A server text distinct from the fallback proves the message comes from params.message.
    patch = () => ({ status: 413, data: blob({ code: 'patch_too_large', params: { message: '服务端：超过 16 MiB', maxBytes: 16 }, requestId: 'r' }) })
    renderPanel()
    const button = await screen.findByRole('button', { name: '下载 patch' })
    await userEvent.click(button)
    expect(await screen.findByRole('alert')).toHaveTextContent('服务端：超过 16 MiB')
    patch = () => ({ status: 409, data: blob({ code: 'delivery_failed', params: { reasonCode: 'changes_too_large', reason: '改动过大' }, requestId: 'r' }) })
    await userEvent.click(button)
    await vi.waitFor(() => expect(screen.getByRole('alert')).toHaveTextContent('这一轮交付失败，没有可下载的改动。'))
    patch = () => ({ status: 503, data: blob({ code: 'workspace_unavailable', params: {}, requestId: 'r' }) })
    await userEvent.click(button)
    await vi.waitFor(() => expect(screen.getByRole('alert')).toHaveTextContent('下载失败，请稍后重试。'))
    expect(saved).toEqual([])
  })

  it('clears a download error once the panel shows another delivery', async () => {
    state = { mode: 'delivery', inProgress: false, viewDelivery: d1, latestDelivery: d1 }
    patch = () => ({ status: 503, data: new Blob(['{}']) })
    const view = renderPanel()
    await userEvent.click(await screen.findByRole('button', { name: '下载 patch' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('下载失败')
    state = { mode: 'delivery', inProgress: false, viewDelivery: d2, latestDelivery: d2 }
    view.rerender({ revision: '1' })
    await vi.waitFor(() => expect(screen.queryByRole('alert')).toBeNull())
    expect(screen.getByRole('button', { name: '下载 patch' })).toBeEnabled()
  })

  it('reports a conversation failure only while it is the latest outcome, in the server order', async () => {
    const failed = delivery('d2', 'failed', '2026-11-18T09:00:00Z')
    // The failure is the latest outcome even though its timestamp is older than the good one.
    state = { mode: 'delivery', inProgress: false, viewDelivery: d1, latestDelivery: failed, latestFailedDelivery: failed }
    const view = renderPanel()
    expect(await screen.findByRole('alert')).toHaveTextContent('本轮交付失败：改动过大。下面是上一次成功的交付。')
    expect(within(screen.getByLabelText('改动的文件')).getByText('README.md')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '下载 patch' })).toBeEnabled()
    // While a new turn runs the failure belongs to the previous one.
    state = { ...state, inProgress: true }
    view.rerender({ revision: '1' })
    await vi.waitFor(() => expect(screen.getByRole('alert')).toHaveTextContent('上一轮交付失败：改动过大'))
    // A later good delivery supersedes it, even with an older timestamp.
    const later = delivery('d3', 'delivered', '2026-11-18T08:00:00Z')
    const newer = delivery('d4', 'failed', '2026-11-18T12:00:00Z')
    state = { mode: 'delivery', inProgress: false, viewDelivery: later, latestDelivery: later, latestFailedDelivery: newer }
    view.rerender({ revision: '2' })
    await vi.waitFor(() => expect(screen.queryByRole('alert')).toBeNull())
  })

  it('reports a failure with no good delivery', async () => {
    const failed = delivery('d2', 'failed', '2026-11-18T09:00:00Z')
    state = { mode: 'delivery', inProgress: false, latestDelivery: failed, latestFailedDelivery: failed }
    files = []
    renderPanel()
    const alert = await screen.findByRole('alert')
    expect(alert).toHaveTextContent('本轮交付失败：改动过大')
    expect(alert).not.toHaveTextContent('上一次成功')
  })

  it('always reports a workflow run node failure, naming the node, after other nodes delivered', async () => {
    const failed = delivery('d2', 'failed', '2026-11-18T09:00:00Z', 'writer')
    const later = delivery('d3', 'delivered', '2026-11-18T10:00:00Z', 'reviewer')
    state = { mode: 'delivery', inProgress: false, viewDelivery: later, latestDelivery: later, latestFailedDelivery: failed }
    renderPanel({}, runSource)
    expect(await screen.findByRole('alert')).toHaveTextContent('节点 writer 的交付失败：改动过大。下面是最近一次成功的交付。')
    expect(within(screen.getByLabelText('改动的文件')).getByText('README.md')).toBeInTheDocument()
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
