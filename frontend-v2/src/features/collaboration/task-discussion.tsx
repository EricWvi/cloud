import { AtSign, Bot, ExternalLink, MessageSquare, Play, Workflow } from 'lucide-react'
import { Link } from 'react-router'
import { WORKFLOW_STATUS_META, formatWorkflowOutput } from './comment-workflow'
import { formatTime, runEndLabel } from './task-model'
import type { TaskComment, TaskCommentWorkflowRun, TaskRun } from './tasks-api/generated'

type Entry = { at: string; comment: TaskComment } | { at: string; run: TaskRun; index: number }

function CommentWorkflowCard({ taskId, run }: { taskId: string; run: TaskCommentWorkflowRun }) {
  const status = WORKFLOW_STATUS_META[run.status] ?? { label: run.status, tone: 'pending' }
  const output = !run.deleted && run.output ? formatWorkflowOutput(run.output) : ''
  return <section className="mc-comment-workflow" aria-label={`工作流运行：${run.workflowName}`}>
    <div className="mc-comment-workflow-head">
      <Workflow size={13} aria-hidden="true" />
      <strong className="mc-comment-workflow-name">{run.workflowName}</strong>
      {run.version && <small className="mc-comment-workflow-version">{run.version}</small>}
      <span className={`mc-status ${status.tone}`}>{status.label}</span>
      {run.deleted
        ? <small className="mc-comment-workflow-deleted">运行已删除</small>
        : <Link className="mc-link mc-comment-workflow-link" to={`/collaboration/tasks/${taskId}?tab=workflows&workflow=${run.id}`}>查看运行<ExternalLink size={11} aria-hidden="true" /></Link>}
    </div>
    {!run.deleted && run.status === 'succeeded' && (
      output
        ? <pre className="mc-comment-workflow-output" aria-label="工作流输出">{output}</pre>
        : <p className="mc-comment-workflow-note">已完成，无输出内容。</p>
    )}
    {!run.deleted && (run.status === 'failed' || run.status === 'cancelled' || run.status === 'interrupted') && run.error && (
      <p className="mc-comment-workflow-error" role="status">{run.error}</p>
    )}
    {!run.deleted && (run.status === 'pending' || run.status === 'running') && (
      <p className="mc-comment-workflow-note">工作流正在运行，完成后将在此显示结果…</p>
    )}
    {!run.deleted && run.status === 'awaitingInput' && (
      <p className="mc-comment-workflow-note">工作流等待人工确认后继续。</p>
    )}
  </section>
}

/**
 * The team's side of the task in time order: comments, and when each run started. A comment sent
 * to the Agent also appears in that run's thread as its prompt; here it says so.
 */
export function TaskDiscussion({ taskId, comments, runs, onRun }: { taskId: string; comments: TaskComment[]; runs: TaskRun[]; onRun: (id: string) => void }) {
  const entries: Entry[] = [
    ...comments.map(comment => ({ at: comment.createdAt, comment })),
    ...runs.map((run, index) => ({ at: run.createdAt, run, index })),
  ].sort((a, b) => a.at.localeCompare(b.at))
  if (entries.length === 0) {
    return <div className="mc-run-empty"><MessageSquare size={22} aria-hidden="true" /><p>还没有讨论。评论全团队可见，@ 一个成员提醒他来看。</p></div>
  }
  const runIndex = new Map(runs.map((run, i) => [run.id, i + 1]))
  return <ol className="mc-discussion" aria-label="讨论">
    {entries.map(entry => 'comment' in entry
      ? <li key={entry.comment.id} className="mc-comment">
        <header><b>{entry.comment.authorName}</b><time>{formatTime(entry.comment.createdAt)}</time>
          {entry.comment.runId && <button type="button" className="mc-tag agent" onClick={() => onRun(entry.comment.runId)}><Bot size={11} />发给 Agent · 运行 {runIndex.get(entry.comment.runId)}</button>}
          {entry.comment.workflowRun && <span className="mc-tag workflow"><Workflow size={11} />发起了工作流</span>}
        </header>
        <p>{entry.comment.body}</p>
        {entry.comment.workflowRun && <CommentWorkflowCard taskId={taskId} run={entry.comment.workflowRun} />}
        {entry.comment.mentions.length > 0 && <div className="mc-chips">{entry.comment.mentions.map(m => <span key={`${m.type}:${m.id}`} className={`mc-chip ${m.type}`}>{m.type === 'agent' ? <Bot size={11} /> : m.type === 'workflow' ? <Workflow size={11} /> : <AtSign size={11} />}{m.displayName}</span>)}</div>}
      </li>
      : <li key={entry.run.id} className="mc-event">
        <Play size={12} aria-hidden="true" />
        <span><b>{entry.run.startedByName}</b> 用 {entry.run.executorName} 发起了运行 {entry.index + 1}{entry.run.state === 'ended' && `，${runEndLabel(entry.run.endReason)}`}</span>
        <button type="button" className="mc-link" onClick={() => onRun(entry.run.id)}>查看过程</button>
        <time>{formatTime(entry.run.createdAt)}</time>
      </li>)}
  </ol>
}
