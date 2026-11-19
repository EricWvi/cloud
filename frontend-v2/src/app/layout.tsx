import { Suspense } from 'react'
import { Outlet } from 'react-router'
import { SessionGate } from './auth/session'
import { TeamGate } from './auth/team-gate'
import { NavRail } from './nav-rail'

/** Signed-in members of a workspace only. Icon rail on the left; the rest of the window belongs to the current feature page. */
export function Layout() {
  return (
    <SessionGate>
      <TeamGate>
        <div className="flex h-svh">
          <NavRail />
          <main className="flex min-w-0 flex-1 flex-col overflow-auto bg-background">
            <Suspense fallback={<p className="p-4 text-sm text-muted-foreground">加载中…</p>}>
              <Outlet />
            </Suspense>
          </main>
        </div>
      </TeamGate>
    </SessionGate>
  )
}
