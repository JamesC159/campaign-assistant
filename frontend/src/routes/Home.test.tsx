import { screen } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import type { Health } from '../api/client'
import { mockFetchJSON, renderRoute } from '../test/render'

const healthy: Health = {
  status: 'ok',
  version: 'dev',
  env: 'dev',
  provider: 'mock',
  database: { status: 'up', latency_ms: 3 },
}

afterEach(() => vi.unstubAllGlobals())

describe('Home', () => {
  it('shows healthy status from the API', async () => {
    mockFetchJSON(200, healthy)
    renderRoute('/')

    expect(await screen.findByText('All systems up')).toBeInTheDocument()
    expect(screen.getByText('Connected (3 ms)')).toBeInTheDocument()
    expect(screen.getByText('Local mocks (no AWS calls)')).toBeInTheDocument()
  })

  it('treats a 503 health response as degraded, not as a failure', async () => {
    mockFetchJSON(503, { ...healthy, status: 'degraded', database: { status: 'down', latency_ms: 2000 } })
    renderRoute('/')

    expect(await screen.findByText('Degraded')).toBeInTheDocument()
    expect(screen.getByText('Unreachable')).toBeInTheDocument()
  })

  it('explains when the API cannot be reached', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => Promise.reject(new TypeError('Failed to fetch'))))
    renderRoute('/')

    expect(await screen.findByText('API offline')).toBeInTheDocument()
    expect(screen.getByText(/API is not reachable/)).toBeInTheDocument()
  })
})

describe('routing', () => {
  it('renders a not-found page for unknown paths', async () => {
    mockFetchJSON(200, healthy)
    renderRoute('/no/such/page')

    expect(await screen.findByRole('heading', { name: 'Page not found' })).toBeInTheDocument()
  })
})
