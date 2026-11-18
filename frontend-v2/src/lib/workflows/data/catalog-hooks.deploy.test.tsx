import { describe, expect, it } from 'vitest'
import { renderHook, waitFor } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import type { ReactNode } from 'react'
import { UserContext } from '@/app/auth/user'
import { fakeBackend, demoUser } from '@/test/backend'
import { useAgents } from './catalog-hooks'

const tid = '00000000-0000-4000-8000-0000000000aa'

function wrapperFor(client: QueryClient) {
  return ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={client}>
      <UserContext.Provider value={demoUser}>{children}</UserContext.Provider>
    </QueryClientProvider>
  )
}

function serve(providers: string[]) {
  fakeBackend((c) =>
    c.url === '/auth/providers'
      ? { status: 200, data: { providers } }
      : c.url === '/api/v1/me/tenants'
        ? { status: 200, data: { items: [{ id: tid, name: '演示团队', role: 'member', status: 'active' }], nextCursor: '' } }
        : c.url === `/api/v2/tenants/${tid}/roles`
          ? { status: 200, data: { items: [{
              id: 'role-reviewer',
              name: '代码审查',
              description: '检查改动',
              content: '# 代码审查',
              version: 1,
              createdAt: '2026-11-02T08:00:00Z',
              updatedAt: '2026-11-02T08:00:00Z',
            }] } }
          : undefined,
  )
}

function mount() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  return renderHook(() => useAgents(), { wrapper: wrapperFor(client) })
}

describe('useAgents in a deployed install', () => {
  // Roles are a workspace server catalog, so a deployed install offers them exactly like a
  // development one; only browser-local settings stay hidden.
  it('offers the workspace roles in a deployed install', async () => {
    serve(['huawei-idaas'])
    const { result } = mount()
    await waitFor(() => expect(result.current.data?.length).toBe(1))
    expect(result.current.data?.map((role) => role.name)).toEqual(['代码审查'])
  })

  it('offers the same workspace roles when the gateway is email (development)', async () => {
    serve(['email'])
    const { result } = mount()
    await waitFor(() => expect(result.current.data?.length).toBe(1))
    expect(result.current.data?.map((role) => role.name)).toEqual(['代码审查'])
  })
})
