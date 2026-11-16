import { createContext, useContext } from 'react'

/**
 * Whether the person viewing the run started it. A node's session belongs to the run's starter, so
 * only they may talk to, stop or complete an interactive node; everyone else reads. Views outside
 * a run workspace, and read-only hosts, leave it false.
 */
export const RunStarterContext = createContext(false)

/** True when the viewer may act on the run's interactive nodes. */
export function useIsRunStarter(): boolean {
  return useContext(RunStarterContext)
}

/**
 * Whether `viewer` started `run` and may act on it: a read-only host never acts, a run that has not
 * started has no starter, and a missing viewer (outside the signed-in shell) is nobody.
 */
export function startedRun(viewer: { id: string } | null, run: { startedBy?: string }, readOnly: boolean): boolean {
  return !readOnly && viewer !== null && run.startedBy !== undefined && run.startedBy === viewer.id
}
