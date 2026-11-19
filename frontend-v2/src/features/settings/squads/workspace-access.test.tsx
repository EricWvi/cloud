import { describe, expect, it } from 'vitest'
import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { fakeBackend } from '@/test/backend'
import { renderSettingsPage } from '../test-utils'
import { WorkspaceAccess } from './workspace-access'

const tenantId = '00000000-0000-4000-8000-0000000000aa'
const me = '00000000-0000-4000-8000-000000000001'
const alice = '00000000-0000-4000-8000-000000000002'
const squads = [
  { id: 'sq-live', name: '后端', archivedAt: null },
  { id: 'sq-old', name: '旧小队', archivedAt: '2026-11-01T00:00:00Z' },
] as never

const link = (id: string, extra: object = {}) => ({
  id, role: 'member', squad: null, createdByName: '演示用户', createdAt: '2026-11-18T00:00:00Z',
  expiresAt: '2026-11-25T00:00:00Z', usedByName: null, usedAt: null, ...extra,
})

function serve(state: { links: object[]; members: { userId: string; displayName: string; role: string }[] }, seen: string[]) {
  fakeBackend((c) => {
    const url = c.url ?? ''
    if (url === `/api/v2/tenants/${tenantId}/invite-links` && c.method === 'get') return { status: 200, data: { items: state.links } }
    if (url === `/api/v2/tenants/${tenantId}/invite-links` && c.method === 'post') {
      seen.push(`create ${String(c.data)}`)
      state.links = [link('new'), ...state.links]
      return { status: 201, data: { link: link('new'), url: '/invite/tok123' } }
    }
    if (url.startsWith(`/api/v2/tenants/${tenantId}/invite-links/`) && c.method === 'delete') {
      seen.push(`revoke ${url.split('/').pop()}`)
      state.links = state.links.filter((l) => (l as { id: string }).id !== url.split('/').pop())
      return { status: 204 }
    }
    if (url === `/api/v1/tenants/${tenantId}/members`) {
      return { status: 200, data: { items: state.members.map((m) => ({ id: m.userId, tenantId, status: 'active', version: 1, ...m })), nextCursor: '' } }
    }
    if (url.startsWith(`/api/v2/tenants/${tenantId}/members/`) && c.method === 'delete') {
      seen.push(`remove ${url.split('/').pop()}`)
      state.members = state.members.filter((m) => m.userId !== url.split('/').pop())
      return { status: 204 }
    }
    return undefined
  })
}

describe('WorkspaceAccess', () => {
  it('generates a link into a live squad and shows its address once', async () => {
    const user = userEvent.setup()
    const seen: string[] = []
    serve({ links: [], members: [{ userId: me, displayName: '演示用户', role: 'owner' }] }, seen)
    renderSettingsPage(<WorkspaceAccess tenantId={tenantId} userId={me} squads={squads} />, '/settings/squads')
    const select = screen.getByLabelText('同时加入小队（可选）')
    expect(within(select).queryByText('旧小队')).not.toBeInTheDocument()
    await user.selectOptions(select, 'sq-live')
    await user.click(screen.getByRole('button', { name: '生成邀请链接' }))
    expect(await screen.findByLabelText('邀请链接')).toHaveTextContent(`${window.location.origin}/invite/tok123`)
    expect(JSON.parse(seen[0].slice('create '.length))).toEqual({ squadId: 'sq-live' })
    // The list shows the new link without its token.
    const unused = await screen.findByRole('region', { name: '未使用的邀请链接' })
    await waitFor(() => expect(within(unused).getByRole('button', { name: '吊销' })).toBeInTheDocument())
    expect(unused).not.toHaveTextContent('tok123')
  })

  it('revokes an unused link and lists who used the others', async () => {
    const user = userEvent.setup()
    const seen: string[] = []
    serve({
      links: [link('l1'), link('l2', { usedByName: '张三', usedAt: '2026-11-19T08:00:00Z' })],
      members: [{ userId: me, displayName: '演示用户', role: 'owner' }],
    }, seen)
    renderSettingsPage(<WorkspaceAccess tenantId={tenantId} userId={me} squads={squads} />, '/settings/squads')
    expect(await screen.findByRole('region', { name: '已使用的邀请链接' })).toHaveTextContent('张三')
    await user.click(screen.getByRole('button', { name: '吊销' }))
    await waitFor(() => expect(seen).toContain('revoke l1'))
    expect(await screen.findByText('没有未使用的链接。')).toBeInTheDocument()
  })

  it('never offers removing the last owner', async () => {
    const seen: string[] = []
    serve({
      links: [],
      members: [
        { userId: me, displayName: '演示用户', role: 'owner' },
        { userId: alice, displayName: 'Alice', role: 'member' },
      ],
    }, seen)
    renderSettingsPage(<WorkspaceAccess tenantId={tenantId} userId={alice} squads={squads} />, '/settings/squads')
    const roster = await screen.findByRole('region', { name: '工作区成员' })
    // Viewed by another owner, the only owner still cannot be removed.
    await waitFor(() => expect(within(roster).getByRole('button', { name: '移出' })).toBeDisabled())
  })

  it('confirms before removing', async () => {
    const user = userEvent.setup()
    const seen: string[] = []
    serve({
      links: [],
      members: [
        { userId: me, displayName: '演示用户', role: 'owner' },
        { userId: alice, displayName: 'Alice', role: 'member' },
      ],
    }, seen)
    renderSettingsPage(<WorkspaceAccess tenantId={tenantId} userId={me} squads={squads} />, '/settings/squads')
    const roster = await screen.findByRole('region', { name: '工作区成员' })
    await user.click(await within(roster).findByRole('button', { name: '移出' }))
    expect(seen).toEqual([])
    await user.click(within(roster).getByRole('button', { name: '确认移出' }))
    await waitFor(() => expect(seen).toContain(`remove ${alice}`))
    await waitFor(() => expect(within(roster).queryByText('Alice')).not.toBeInTheDocument())
  })
})
