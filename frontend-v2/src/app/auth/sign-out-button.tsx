import { useMutation, useQueryClient } from '@tanstack/react-query'
import { Button } from '@/components/ui/button'
import { logout } from '@/lib/auth-api'
import { forgetAuthIdentity } from './sync'

/** A plain 「退出登录」 for full-page screens outside the shell's icon rail. */
export function SignOutButton() {
  const queryClient = useQueryClient()
  // Dropping every cached query makes the session gate ask /api/v1/me again and show the login.
  const signOut = useMutation({
    mutationFn: logout,
    onSuccess: () => {
      forgetAuthIdentity()
      void queryClient.resetQueries()
    },
  })
  return <Button variant="ghost" size="sm" className="self-start" disabled={signOut.isPending} onClick={() => signOut.mutate()}>
    退出登录
  </Button>
}
