import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Bell } from 'lucide-react'
import { useNavigate } from 'react-router'
import { useGetApiV1MeTenants } from '@/api/me/me'
import { Popover, PopoverContent, PopoverTrigger } from '@/components/ui/popover'
import { listNotifications, readNotifications, type Notification, type NotificationList } from '@/lib/tasks-api/generated'
import { cn } from '@/lib/utils'
import { notificationText } from './notification-text'

/** How often the inbox is read again; v2 has no push channel, like the task list. */
const POLL_MS = 15000

const notificationsKey = (tenantId: string) => ['notifications', tenantId] as const

const formatTime = (iso: string): string =>
  new Intl.DateTimeFormat('zh-CN', { dateStyle: 'medium', timeStyle: 'short' }).format(new Date(iso))

/**
 * The bell in the icon rail: the caller's unread count, and a list of task notifications that
 * opens the task and marks the notice read.
 */
export function NotificationBell() {
  const tenants = useGetApiV1MeTenants()
  const tenantId = tenants.data?.items?.[0]?.id ?? ''
  const [open, setOpen] = useState(false)
  const navigate = useNavigate()
  const client = useQueryClient()
  const inbox = useQuery({
    queryKey: notificationsKey(tenantId),
    queryFn: () => listNotifications(tenantId, { limit: 50 }),
    enabled: !!tenantId,
    refetchInterval: POLL_MS,
  })
  const markRead = useMutation({
    mutationFn: (body: { ids?: string[]; all?: boolean }) => readNotifications(tenantId, body),
    onSuccess: ({ unread }, body) => {
      const now = new Date().toISOString()
      client.setQueryData<NotificationList>(notificationsKey(tenantId), prev => prev && {
        unread,
        items: prev.items.map(n => (body.all || body.ids?.includes(n.id)) && !n.readAt ? { ...n, readAt: now } : n),
      })
    },
  })
  if (!tenantId) return null
  const items = inbox.data?.items ?? []
  const unread = inbox.data?.unread ?? 0
  const label = unread > 0 ? `提醒，${unread} 条未读` : '提醒'

  const openTask = (n: Notification) => {
    if (!n.readAt) markRead.mutate({ ids: [n.id] })
    setOpen(false)
    void navigate(`/collaboration/tasks/${n.taskId}`)
  }

  return (
    <Popover open={open} onOpenChange={setOpen}>
      <PopoverTrigger
        aria-label={label}
        title="提醒"
        className="relative flex size-9 items-center justify-center rounded-lg text-muted-foreground transition-colors duration-150 outline-none hover:bg-sidebar-accent hover:text-foreground focus-visible:ring-3 focus-visible:ring-ring/50 data-popup-open:bg-sidebar-accent data-popup-open:text-foreground"
      >
        <Bell className="size-[18px]" />
        {unread > 0 && (
          <span aria-hidden className="absolute top-1 right-1 flex h-4 min-w-4 items-center justify-center rounded-full bg-destructive px-1 text-[10px] leading-none font-semibold text-white">
            {unread > 99 ? '99+' : unread}
          </span>
        )}
      </PopoverTrigger>
      <PopoverContent side="right" align="end" className="w-80 gap-0 p-0">
        <header className="flex items-center justify-between border-b px-3 py-2">
          <h2 className="text-sm font-semibold">提醒</h2>
          <button
            type="button"
            disabled={unread === 0 || markRead.isPending}
            onClick={() => markRead.mutate({ all: true })}
            className="text-xs text-muted-foreground hover:text-foreground disabled:opacity-40"
          >
            全部已读
          </button>
        </header>
        {items.length === 0
          ? <p className="px-3 py-8 text-center text-xs text-muted-foreground">{inbox.isError ? '提醒加载失败' : '暂无提醒'}</p>
          : (
              <ul aria-label="提醒列表" className="max-h-96 overflow-y-auto py-1">
                {items.map(n => (
                  <li key={n.id}>
                    <button
                      type="button"
                      onClick={() => openTask(n)}
                      className={cn('flex w-full gap-2 px-3 py-2 text-left hover:bg-accent', n.readAt && 'text-muted-foreground')}
                    >
                      <span aria-label={n.readAt ? undefined : '未读'} className={cn('mt-1.5 size-1.5 shrink-0 rounded-full', !n.readAt && 'bg-brand')} />
                      <span className="flex min-w-0 flex-col gap-0.5">
                        <span className={cn('text-xs', !n.readAt && 'font-medium text-foreground')}>{notificationText(n)}</span>
                        <span className="truncate text-sm">{n.taskTitle}</span>
                        {n.excerpt && <span className="truncate text-xs text-muted-foreground">{n.excerpt}</span>}
                        <time dateTime={n.createdAt} className="text-[11px] text-muted-foreground">{formatTime(n.createdAt)}</time>
                      </span>
                    </button>
                  </li>
                ))}
              </ul>
            )}
      </PopoverContent>
    </Popover>
  )
}
