import { afterEach, expect, it, vi } from 'vitest'
import { cdaseInput, cdaseReport, createCdaseRun, readCdaseRun, startCdaseRun, cdaseProjectRuns, endCdaseRun, isCdaseRun } from './cdase'

const demo = vi.hoisted(() => ({ on: false }))
vi.mock('./flags', () => ({ get DEMO_MOCK() { return demo.on } }))

afterEach(() => { demo.on = false })
it('keeps demo data unavailable by default and scoped to the tenant and project', () => {
  demo.on = false
  expect(() => createCdaseRun('t', 'p', '')).toThrow('演示未启用')
  demo.on = true
  const id = createCdaseRun('t', 'p', '', 0)
  expect(() => readCdaseRun('another', id)).toThrow()
  expect(cdaseProjectRuns('t', 'other')).toEqual([])
  demo.on = false
  expect(isCdaseRun(id)).toBe(false)
  expect(cdaseProjectRuns('t', 'p')).toEqual([])
})
it('projects real view models without claiming repository changes or completed tests', () => {
  demo.on = true
  const id = createCdaseRun('t', 'p', '退款风险评审', 0)
  expect(() => startCdaseRun('t', id, 'unrelated task', 0)).toThrow()
  startCdaseRun('t', id, cdaseInput, 100)
  expect(readCdaseRun('t', id, 2000).status).toBe('running')
  const done = readCdaseRun('t', id, 9200)
  expect(done.status).toBe('succeeded')
  expect(Object.values(done.nodeStates).every(n => n.status === 'succeeded')).toBe(true)
  expect(done.finalOutput).toBe(cdaseReport)
  expect(done.finalOutput).toContain('未修改仓库文件')
  expect(done.finalOutput).toContain('尚未整改或执行测试')
  expect(Object.values(done.nodeStates).some(n => n.fileChanges?.length)).toBe(false)
})
it('cancels only its own fixture, and repeated runs start pending', () => {
  demo.on = true
  const id = createCdaseRun('t', 'p', '')
  startCdaseRun('t', id, cdaseInput)
  expect(() => endCdaseRun('t', id, true)).toThrow()
  endCdaseRun('t', id, false)
  expect(readCdaseRun('t', id).status).toBe('cancelled')
  endCdaseRun('t', id, true)
  expect(() => readCdaseRun('t', id)).toThrow()
  expect(readCdaseRun('t', createCdaseRun('t', 'p', '')).status).toBe('pending')
})
