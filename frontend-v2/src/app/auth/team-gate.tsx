import type { ReactNode } from 'react'
import { useGetApiV1MeTenants } from '@/api/me/me'
import { OraMark } from '@/components/ora-mark'
import { SignOutButton } from './sign-out-button'

/**
 * Signing in does not join a team (A6 D4): an account nobody invited belongs to no workspace, and
 * every page below needs one. Such an account sees this screen instead of an empty shell until an
 * invitation link adds it. While the memberships load, or if they fail, the shell renders as before
 * and its pages report their own state.
 */
export function TeamGate({ children }: { children: ReactNode }) {
  const tenants = useGetApiV1MeTenants()
  if (tenants.data && tenants.data.items.length === 0) {
    return <div className="flex h-svh items-center justify-center bg-sidebar px-4">
      <div className="flex w-full max-w-sm flex-col gap-4 rounded-2xl border bg-card p-8 text-sm">
        <OraMark className="size-10" />
        <h1 className="text-xl font-semibold tracking-tight">还没有加入团队</h1>
        <p className="text-muted-foreground">请联系团队管理员要一个邀请链接，打开链接加入后就能使用。</p>
        <SignOutButton />
      </div>
    </div>
  }
  return children
}
