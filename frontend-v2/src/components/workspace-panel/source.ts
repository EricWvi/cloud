import { cdaseWorkspaceReader } from '@/lib/demo/cdase'
import { DEMO_MOCK } from '@/lib/demo/flags'
import {
  getChatWorkspaceChanges,
  getChatWorkspaceDeliveryPatch,
  getChatWorkspaceDiff,
  getChatWorkspaceEntries,
  getChatWorkspaceFile,
  getTaskRunWorkspaceChanges,
  getTaskRunWorkspaceDeliveryPatch,
  getTaskRunWorkspaceDiff,
  getTaskRunWorkspaceEntries,
  getTaskRunWorkspaceFile,
  getWorkflowRunWorkspaceChanges,
  getWorkflowRunWorkspaceDeliveryPatch,
  getWorkflowRunWorkspaceDiff,
  getWorkflowRunWorkspaceEntries,
  getWorkflowRunWorkspaceFile,
  type WorkspaceChange,
  type WorkspaceChanges,
  type WorkspaceDiff,
  type WorkspaceDirectory,
  type WorkspaceFile,
} from '@/lib/workspace-api/generated'

/** Whose checkout the panel shows: a conversation's, a task run's, or the one a workflow run's Agents share. */
export type WorkspaceSource =
  | { kind: 'chat'; tenantId: string; conversationId: string }
  | { kind: 'run'; tenantId: string; runId: string }
  | { kind: 'task'; tenantId: string; taskId: string; runId: string }

/**
 * The read-only views of one source, and the query-key prefix every one of them shares: browsing
 * reads the live checkout, changes and diffs read the source's latest delivery (or, for a source
 * from before deliveries, the checkout), and a patch is one delivery's whole change.
 */
export interface WorkspaceReader {
  key: readonly string[]
  entries: (path: string) => Promise<WorkspaceDirectory>
  file: (path: string) => Promise<WorkspaceFile>
  changes: () => Promise<WorkspaceChanges>
  /** One file's diff; a delivery ID pins it to the delivery the change list showed. */
  diff: (path: string, deliveryId?: string) => Promise<WorkspaceDiff>
  /** One delivery's patch; absent where the source offers no download. */
  patch?: (deliveryId: string) => Promise<Blob>
}

const diffParams = (path: string, deliveryId?: string) => (deliveryId ? { path, deliveryId } : { path })

/** Binds the generated client to one source. */
export function workspaceReader(source: WorkspaceSource): WorkspaceReader {
  if (source.kind === 'chat') {
    const { tenantId: t, conversationId: c } = source
    return {
      key: ['workspace', 'chat', t, c],
      entries: (path) => getChatWorkspaceEntries(t, c, path ? { path } : undefined),
      file: (path) => getChatWorkspaceFile(t, c, { path }),
      changes: () => getChatWorkspaceChanges(t, c),
      diff: (path, id) => getChatWorkspaceDiff(t, c, diffParams(path, id)),
      patch: (id) => getChatWorkspaceDeliveryPatch(t, c, id) as Promise<Blob>,
    }
  }
  if (source.kind === 'task') {
    const { tenantId: t, taskId: k, runId: r } = source
    return {
      key: ['workspace', 'task', t, k, r],
      entries: (path) => getTaskRunWorkspaceEntries(t, k, r, path ? { path } : undefined),
      file: (path) => getTaskRunWorkspaceFile(t, k, r, { path }),
      changes: () => getTaskRunWorkspaceChanges(t, k, r),
      diff: (path, id) => getTaskRunWorkspaceDiff(t, k, r, diffParams(path, id)),
      patch: (id) => getTaskRunWorkspaceDeliveryPatch(t, k, r, id) as Promise<Blob>,
    }
  }
  const { tenantId: t, runId: r } = source
  const demo = DEMO_MOCK ? cdaseWorkspaceReader(t, r) : undefined
  if (demo) return demo
  return {
    key: ['workspace', 'run', t, r],
    entries: (path) => getWorkflowRunWorkspaceEntries(t, r, path ? { path } : undefined),
    file: (path) => getWorkflowRunWorkspaceFile(t, r, { path }),
    changes: () => getWorkflowRunWorkspaceChanges(t, r),
    diff: (path, id) => getWorkflowRunWorkspaceDiff(t, r, diffParams(path, id)),
    patch: (id) => getWorkflowRunWorkspaceDeliveryPatch(t, r, id) as Promise<Blob>,
  }
}

/**
 * The checkout-relative path for a path an Agent reported. Agents report absolute paths inside
 * their checkout (`…/w/r/<checkout>/src/a.go`); the panel speaks relative ones. A path already
 * among the changes, or ending with one of them, wins over the layout guess.
 */
export function relativePath(path: string, changes: readonly WorkspaceChange[] = []): string {
  const normalized = path.replaceAll('\\', '/')
  const exact = changes.find((c) => c.path === normalized)
  if (exact) return exact.path
  const suffix = changes
    .filter((c) => normalized.endsWith(`/${c.path}`))
    .sort((a, b) => b.path.length - a.path.length)[0]
  if (suffix) return suffix.path
  const inCheckout = normalized.match(/\/w\/r\/[^/]+\/(.+)$/)?.[1]
  return inCheckout ?? normalized.replace(/^\/+/, '')
}

/** The highlighter language for a file name, or '' to show it plain. */
export function languageOf(path: string): string {
  const name = path.split('/').at(-1)?.toLowerCase() ?? ''
  const ext = name.includes('.') ? name.split('.').at(-1)! : ''
  const byExt: Record<string, string> = {
    ts: 'typescript', mts: 'typescript', cts: 'typescript', tsx: 'tsx', js: 'javascript', mjs: 'javascript', cjs: 'javascript', jsx: 'jsx',
    json: 'json', md: 'markdown', py: 'python', sh: 'bash', bash: 'bash', zsh: 'bash', yml: 'yaml', yaml: 'yaml',
    css: 'css', html: 'html', sql: 'sql', diff: 'diff', patch: 'diff',
  }
  return byExt[ext] ?? ''
}
