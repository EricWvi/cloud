import { afterEach, expect, it, vi } from 'vitest'

afterEach(() => {
  vi.unstubAllEnvs()
  vi.resetModules()
})

// The flag is read once, when the module loads, so each case loads a fresh copy.
it.each([
  ['1', true],
  ['', false],
  ['0', false],
  ['true', false],
])('VITE_DEMO_MOCK=%j turns the demo mocks %s', async (value, on) => {
  vi.stubEnv('VITE_DEMO_MOCK', value)
  const { DEMO_MOCK } = await import('./flags')
  expect(DEMO_MOCK).toBe(on)
})
