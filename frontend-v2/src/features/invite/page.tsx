import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useNavigate, useParams } from 'react-router'
import { OraMark } from '@/components/ora-mark'
import { Button } from '@/components/ui/button'
import { codeOf, statusOf } from '@/lib/api-client'
import { acceptInvite, previewInvite } from '@/features/settings/api/generated'
import { SignOutButton } from '@/app/auth/sign-out-button'

const UNAVAILABLE = '链接已失效，请联系管理员重新生成。'

/**
 * `/invite/<token>`: where an invitation link lands. Opening it only previews the workspace; joining
 * is the 「加入」 button, a same-origin POST. The session gate around it has already signed the
 * visitor in, returning here afterwards. A member who opens a link just goes in, and the link stays
 * unused for whoever it was meant for.
 */
export default function InvitePage() {
  const { token = '' } = useParams()
  const navigate = useNavigate()
  const client = useQueryClient()
  const preview = useQuery({
    queryKey: ['invite', token],
    queryFn: () => previewInvite({ token }),
    retry: false,
    // The token is a secret; nothing about it should outlive this page.
    gcTime: 0,
  })
  const enter = async () => {
    // Membership is re-read on every request, so the shell sees the workspace once its queries refetch.
    await client.resetQueries()
    navigate('/', { replace: true })
  }
  const join = useMutation({ mutationFn: () => acceptInvite({ token }), onSuccess: enter })

  let body
  if (preview.isPending) {
    body = <p className="text-sm text-muted-foreground">正在读取邀请…</p>
  } else if (preview.isError) {
    body = <p role="alert" className="text-sm text-destructive">
      {statusOf(preview.error) === 404 ? '邀请链接无效，请检查链接是否完整。' : '无法读取邀请，请稍后重试。'}
    </p>
  } else {
    const { tenant, squad, role, state, alreadyMember } = preview.data
    const target = <>「{tenant.name}」{squad && <>的小队「{squad.name}」</>}</>
    if (alreadyMember) {
      body = <>
        <p>你已经是 {target} 的成员。</p>
        <Button onClick={() => void enter()}>进入团队</Button>
      </>
    } else if (state !== 'active') {
      body = <p role="alert" className="text-sm text-destructive">{UNAVAILABLE}</p>
    } else {
      body = <>
        <p>你被邀请以{role === 'owner' ? '管理员' : '成员'}身份加入 {target}。</p>
        <Button disabled={join.isPending} onClick={() => join.mutate()}>加入</Button>
        {join.isError && <p role="alert" className="text-sm text-destructive">
          {codeOf(join.error) === 'invite_unavailable' ? UNAVAILABLE : '加入失败，请重试。'}
        </p>}
      </>
    }
  }

  return <div className="flex h-svh items-center justify-center bg-sidebar px-4">
    <div className="flex w-full max-w-sm flex-col gap-4 rounded-2xl border bg-card p-8 text-sm">
      <OraMark className="size-10" />
      <h1 className="text-xl font-semibold tracking-tight">加入团队</h1>
      {body}
      <SignOutButton />
    </div>
  </div>
}
