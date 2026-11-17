import { NavLink, Route, Routes } from 'react-router-dom'

function Home() {
  return (
    <section>
      <h1>Welcome to RelayForge</h1>
      <p>Reliable webhook infrastructure. Features will appear here as they are built.</p>
    </section>
  )
}

function NotFound() {
  return (
    <section>
      <h1>Page not found</h1>
      <p>The page you requested does not exist.</p>
    </section>
  )
}

export function App() {
  return (
    <div className="shell">
      <header className="shell-header">
        <span className="brand">RelayForge</span>
        <nav aria-label="Main navigation">
          <NavLink to="/" end>
            Home
          </NavLink>
        </nav>
      </header>
      <main className="shell-main">
        <Routes>
          <Route path="/" element={<Home />} />
          <Route path="*" element={<NotFound />} />
        </Routes>
      </main>
      <footer className="shell-footer">RelayForge &middot; Phase 0 foundation</footer>
    </div>
  )
}
