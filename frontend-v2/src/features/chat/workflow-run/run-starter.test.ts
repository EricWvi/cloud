import { describe, expect, it } from 'vitest'
import { startedRun } from './run-starter'

describe('startedRun', () => {
  it('lets only the starter act, and never in a read-only host', () => {
    expect(startedRun({ id: 'u1' }, { startedBy: 'u1' }, false)).toBe(true)
    expect(startedRun({ id: 'u2' }, { startedBy: 'u1' }, false)).toBe(false)
    expect(startedRun({ id: 'u1' }, { startedBy: 'u1' }, true)).toBe(false)
    expect(startedRun(null, { startedBy: 'u1' }, false)).toBe(false)
    expect(startedRun({ id: 'u1' }, {}, false)).toBe(false)
  })
})
