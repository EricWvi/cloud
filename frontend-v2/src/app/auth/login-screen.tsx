import { useEffect, useState } from 'react'
import { useMutation, useQuery } from '@tanstack/react-query'
import { useLocation, useNavigate } from 'react-router'
import { OraMark } from '@/components/ora-mark'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { codeOf } from '@/lib/api-client'
import { getProviders, startLogin } from '@/lib/auth-api'

const LABELS: Record<string, string> = { github: '使用 GitHub 登录', 'huawei-idaas': '使用华为账号登录' }

/** Why a redirect login came back without a session; v2-server sends the category, never details. */
const LOGIN_ERRORS: Record<string, string> = {
  failed: '登录没有成功，请重试。',
  unavailable: '暂时连不上登录服务，请稍后再试。',
  disabled: '账号已停用，请联系管理员。',
}

/**
 * Reads `login_error` once and removes it from the address, so neither a reload nor the next
 * login's returnTo carries it.
 */
function useLoginError(): string {
  const location = useLocation()
  const navigate = useNavigate()
  const code = new URLSearchParams(location.search).get('login_error')
  const [error] = useState(() => (code === null ? '' : LOGIN_ERRORS[code] ?? LOGIN_ERRORS.failed))
  useEffect(() => {
    if (code === null) return
    const params = new URLSearchParams(location.search)
    params.delete('login_error')
    const search = params.toString()
    navigate({ pathname: location.pathname, search: search ? `?${search}` : '', hash: location.hash }, { replace: true })
  }, [code, location.pathname, location.search, location.hash, navigate])
  return error
}

/**
 * The login screen the session gate shows in place, with the methods V2_LOGIN offers in its order.
 * `email` signs in by address alone, whoever the startup preset lists or an owner invited, so its
 * form says plainly that knowing an address is enough; it only appears when the server lists
 * `email`. Every other method is a button that leaves for that provider's sign-in page; a failed
 * round trip comes back with `?login_error=<category>`, shown here once.
 */
export function LoginScreen({ returnTo }: { returnTo: string }) {
  const loginError = useLoginError()
  const providers = useQuery({ queryKey: ['auth', 'providers'], queryFn: getProviders, retry: false })
  const [email, setEmail] = useState('')
  const login = useMutation({
    mutationFn: (address: string) => startLogin('email', returnTo, address),
    onSuccess: (url) => window.location.assign(url),
  })
  const providerLogin = useMutation({
    mutationFn: (provider: string) => startLogin(provider, returnTo),
    onSuccess: (url) => window.location.assign(url),
  })
  const offered = providers.data?.providers ?? []
  const emailOffered = offered.includes('email')
  const providerButtons = offered.filter((p) => p !== 'email')
  const refused = codeOf(login.error) === 'email_not_invited'

  return (
    <div className="flex h-svh items-center justify-center bg-sidebar bg-[radial-gradient(ellipse_at_top,var(--brand-soft),transparent_60%)] px-4">
      <form
        className="flex w-full max-w-sm flex-col gap-5 rounded-2xl border bg-card p-8 shadow-[0_12px_40px_var(--shadow-color)]"
        onSubmit={(e) => {
          e.preventDefault()
          login.mutate(email.trim())
        }}
      >
        <div className="flex flex-col gap-3">
          <OraMark className="size-10" />
          <h1 className="text-xl font-semibold tracking-tight">登录 Ora</h1>
        </div>
        {loginError && <p role="alert" className="text-sm text-destructive">{loginError}</p>}
        {providers.isPending && <p className="text-sm text-muted-foreground">加载中…</p>}
        {providers.isError && <p className="text-sm text-destructive">无法获取登录方式，请稍后重试。</p>}
        {emailOffered && (
          <>
            <div className="flex flex-col gap-2">
              <Label htmlFor="login-email">邮箱</Label>
              <Input
                id="login-email"
                type="email"
                autoComplete="email"
                required
                placeholder="owner@demo.ora-space.dev"
                value={email}
                onChange={(e) => setEmail(e.target.value)}
              />
            </div>
            <Button type="submit" size="lg" disabled={login.isPending || email.trim() === ''}>
              登录
            </Button>
            {refused && <p className="text-sm text-destructive">该邮箱不在演示团队中，请让管理员邀请后再登录。</p>}
            {login.isError && !refused && <p className="text-sm text-destructive">登录失败，请重试。</p>}
          </>
        )}
        {providerButtons.map((p) => (
          <Button key={p} type="button" variant="outline" disabled={providerLogin.isPending} onClick={() => providerLogin.mutate(p)}>
            {LABELS[p] ?? p}
          </Button>
        ))}
        {emailOffered && (
          <p className="rounded-lg bg-warning-soft px-3 py-2 text-xs leading-relaxed text-warning">
            演示环境：只凭邮箱即可登录，知道邮箱就能冒充任何人。请勿在演示环境之外使用。
          </p>
        )}
      </form>
    </div>
  )
}
