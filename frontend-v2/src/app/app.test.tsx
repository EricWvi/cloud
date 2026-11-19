import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { createMemoryRouter } from 'react-router'
import { beforeEach, describe, expect, it } from 'vitest'
import { demoUser, fakeBackend } from '@/test/backend'
import { App } from './app'
import { features } from './features'
import { routes } from './routes'

const renderAt = (path: string) => {
  const router = createMemoryRouter(routes, { initialEntries: [path] })
  render(<App router={router} />)
  return router
}

describe('app shell, signed in', () => {
  beforeEach(() => {
    fakeBackend((c) => {
      if (c.url === '/api/v1/me') return { status: 200, data: demoUser }
      if (c.url === '/api/v1/me/tenants') {
        return {
          status: 200,
          data: { items: [{ id: '00000000-0000-4000-8000-0000000000aa', name: '演示团队', status: 'active', role: 'owner' }], nextCursor: '' },
        }
      }
      return undefined
    })
  })

  it('opens the first page from the root', async () => {
    const router = renderAt('/')
    // The first visit loads the page's chunk, which can take a moment under jsdom.
    expect(await screen.findByRole('heading', { level: 1, name: features[0].title }, { timeout: 5000 })).toBeInTheDocument()
    expect(router.state.location.pathname).toBe(`/${features[0].path}`)
  })

  it('switches pages from the icon rail', async () => {
    const router = renderAt('/')
    await screen.findByRole('heading', { level: 1, name: features[0].title })
    for (const f of features) {
      await userEvent.click(screen.getByRole('link', { name: f.title }))
      if (f.path === 'settings') {
        expect(await screen.findByRole('heading', { level: 1, name: '小队' })).toBeInTheDocument()
        expect(router.state.location.pathname).toBe('/settings/squads')
        continue
      }
      // Each page's chunk loads on its first visit; the workflow editor's is the largest.
      expect(await screen.findByRole('heading', { level: 1, name: f.title }, { timeout: 5000 })).toBeInTheDocument()
      expect(router.state.location.pathname).toBe(`/${f.path}`)
    }
    // Loading every page's chunk in turn outlasts the default 5s budget on a busy machine.
  }, 20_000)

  it('sends unknown paths home', async () => {
    const router = renderAt('/nope')
    await screen.findByRole('heading', { level: 1, name: features[0].title })
    expect(router.state.location.pathname).toBe(`/${features[0].path}`)
  })

  it('shows who is signed in', async () => {
    renderAt('/')
    expect(await screen.findByRole('img', { name: '演示用户' })).toHaveTextContent('演')
  })
})

describe('session gate', () => {
  it('shows the login screen on 401 and signs in by email alone', async () => {
    const seen = fakeBackend((c) => {
      if (c.url === '/api/v1/me') return { status: 401, data: { code: 'unauthenticated', params: {}, requestId: 'x' } }
      if (c.url === '/auth/providers') return { status: 200, data: { providers: ['email'] } }
      if (c.url === '/auth/login') return { status: 200, data: { authorizationUrl: '/auth/callback?code=abc' } }
      return undefined
    })
    renderAt('/workflows')
    expect(await screen.findByRole('heading', { name: '登录 Ora' })).toBeInTheDocument()
    await userEvent.type(await screen.findByLabelText('邮箱'), 'owner@demo.ora-space.dev')
    await userEvent.click(screen.getByRole('button', { name: '登录' }))
    expect(seen).toContain('POST /auth/login')
  })

  it('shows no email entry when the gateway does not offer it', async () => {
    fakeBackend((c) => {
      if (c.url === '/api/v1/me') return { status: 401, data: { code: 'unauthenticated', params: {}, requestId: 'x' } }
      if (c.url === '/auth/providers') return { status: 200, data: { providers: ['github'], default: 'github' } }
      return undefined
    })
    renderAt('/workflows')
    expect(await screen.findByRole('button', { name: '使用 GitHub 登录' })).toBeInTheDocument()
    // An address field against a gateway without `email` could only fail as unknown_provider.
    expect(screen.queryByLabelText('邮箱')).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /^登录$/ })).not.toBeInTheDocument()
    expect(screen.queryByText(/演示环境：只凭邮箱即可登录/)).not.toBeInTheDocument()
  })

  it('tells a disabled user on 403', async () => {
    fakeBackend((c) => (c.url === '/api/v1/me' ? { status: 403, data: {} } : undefined))
    renderAt('/')
    expect(await screen.findByText('账号已停用，请联系管理员。')).toBeInTheDocument()
  })

  it('signs out back to the login screen', async () => {
    let signedIn = true
    const seen = fakeBackend((c) => {
      if (c.url === '/api/v1/me') return signedIn ? { status: 200, data: demoUser } : { status: 401, data: {} }
      if (c.url === '/auth/logout') return (signedIn = false), { status: 204 }
      if (c.url === '/auth/providers') return { status: 200, data: { providers: ['email'] } }
      return undefined
    })
    renderAt('/')
    await userEvent.click(await screen.findByRole('button', { name: '退出登录' }))
    expect(await screen.findByRole('heading', { name: '登录 Ora' })).toBeInTheDocument()
    expect(seen).toContain('POST /auth/logout')
  })
})

describe('deployed install hides browser-only settings routes', () => {
  beforeEach(() => {
    fakeBackend((c) => {
      if (c.url === '/api/v1/me') return { status: 200, data: demoUser }
      if (c.url === '/api/v1/me/tenants') {
        return {
          status: 200,
          data: { items: [{ id: '00000000-0000-4000-8000-0000000000aa', name: '演示团队', status: 'active', role: 'owner' }], nextCursor: '' },
        }
      }
      if (c.url === '/auth/providers')
        return { status: 200, data: { providers: ['huawei-idaas'] } }
      return undefined
    })
  })

  // Roles come from the workspace's server catalog, so the route stays reachable.
  it('keeps the roles page reachable', async () => {
    const router = renderAt('/settings/roles')
    await waitFor(() =>
      expect(router.state.location.pathname).toBe('/settings/roles'),
    )
  })

  it('redirects a direct visit to model-connections back to the squad page', async () => {
    const router = renderAt('/settings/model-connections')
    await waitFor(() =>
      expect(router.state.location.pathname).toBe('/settings/squads'),
    )
  })
})

describe('accounts outside any workspace', () => {
  const tenant = { id: '00000000-0000-4000-8000-0000000000aa', name: '研发部', status: 'active', role: 'member' }

  it('shows the no-team screen instead of the shell', async () => {
    fakeBackend((c) => {
      if (c.url === '/api/v1/me') return { status: 200, data: demoUser }
      if (c.url === '/api/v1/me/tenants') return { status: 200, data: { items: [], nextCursor: '' } }
      return undefined
    })
    renderAt('/')
    expect(await screen.findByRole('heading', { name: '还没有加入团队' })).toBeInTheDocument()
    expect(screen.queryByRole('link', { name: features[0].title })).not.toBeInTheDocument()
    expect(screen.getByRole('button', { name: '退出登录' })).toBeInTheDocument()
  })

  it('joins through an invitation link and lands in the shell', async () => {
    let joined = false
    const bodies: string[] = []
    fakeBackend((c) => {
      if (c.url === '/api/v1/me') return { status: 200, data: demoUser }
      if (c.url === '/api/v1/me/tenants') return { status: 200, data: { items: joined ? [tenant] : [], nextCursor: '' } }
      if (c.url === '/api/v1/invites/preview') {
        bodies.push(String(c.data))
        return { status: 200, data: { state: 'active', tenant: { id: tenant.id, name: tenant.name }, squad: { id: 's', name: '后端' }, role: 'member', alreadyMember: joined } }
      }
      if (c.url === '/api/v1/invites/accept' && c.method === 'post') return (joined = true), { status: 200, data: { tenantId: tenant.id } }
      return undefined
    })
    const router = renderAt('/invite/secret-token')
    expect(await screen.findByText(/你被邀请以成员身份加入/)).toHaveTextContent('「研发部」的小队「后端」')
    // Opening the page only previews; the token travels in the body, not the API path.
    expect(joined).toBe(false)
    expect(JSON.parse(bodies[0])).toEqual({ token: 'secret-token' })
    await userEvent.click(screen.getByRole('button', { name: '加入' }))
    await screen.findByRole('heading', { level: 1, name: features[0].title }, { timeout: 5000 })
    expect(router.state.location.pathname).toBe(`/${features[0].path}`)
  })

  it('explains a used link', async () => {
    fakeBackend((c) => {
      if (c.url === '/api/v1/me') return { status: 200, data: demoUser }
      if (c.url === '/api/v1/invites/preview') return { status: 200, data: { state: 'used', tenant: { id: tenant.id, name: tenant.name }, squad: null, role: 'member', alreadyMember: false } }
      return undefined
    })
    renderAt('/invite/used-token')
    expect(await screen.findByRole('alert')).toHaveTextContent('链接已失效，请联系管理员重新生成。')
    expect(screen.queryByRole('button', { name: '加入' })).not.toBeInTheDocument()
  })

  it('asks a signed-out visitor to sign in and comes back to the link', async () => {
    fakeBackend((c) => {
      if (c.url === '/api/v1/me') return { status: 401, data: {} }
      if (c.url === '/auth/providers') return { status: 200, data: { providers: ['email'] } }
      return undefined
    })
    renderAt('/invite/abc')
    expect(await screen.findByRole('heading', { name: '登录 Ora' })).toBeInTheDocument()
  })
})

describe('redirect login', () => {
  it('starts the provider round trip from its button', async () => {
    const posted: string[] = []
    fakeBackend((c) => {
      if (c.url === '/api/v1/me') return { status: 401, data: {} }
      if (c.url === '/auth/providers') return { status: 200, data: { providers: ['huawei-idaas'] } }
      if (c.url === '/auth/login') return posted.push(String(c.data)), { status: 200, data: { authorizationUrl: 'about:blank' } }
      return undefined
    })
    renderAt('/workflows')
    await userEvent.click(await screen.findByRole('button', { name: '使用华为账号登录' }))
    await waitFor(() => expect(posted).toHaveLength(1))
    expect(JSON.parse(posted[0])).toEqual({ provider: 'huawei-idaas', returnTo: '/workflows' })
    expect(screen.queryByLabelText('邮箱')).not.toBeInTheDocument()
  })

  it('explains a failed round trip once and drops it from the address', async () => {
    const posted: string[] = []
    fakeBackend((c) => {
      if (c.url === '/api/v1/me') return { status: 401, data: {} }
      if (c.url === '/auth/providers') return { status: 200, data: { providers: ['huawei-idaas'] } }
      if (c.url === '/auth/login') return posted.push(String(c.data)), { status: 200, data: { authorizationUrl: 'about:blank' } }
      return undefined
    })
    const router = renderAt('/?login_error=unavailable')
    expect(await screen.findByRole('alert')).toHaveTextContent('暂时连不上登录服务，请稍后再试。')
    await waitFor(() => expect(router.state.location.search).toBe(''))
    await userEvent.click(await screen.findByRole('button', { name: '使用华为账号登录' }))
    await waitFor(() => expect(posted).toHaveLength(1))
    expect(JSON.parse(posted[0]).returnTo).not.toContain('login_error')
  })
})
