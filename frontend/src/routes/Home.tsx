import { useHealth } from '../api/health'
import { ApiError } from '../api/client'
import styles from './Home.module.css'

export function Home() {
  return (
    <>
      <section className={styles.hero}>
        <h1>Your campaign, remembered.</h1>
        <p className={styles.lede}>
          Upload session notes or audio and get a recap, an up-to-date world wiki, and a warning whenever something
          contradicts what your table already established.
        </p>
      </section>
      <SystemStatus />
    </>
  )
}

function SystemStatus() {
  const { data, error, isPending } = useHealth()

  return (
    <section className={styles.card} aria-labelledby="status-heading">
      <h2 id="status-heading" className={styles.cardTitle}>
        System status
      </h2>

      {isPending && <p className={styles.muted}>Contacting the API…</p>}

      {error && (
        <p className={styles.error}>
          {error instanceof ApiError && error.status === 0 ? (
            <>
              The API is not reachable. Is it running? Try <code>make dev</code>.
            </>
          ) : (
            `The API returned an error: ${error.message}`
          )}
        </p>
      )}

      {data && (
        <dl className={styles.facts}>
          <dt>API</dt>
          <dd>
            {data.status === 'ok' ? 'Healthy' : 'Degraded'} · version <code>{data.version}</code> · {data.env}{' '}
            environment
          </dd>
          <dt>Database</dt>
          <dd>{data.database.status === 'up' ? `Connected (${data.database.latency_ms} ms)` : 'Unreachable'}</dd>
          <dt>AI &amp; storage</dt>
          <dd>{data.provider === 'mock' ? 'Local mocks (no AWS calls)' : 'AWS'}</dd>
        </dl>
      )}
    </section>
  )
}
