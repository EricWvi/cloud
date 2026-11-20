import { describe, expect, it } from 'vitest'
import { marketCatalogRefetchInterval } from './marketplace'

describe('marketCatalogRefetchInterval', () => {
  it('keeps polling while the catalog has not synced', () => {
    expect(marketCatalogRefetchInterval(undefined)).toBe(3000)
    expect(marketCatalogRefetchInterval({ syncedAt: null })).toBe(3000)
  })

  it('stops polling once the catalog has a syncedAt', () => {
    expect(marketCatalogRefetchInterval({ syncedAt: '2026-11-20T00:00:00Z' })).toBe(false)
  })
})