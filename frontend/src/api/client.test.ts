import { afterEach, describe, expect, it, vi } from 'vitest'
import { ApiError, getJSON } from './client'
import { mockFetchJSON } from '../test/render'

afterEach(() => vi.unstubAllGlobals())

describe('getJSON', () => {
  it('parses the API error envelope into ApiError', async () => {
    mockFetchJSON(404, { error: { code: 'not_found', message: 'no such endpoint' } })

    const err = await getJSON('/api/x').catch((e: unknown) => e)
    expect(err).toBeInstanceOf(ApiError)
    expect(err).toMatchObject({ status: 404, code: 'not_found', message: 'no such endpoint' })
  })

  it('falls back to a generic error for non-JSON bodies', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => new Response('<html>Bad gateway</html>', { status: 502 })))

    await expect(getJSON('/api/x')).rejects.toMatchObject({ status: 502, code: 'http_error' })
  })

  it('returns the body for explicitly accepted non-2xx statuses', async () => {
    mockFetchJSON(503, { status: 'degraded' })

    await expect(getJSON('/api/health', { acceptStatuses: [503] })).resolves.toEqual({ status: 'degraded' })
  })

  it('reports network failures as status 0', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => Promise.reject(new TypeError('Failed to fetch'))))

    await expect(getJSON('/api/x')).rejects.toMatchObject({ status: 0, code: 'unreachable' })
  })
})
