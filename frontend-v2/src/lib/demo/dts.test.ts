import { afterEach, expect, it, vi } from 'vitest'
import { simulateDtsSubmission } from './dts'

const demo = vi.hoisted(() => ({ on: false }))
vi.mock('./flags', () => ({ get DEMO_MOCK() { return demo.on } }))

afterEach(() => { demo.on = false })
it('never returns a simulated success outside demo mode', async () => {
  demo.on = false
  expect(await simulateDtsSubmission('task')).toBeNull()
})
it('reuses the receipt identity of a task', async () => {
  demo.on = true
  expect(await simulateDtsSubmission('abc-123')).toEqual(await simulateDtsSubmission('abc-123'))
  expect((await simulateDtsSubmission('abc-123'))?.number).toBe('DTS-DEMO-ABC123')
})
