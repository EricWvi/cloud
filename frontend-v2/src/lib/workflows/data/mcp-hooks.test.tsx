import { act, renderHook, waitFor } from '@testing-library/react'
import { afterEach, expect, it, vi } from 'vitest'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import type { ReactNode } from 'react'
import { UserContext } from '@/app/auth/user'
import { demoUser, fakeBackend } from '@/test/backend'
import { http } from '@/lib/api-client'
import { useDemoPluginSelections } from '@/lib/demo/plugin-installs'
import { useInstalledPlugins } from './catalog-hooks'
import { createContractsClient, createHttpWorkflowContracts } from './contracts-client'
import { ContractsClientProvider } from './contracts-client-provider'

const original = http.defaults.adapter
afterEach(() => { http.defaults.adapter = original; useDemoPluginSelections.getState().reset() })

const mock = vi.hoisted(() => ({ enabled: true }))
vi.mock('@/lib/demo/flags', () => ({ demoMockEnabled: () => mock.enabled }))

it('ignores historical browser MCP choices and refreshes after server installation changes', async () => {
  let installed = false
  fakeBackend((config) => config.url === '/api/v1/tenants/t/mcp' ? ({ status: 200, data: { items: installed ? [{
    id: 'official/acme.tools', name: 'Tools', pluginVersion: '1.0.0', complete: true,
  }] : [] } }) : { status: 503 })
  const client = createContractsClient(createHttpWorkflowContracts('t'), 't')
  const query = new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: Infinity } } })
  const wrapper = ({ children }: { children: ReactNode }) => <UserContext.Provider value={demoUser}>
    <QueryClientProvider client={query}><ContractsClientProvider client={client}>{children}</ContractsClientProvider></QueryClientProvider>
  </UserContext.Provider>
  const { result } = renderHook(useInstalledPlugins, { wrapper })
  await waitFor(() => expect(result.current.data).toEqual([]))
  act(() => useDemoPluginSelections.getState().setSelected(`${demoUser.id}:t:s`, 'demo/sample-mcp', true))
  expect(result.current.data).toEqual([])
  installed = true
  await act(() => query.invalidateQueries({ queryKey: ['workflow-catalog', 'plugins'] }))
  await waitFor(() => expect(result.current.data?.map((p) => p.id)).toEqual(['official/acme.tools']))
  installed = false
  await act(() => query.invalidateQueries({ queryKey: ['workflow-catalog', 'plugins'] }))
  await waitFor(() => expect(result.current.data).toEqual([]))
})

it('keeps server-installed MCPs visible when the supplemental space directory fails', async () => {
  fakeBackend(() => ({ status: 503 }))
  const client = createContractsClient(createHttpWorkflowContracts('t'), 't')
  vi.spyOn(client.plugin, 'listInstalled').mockResolvedValue({ plugins: [{
    id: 'official/server-tools', namespace: 'official', name: 'server-tools',
    displayName: 'Server tools', version: '1', description: '', homepage: null,
    license: null, logo: null, kind: 'mcp', runtime: 'stopped',
    installationValidity: { validity: 'valid' }, configuration: { state: 'not_declared' },
  }] })
  const query = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  const wrapper = ({ children }: { children: ReactNode }) => <UserContext.Provider value={demoUser}>
    <QueryClientProvider client={query}><ContractsClientProvider client={client}>{children}</ContractsClientProvider></QueryClientProvider>
  </UserContext.Provider>
  const { result } = renderHook(useInstalledPlugins, { wrapper })
  await waitFor(() => expect(result.current.data?.map((p) => p.id)).toEqual(['official/server-tools']))
  expect(result.current.isError).toBe(false)
})
