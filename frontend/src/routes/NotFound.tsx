import { Link } from 'react-router'

export function NotFound() {
  return (
    <section>
      <h1>Page not found</h1>
      <p>That page doesn’t exist. Maybe it was never written into the lore.</p>
      <Link to="/">Back to home</Link>
    </section>
  )
}
