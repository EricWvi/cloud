import { useEffect, useState, type ReactNode } from 'react'
import { WorkspaceSplit } from '@/components/workspace-panel/workspace-split'

const narrowQuery = '(max-width: 760px)'

/** On phones the selected workspace uses the available width; closing it restores the task. */
export function TaskWorkspace({ panel, children }: { panel: ReactNode; children: ReactNode }) {
  const [narrow, setNarrow] = useState(() => window.matchMedia?.(narrowQuery).matches ?? false)
  useEffect(() => {
    const media = window.matchMedia?.(narrowQuery)
    if (!media) return
    const update = () => setNarrow(media.matches)
    media.addEventListener('change', update)
    return () => media.removeEventListener('change', update)
  }, [])
  if (narrow) return <div className="min-h-0 min-w-0 flex-1 overflow-auto">{panel || children}</div>
  return <WorkspaceSplit id="task" panel={panel}>{children}</WorkspaceSplit>
}
