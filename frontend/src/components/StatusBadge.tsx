import { useHealth } from '../api/health'
import styles from './StatusBadge.module.css'

type Tone = 'ok' | 'warn' | 'danger' | 'pending'

/** Compact API health indicator for the app header. */
export function StatusBadge() {
  const { data, isPending, isError } = useHealth()

  let tone: Tone
  let label: string
  if (isPending) {
    tone = 'pending'
    label = 'Checking…'
  } else if (isError) {
    tone = 'danger'
    label = 'API offline'
  } else if (data.status === 'ok') {
    tone = 'ok'
    label = 'All systems up'
  } else {
    tone = 'warn'
    label = 'Degraded'
  }

  return (
    <span className={styles.badge} data-tone={tone} role="status" aria-live="polite">
      <span className={styles.dot} aria-hidden="true" />
      {label}
    </span>
  )
}
