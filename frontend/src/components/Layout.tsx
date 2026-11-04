import { Link, Outlet } from 'react-router'
import { StatusBadge } from './StatusBadge'
import styles from './Layout.module.css'

export function Layout() {
  return (
    <div className={styles.shell}>
      <header className={styles.header}>
        <div className={styles.headerInner}>
          <Link to="/" className={styles.brand}>
            <img src="/favicon.svg" alt="" width={24} height={24} />
            Campaign Assistant
          </Link>
          <StatusBadge />
        </div>
      </header>
      <main className={styles.main}>
        <Outlet />
      </main>
    </div>
  )
}
