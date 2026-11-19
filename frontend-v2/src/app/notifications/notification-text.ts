import type { Notification } from '@/lib/tasks-api/generated'

/** One line saying who did what to which task, in the caller's terms. */
export function notificationText(n: Notification): string {
  const who = n.actorName ?? 'Agent'
  switch (n.kind) {
    case 'assigned':
      return `${who} 把任务指派给了你`
    case 'mentioned':
      return `${who} 在任务里 @ 了你`
    case 'review':
      return n.actorName ? `${n.actorName} 把你创建的任务提交待验收` : '你创建的任务已进入待验收'
  }
}
