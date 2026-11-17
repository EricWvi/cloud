import { useMutation, useQueryClient } from '@tanstack/react-query'
import { Button } from '@/components/ui/button'
import { cancelWorkflowRun } from '@/lib/workflows/api/generated'
import { lazy, Suspense } from 'react'
import { ChevronLeft, Workflow } from 'lucide-react'
import type { WorkspacePanelState } from '@/components/workspace-panel/use-panel'
import { formatTime } from './task-model'
import type { TaskWorkflowRun as TaskWorkflowRecord } from './task-workflows-api/generated'

const WorkflowRunView = lazy(() => import('@/features/chat/workflow-run/run-pane'))

const STATUS: Record<string, { label: string; tone: string }> = {
  pending: { label: '待启动', tone: 'pending' },
  running: { label: '运行中', tone: 'running' },
  awaitingInput: { label: '待确认', tone: 'review' },
  succeeded: { label: '已完成', tone: 'done' },
  failed: { label: '失败', tone: 'cancelled' },
  cancelled: { label: '已取消', tone: 'cancelled' },
  interrupted: { label: '已中断', tone: 'cancelled' },
}

/** A durable task workflow association, including inspection sources. */
export type TaskWorkflowRun = TaskWorkflowRecord
const kindLabel = { handle: '处理', decomposition: '拆分', source: '巡检来源', mention: '评论 @' }

function outcome(d: TaskWorkflowRun): string {
  if (d.deleted) return '运行已删除'
  if (d.failure) return d.failure
  if (d.kind === 'decomposition' && d.status === 'succeeded') return `拆出 ${d.createdCount} 个子任务`
  return STATUS[d.status].label
}

function WorkflowRow({ run: d }: { run: TaskWorkflowRun }) {
  const status = STATUS[d.status]
  return <>
    <span className={`mc-status ${status.tone}`}>{status.label}</span>
    <span className="mc-task-main">
      <strong>{d.workflowName}</strong>
      <small className="mc-workflow-meta">{kindLabel[d.kind]} · {d.version} · {d.startedByName} 发起<time dateTime={d.createdAt}>{formatTime(d.createdAt)}</time></small>
    </span>
    <small className="mc-workflow-outcome">{outcome(d)}</small>
  </>
}

/**
 * The workflows run for this task, newest first, and one run opened in the same view the chat
 * page has. The view is read-only here: a run started from it would not belong to the task.
 */
export function TaskWorkflows({ tenantId, runs, selected, workspace, onSelect, onStopped }: {
  tenantId: string
  onStopped: () => Promise<void>
  runs: TaskWorkflowRun[]
  /** The run opened; undefined shows the list. */
  selected: TaskWorkflowRun | undefined
  /** The page's workspace panel, which the run's changed files open. */
  workspace: WorkspacePanelState
  onSelect: (workflowRunId?: string) => void
}) {
  if (selected && !selected.deleted) {
    const back = <button type="button" className="mc-workflow-back" aria-label="返回工作流列表" title="返回工作流列表" onClick={() => onSelect()}><ChevronLeft size={16} aria-hidden="true" /></button>
    return <section className="mc-workflow-run" aria-label="工作流运行">
      <StopHandlingRun tenantId={tenantId} run={selected} onStopped={onStopped} />
      <Suspense fallback={<p role="status" className="mc-run-note">正在读取运行…</p>}>
        <WorkflowRunView runId={selected.workflowRunId} tenantId={tenantId} readOnly workspace={workspace} leading={back} />
      </Suspense>
    </section>
  }
  if (runs.length === 0) {
    return <div className="mc-run-empty"><Workflow size={22} aria-hidden="true" /><p>这个任务还没有运行过工作流。</p></div>
  }
  return <ul className="mc-tasks" aria-label="工作流运行记录">{runs.map(d => <li key={d.id}>
    {d.workflowRunId && !d.deleted
      ? <button type="button" className="mc-task-row" aria-label={`查看工作流运行 ${d.workflowName}，${formatTime(d.createdAt)}`} onClick={() => onSelect(d.workflowRunId)}><WorkflowRow run={d} /></button>
      : <div className="mc-task-row"><WorkflowRow run={d} /></div>}
    <StopHandlingRun tenantId={tenantId} run={d} onStopped={onStopped} />
  </li>)}</ul>
}

/** Every member who can see the task uses the chat run view's existing cancellation endpoint. */
function StopHandlingRun({tenantId,run,onStopped}:{tenantId:string;run:TaskWorkflowRun;onStopped:()=>Promise<void>}){
 const client=useQueryClient()
 const stop=useMutation({
  mutationFn:()=>cancelWorkflowRun(tenantId,run.workflowRunId),
  onSuccess:async()=>{
   // The task history read settles the cancelled link before the task's version is refreshed.
   await client.invalidateQueries({queryKey:['task-workflow-runs',tenantId,run.taskId]})
   await onStopped()
  },
 })
 if(run.deleted || run.kind!=='handle' || !['pending','running','awaitingInput'].includes(run.status))return null
 return <div className="flex justify-end gap-2 px-3 py-2">
  {stop.isError && <p role="alert">停止未确认，请重试。</p>}
  <Button type="button" size="sm" variant="outline" onClick={()=>stop.mutate()} disabled={stop.isPending}>{stop.isPending?'停止中…':'停止'}</Button>
 </div>
}
