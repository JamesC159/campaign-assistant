import { isRouteErrorResponse, Link, useRouteError } from 'react-router'

/** Shown when a route throws while rendering or loading. */
export function RouteError() {
  const error = useRouteError()
  const message = isRouteErrorResponse(error)
    ? `${error.status} ${error.statusText}`
    : error instanceof Error
      ? error.message
      : 'Unknown error'

  return (
    <main style={{ maxWidth: 'var(--content-width)', margin: '0 auto', padding: 'var(--space-8) var(--space-4)' }}>
      <h1>Something went wrong</h1>
      <p>{message}</p>
      <Link to="/">Back to home</Link>
    </main>
  )
}
