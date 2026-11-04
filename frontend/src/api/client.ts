import type { components } from './schema'

// Types generated from api/openapi.yaml (`make gen`). Never hand-edit schema.d.ts.
export type Health = components['schemas']['Health']
type ErrorBody = components['schemas']['Error']

/** A failed API call. status 0 means the server could not be reached at all. */
export class ApiError extends Error {
  readonly status: number
  readonly code: string

  constructor(status: number, code: string, message: string) {
    super(message)
    this.name = 'ApiError'
    this.status = status
    this.code = code
  }
}

interface GetOptions {
  signal?: AbortSignal
  /** Non-2xx statuses whose body is still a valid response (e.g. 503 from /api/health). */
  acceptStatuses?: number[]
}

export async function getJSON<T>(path: string, { signal, acceptStatuses = [] }: GetOptions = {}): Promise<T> {
  let res: Response
  try {
    res = await fetch(path, { headers: { Accept: 'application/json' }, credentials: 'same-origin', signal })
  } catch (err) {
    if (err instanceof DOMException && err.name === 'AbortError') throw err
    throw new ApiError(0, 'unreachable', 'Could not reach the server')
  }

  if (res.ok || acceptStatuses.includes(res.status)) {
    return (await res.json()) as T
  }

  let body: ErrorBody | undefined
  try {
    body = (await res.json()) as ErrorBody
  } catch {
    // Non-JSON error (e.g. a proxy page); fall through to a generic message.
  }
  throw new ApiError(res.status, body?.error.code ?? 'http_error', body?.error.message ?? `Request failed (${res.status})`)
}

export function fetchHealth(signal?: AbortSignal): Promise<Health> {
  return getJSON<Health>('/api/health', { signal, acceptStatuses: [503] })
}
