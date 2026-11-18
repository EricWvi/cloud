import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Copy } from 'lucide-react'
import { getGetApiV1TenantsTidMembersQueryKey, useGetApiV1TenantsTidMembers } from '@/api/members/members'
import { Button } from '@/components/ui/button'
import { Field, FieldLabel } from '@/components/ui/field'
import { NativeSelect, NativeSelectOption } from '@/components/ui/native-select'
import { codeOf } from '@/lib/api-client'
import type { SquadSummary } from '@/lib/squads-api/generated'
import { createInviteLink, getListInviteLinksQueryKey, listInviteLinks, removeMember, revokeInviteLink } from '../api/generated'

const ROLE_LABELS: Record<string, string> = { owner: '管理员', member: '成员' }

const when = (iso: string) => new Date(iso).toLocaleString('zh-CN', { dateStyle: 'medium', timeStyle: 'short' })

/**
 * The owner's view of who belongs to the workspace and how others get in: one-time invitation links
 * (generate, optionally into a squad; list and revoke the unused ones; see who used the others) and
 * the roster, where a member can be removed. A link's address is shown once, right after it is
 * generated: the server keeps only its hash.
 */
export function WorkspaceAccess({ tenantId, userId, squads }: {
  tenantId: string
  /** The signed-in owner, who is not offered removing themselves. */
  userId: string
  /** The workspace's squads; archived ones cannot be joined. */
  squads: SquadSummary[]
}) {
  const client = useQueryClient()
  const [squadId, setSquadId] = useState('')
  const [created, setCreated] = useState('')
  const [copied, setCopied] = useState(false)
  const [confirming, setConfirming] = useState('')
  const links = useQuery({ queryKey: getListInviteLinksQueryKey(tenantId), queryFn: () => listInviteLinks(tenantId) })
  const roster = useGetApiV1TenantsTidMembers(tenantId)
  const refreshLinks = () => client.invalidateQueries({ queryKey: getListInviteLinksQueryKey(tenantId) })
  const generate = useMutation({
    mutationFn: () => createInviteLink(tenantId, squadId ? { squadId } : {}),
    onSuccess: async (result) => {
      setCreated(new URL(result.url, window.location.origin).toString())
      setCopied(false)
      await refreshLinks()
    },
  })
  const revoke = useMutation({ mutationFn: (id: string) => revokeInviteLink(tenantId, id), onSettled: refreshLinks })
  const remove = useMutation({
    mutationFn: (uid: string) => removeMember(tenantId, uid),
    onSuccess: async () => {
      setConfirming('')
      await Promise.all([
        client.invalidateQueries({ queryKey: getGetApiV1TenantsTidMembersQueryKey(tenantId) }),
        // Removal ends the person's squad memberships, so squad counts and details moved.
        client.invalidateQueries({ queryKey: ['squads', tenantId] }),
        client.invalidateQueries({ queryKey: ['squad', tenantId] }),
      ])
    },
  })
  const live = squads.filter((s) => !s.archivedAt)
  const open = (links.data?.items ?? []).filter((l) => !l.usedAt)
  const used = (links.data?.items ?? []).filter((l) => l.usedAt)
  const members = roster.data?.items ?? []
  const owners = members.filter((m) => m.role === 'owner').length

  return <div className="mc-picker">
    <div className="mc-picker-scroll">
      <section className="mc-section" aria-label="生成邀请链接">
        <h3>邀请链接</h3>
        <p className="text-sm text-muted-foreground">链接 7 天内有效，只能用一次。对方打开链接、登录后点「加入」即可。</p>
        <form className="flex flex-wrap items-end gap-3" onSubmit={(e) => { e.preventDefault(); generate.mutate() }}>
          <Field className="min-w-48 flex-1">
            <FieldLabel htmlFor="invite-link-squad">同时加入小队（可选）</FieldLabel>
            <NativeSelect id="invite-link-squad" value={squadId} onChange={(e) => setSquadId(e.target.value)}>
              <NativeSelectOption value="">不加入小队</NativeSelectOption>
              {live.map((s) => <NativeSelectOption key={s.id} value={s.id}>{s.name}</NativeSelectOption>)}
            </NativeSelect>
          </Field>
          <Button type="submit" disabled={generate.isPending}>生成邀请链接</Button>
        </form>
        {generate.isError && <p role="alert" className="text-sm text-destructive">
          {codeOf(generate.error) === 'squad_not_found' ? '这个小队已不存在或已归档。' : '生成失败，请重试。'}
        </p>}
        {created && <div role="status" className="flex flex-col gap-2 rounded-lg border p-3 text-sm">
          <span>链接只显示这一次，请复制后发给对方：</span>
          <div className="flex items-center gap-2">
            <code className="min-w-0 flex-1 break-all" aria-label="邀请链接">{created}</code>
            <Button type="button" variant="outline" size="sm" onClick={() => {
              void navigator.clipboard?.writeText(created).then(() => setCopied(true))
            }}><Copy data-icon="inline-start" />{copied ? '已复制' : '复制'}</Button>
          </div>
        </div>}
      </section>

      <section className="mc-section" aria-label="未使用的邀请链接">
        <h3>未使用的链接</h3>
        {links.isLoading ? <p role="status">正在加载…</p> : links.isError ? <p role="alert">加载失败，请刷新重试。</p>
          : open.length === 0 ? <p className="text-sm text-muted-foreground">没有未使用的链接。</p>
            : <ul className="flex flex-col gap-2">{open.map((l) => <li key={l.id} className="flex items-center justify-between gap-3 text-sm">
              <span>{l.createdByName ?? '部署命令'} 生成{l.squad ? `，加入「${l.squad.name}」` : ''}，{when(l.expiresAt)} 过期</span>
              <Button type="button" variant="outline" size="sm" disabled={revoke.isPending} onClick={() => revoke.mutate(l.id)}>吊销</Button>
            </li>)}</ul>}
        {revoke.isError && <p role="alert" className="text-sm text-destructive">
          {codeOf(revoke.error) === 'invite_already_used' ? '这个链接刚被使用，不能再吊销。' : '吊销失败，请重试。'}
        </p>}
      </section>

      {used.length > 0 && <section className="mc-section" aria-label="已使用的邀请链接">
        <h3>已使用</h3>
        <ul className="flex flex-col gap-2">{used.map((l) => <li key={l.id} className="text-sm">
          {l.usedByName} 于 {when(l.usedAt!)} 加入{l.squad ? `「${l.squad.name}」` : ''}
        </li>)}</ul>
      </section>}

      <section className="mc-section" aria-label="工作区成员">
        <h3>成员</h3>
        {roster.isLoading ? <p role="status">正在加载…</p> : <ul className="flex flex-col gap-2">{members.map((m) => {
          const lastOwner = m.role === 'owner' && owners <= 1
          return <li key={m.userId} className="flex items-center justify-between gap-3 text-sm">
            <span>{m.displayName} <span className="text-muted-foreground">{ROLE_LABELS[m.role] ?? m.role}</span></span>
            {m.userId !== userId && (confirming === m.userId
              ? <span className="flex items-center gap-2">
                <span>移出后对方看不到这个团队，账号仍保留。</span>
                <Button type="button" variant="destructive" size="sm" disabled={remove.isPending} onClick={() => remove.mutate(m.userId)}>确认移出</Button>
                <Button type="button" variant="ghost" size="sm" onClick={() => setConfirming('')}>取消</Button>
              </span>
              : <Button type="button" variant="outline" size="sm" disabled={lastOwner} title={lastOwner ? '不能移出最后一个管理员' : undefined}
                onClick={() => { remove.reset(); setConfirming(m.userId) }}>移出</Button>)}
          </li>
        })}</ul>}
        {remove.isError && <p role="alert" className="text-sm text-destructive">
          {codeOf(remove.error) === 'last_owner' ? '不能移出最后一个管理员。' : '移出失败，请重试。'}
        </p>}
      </section>
    </div>
  </div>
}
