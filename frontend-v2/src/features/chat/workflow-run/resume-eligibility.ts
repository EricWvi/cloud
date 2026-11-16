import type { WorkflowRunResumePreviewReason } from '@/lib/workflows/api/generated'
import type { GraphWorkflowRun, WorkflowNodeErrorDetail } from '@/lib/workflows/runtime'

/**
 * Whether 「从失败处继续」 can help a node that failed this way. A kind v2-server marks not
 * resumable comes from the frozen graph or configuration (`missing_agent_ref`, a broken template,
 * an unsupported structure), which a resume of the same snapshot cannot change, unless the next
 * attempt is told about the failure. A node without a recorded failure (cancelled, or an old row)
 * can always be resumed.
 */
export function resumeCanHelp(detail: WorkflowNodeErrorDetail | undefined): boolean {
  return detail === undefined || detail.resumable || detail.injectsPreviousFailure === true
}

/**
 * Whether the run has something to resume from: a failed or cancelled node at the root, in an
 * Iteration round or in a Loop round. A run cancelled before any node ran has none, and v2-server
 * would refuse it as `no_failed_node`, so the page does not offer the action.
 */
export function hasResumableNode(run: GraphWorkflowRun): boolean {
  const ended = (status: string) => status === 'failed' || status === 'cancelled'
  return Object.values(run.nodeStates).some((state) => ended(state.status)) ||
    Object.values(run.roundStates ?? {}).some((states) => states.some((state) => ended(state.status))) ||
    (run.rounds ?? []).some((round) => Object.values(round.nodeStates).some((state) => ended(state.status)))
}

/** A refused resume as v2-server explains it (`workflow_run_not_resumable` params). */
export interface ResumeRefusal {
  reason: NonNullable<WorkflowRunResumePreviewReason>
  runningNodeIds: string[]
  /**
   * The preview, serialized, that was shown when the server refused. The refusal holds only while
   * the preview stays the same: once it changes (a settling turn ended), the dialog drops it.
   */
  basis?: string
}

const refusalReasons: ReadonlySet<string> = new Set([
  'run_not_ended', 'node_running', 'node_stuck', 'no_failed_node', 'review_form', 'review_form_retry', 'review_form_submitted', 'task_handling', 'task_decomposition', 'run_settling',
])

/** Reads a `workflow_run_not_resumable` refusal's params; an unknown reason reads as none. */
export function resumeRefusalOf(params: Record<string, unknown>): ResumeRefusal | undefined {
  const { reason, runningNodeIds } = params
  if (typeof reason !== 'string' || !refusalReasons.has(reason)) return undefined
  const ids = Array.isArray(runningNodeIds) ? runningNodeIds.filter((id): id is string => typeof id === 'string') : []
  return { reason: reason as ResumeRefusal['reason'], runningNodeIds: ids }
}
