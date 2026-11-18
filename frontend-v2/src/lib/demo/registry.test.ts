import { describe, expect, it } from 'vitest'
import { demoMocks } from './registry'

// Every module in this directory except the switch, the register itself and tests.
const infrastructure = new Set(['flags.ts', 'registry.ts'])
const sources = Object.fromEntries(
  Object.entries(import.meta.glob<string>('./*.{ts,tsx}', { query: '?raw', import: 'default', eager: true }))
    .map(([path, source]) => [path.slice(2), source] as const)
    .filter(([file]) => !infrastructure.has(file) && !/\.test\.tsx?$/.test(file)),
)
const modules = Object.keys(sources)

describe('demo mock register', () => {
  it('lists every mock module exactly once', () => {
    expect(demoMocks.map((mock) => mock.module).sort()).toEqual([...modules].sort())
  })

  it('names the backend that replaces each mock', () => {
    for (const mock of demoMocks) {
      expect(mock.covers.trim(), mock.module).not.toBe('')
      expect(mock.replacedBy.trim(), mock.module).not.toBe('')
    }
  })

  // The build check looks for these markers; one that is not in its module can never be found.
  it('takes each marker from its own module', () => {
    for (const mock of demoMocks) {
      expect(mock.marker.trim(), mock.module).not.toBe('')
      expect(sources[mock.module], mock.module).toContain(mock.marker)
    }
  })
})
