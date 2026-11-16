import { useQuery } from '@tanstack/react-query'
import { useEffect } from 'react'
import { IconAlertTriangle, IconLoader2 } from '@tabler/icons-react'
import { useTranslation } from 'react-i18next'
import type { WorkflowRunResumePreview } from '@/lib/workflows/api/generated'
import { Button, Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/lib/workflows/host/ui'
import type { WorkflowDefinition } from '@/lib/workflows/runtime'
import type { ResumeRefusal } from './resume-eligibility'
import { runRefreshMs, runResumePreviewQuery } from './server-runs'

interface RunResumeDialogProps {
  open: boolean
  tenantId: string
  runId: string
  /** The run's frozen graph, for node titles and container membership. */
  definition: WorkflowDefinition
  busy: boolean
  /** The rule v2-server refused the last resume with, when the preview had not foreseen it. */
  refusal?: ResumeRefusal
  /** Called when the preview changed since the refusal, which then no longer holds. */
  onRefusalStale?: () => void
  onOpenChange: (open: boolean) => void
  onResume: () => Promise<void>
}


/**
 * 「从失败处继续」's confirmation, desktop's resume dialog with only its keep-files mode: it reads
 * v2-server's preview each time it opens and lists the nodes the resume starts from, the Loop and
 * Iteration containers that restart from round 1 in their place, and the nodes that run again after
 * them. A refused preview says why and disables the action; the server replans on resume anyway.
 */
export function RunResumeDialog({ open, tenantId, runId, definition, busy, refusal, onRefusalStale, onOpenChange, onResume }: RunResumeDialogProps) {
  const { t } = useTranslation()
  const preview = useQuery({
    ...runResumePreviewQuery(tenantId, runId),
    enabled: open && tenantId !== '',
    // A run that is settling (or was refused as such) becomes resumable without any run change the
    // page would hear of: read the preview again until it says something else.
    refetchInterval: (query) => (query.state.data?.reason === 'run_settling' || refusal !== undefined ? runRefreshMs.interval : false),
  })
  const stale = refusal?.basis !== undefined && preview.data !== undefined && JSON.stringify(preview.data) !== refusal.basis
  useEffect(() => {
    if (stale) onRefusalStale?.()
  }, [stale, onRefusalStale])
  const held = stale ? undefined : refusal
  const resumable = preview.data?.resumable === true && held === undefined
  const title = titleOf(definition)
  return (
    <Dialog open={open} onOpenChange={(next) => !busy && onOpenChange(next)}>
      <DialogContent className="gap-0 overflow-hidden p-0 sm:max-w-lg">
        <DialogHeader className="border-b border-border px-5 py-4">
          <DialogTitle>{t('workflowRun.resume.title')}</DialogTitle>
          <DialogDescription>{t('workflowRun.resume.v2Description')}</DialogDescription>
        </DialogHeader>
        <div className="max-h-[60vh] space-y-3 overflow-y-auto px-5 py-4 text-sm">
          {preview.isPending && open ? (
            <p className="flex items-center gap-1.5 text-muted-foreground">
              <IconLoader2 aria-hidden className="size-3.5 animate-spin" />
              {t('workflowRun.resume.loadingNodes')}
            </p>
          ) : preview.isError ? (
            <p role="alert" className="text-destructive">{t('workflowRun.resume.previewUnavailable')}</p>
          ) : preview.data ? (
            <>
              {held !== undefined && preview.data.resumable && <RefusalNotice refusal={held} title={title} />}
              <ResumePlan preview={preview.data} definition={definition} />
            </>
          ) : null}
          <p className="rounded-lg border border-border bg-muted/40 px-3 py-2 text-xs leading-5 text-muted-foreground">{t('workflowRun.resume.keepFiles')}</p>
        </div>
        <DialogFooter className="mb-0 border-t border-border px-5 py-4">
          <Button type="button" variant="outline" className="min-w-24" disabled={busy} onClick={() => onOpenChange(false)}>
            {t('common.cancel')}
          </Button>
          <Button type="button" className="min-w-28" disabled={busy || !resumable} onClick={() => void onResume()}>
            {busy ? (
              <span className="inline-flex items-center gap-1.5">
                <IconLoader2 className="size-3.5 animate-spin" />
                {t('workflowRun.resume.resuming')}
              </span>
            ) : (
              t('workflowRun.resume.confirm')
            )}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

/** A node's title in the frozen graph, its id when it has none. */
function titleOf(definition: WorkflowDefinition): (id: string) => string {
  const nodes = new Map(definition.nodes.map((node) => [node.id, node]))
  return (id) => nodes.get(id)?.data.title?.trim() || id
}

/** Why the resume is refused, with the nodes that block it when the rule names any. */
function RefusalNotice({ refusal, title }: { refusal: ResumeRefusal; title: (id: string) => string }) {
  const { t } = useTranslation()
  return (
    <p role="alert" className="flex gap-1.5 rounded-lg border border-amber-500/40 bg-amber-500/10 px-3 py-2.5 text-xs leading-5 text-amber-900 dark:text-amber-200">
      <IconAlertTriangle aria-hidden className="mt-0.5 size-3.5 shrink-0" />
      {t(`workflowRun.resume.refused.${refusal.reason}`, { nodes: refusal.runningNodeIds.map(title).join('、') })}
    </p>
  )
}

/** The preview's nodes, by the role each plays in the resume. */
function ResumePlan({ preview, definition }: { preview: WorkflowRunResumePreview; definition: WorkflowDefinition }) {
  const { t } = useTranslation()
  const nodes = new Map(definition.nodes.map((node) => [node.id, node]))
  const title = titleOf(definition)
  if (!preview.resumable) {
    return <RefusalNotice refusal={{ reason: preview.reason ?? 'run_not_ended', runningNodeIds: preview.runningNodeIds }} title={title} />
  }
  const containers = new Set(preview.restartContainerNodeIds)
  const container = (id: string) => {
    const node = nodes.get(id)
    return node?.data.containerId ?? node?.parentId
  }
  const startsFrom = new Set(preview.failedNodes.map((node) => node.nodeId))
  // What the failed nodes and containers do not already name: their descendants. Container
  // members rerun with their container and are not listed again.
  const order = new Map(definition.nodes.map((node, index) => [node.id, index]))
  const after = preview.clearNodeIds
    .filter((id) => !startsFrom.has(id) && !containers.has(id) && !containers.has(container(id) ?? ''))
    .sort((a, b) => (order.get(a) ?? 0) - (order.get(b) ?? 0))
  return (
    <div className="space-y-3">
      <section aria-label={t('workflowRun.resume.startsFrom')}>
        <h4 className="mb-1.5 text-xs font-medium text-muted-foreground">{t('workflowRun.resume.startsFrom')}</h4>
        <ul className="space-y-1">
          {preview.failedNodes.map((node) => (
            <li key={node.nodeRunId} className="flex flex-wrap items-center gap-x-2">
              <span className="font-medium">{title(node.nodeId)}</span>
              <span className={node.status === 'failed' ? 'text-xs text-rose-700 dark:text-rose-300' : 'text-xs text-muted-foreground'}>
                {t(`workflowRun.resume.nodeStatus.${node.status}`)}
              </span>
              {node.containerNodeId !== null && node.containerNodeId !== node.nodeId && (
                <span className="text-xs text-muted-foreground">{t('workflowRun.resume.insideContainer', { name: title(node.containerNodeId) })}</span>
              )}
            </li>
          ))}
        </ul>
      </section>
      {preview.restartContainerNodeIds.length > 0 && (
        <ul className="space-y-1 text-xs text-violet-700 dark:text-violet-300">
          {preview.restartContainerNodeIds.map((id) => (
            <li key={id}>{t('workflowRun.resume.restartsContainer', { name: title(id) })}</li>
          ))}
        </ul>
      )}
      {after.length > 0 && (
        <section aria-label={t('workflowRun.resume.alsoReruns')}>
          <h4 className="mb-1.5 text-xs font-medium text-muted-foreground">{t('workflowRun.resume.alsoReruns')}</h4>
          <p>{after.map(title).join('、')}</p>
        </section>
      )}
    </div>
  )
}
