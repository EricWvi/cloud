import { useState } from 'react'
import { useInfiniteQuery, useMutation, useQuery, useQueryClient, type InfiniteData, type QueryClient } from '@tanstack/react-query'
import { isAxiosError } from 'axios'
import { Bell } from 'lucide-react'
import { useNavigate } from 'react-router'
import { useGetApiV1MeTenants } from '@/api/me/me'
import { Popover, PopoverContent, PopoverTrigger } from '@/components/ui/popover'
import { listNotifications, readNotifications, type Notification, type NotificationList } from '@/lib/tasks-api/generated'
import { cn } from '@/lib/utils'
import { notificationText } from './notification-text'

/** How often the inbox is read again; v2 has no push channel, like the task list. */
const POLL_MS = 15000
/** Notifications per page; older ones load on demand, so every unread one can be reached. */
const PAGE_SIZE = 30

/**
 * Two queries, so the badge's poll and the history a person pages through never share a cache:
 * the newest page, polled for the badge and the list's top; and older pages, read only while the
 * list is open and the person asked for them, never polled, dropped when the list closes.
 */
const newestKey = (tenantId: string) => ['notifications', tenantId, 'newest'] as const
const olderKey = (tenantId: string) => ['notifications', tenantId, 'older'] as const

/** One page of the inbox, or the record that the server refused it. */
type InboxPage = NotificationList & { denied?: true }
type Older = InfiniteData<InboxPage, string>

const deniedPage: InboxPage = { items: [], unread: 0, nextCursor: '', denied: true }

/**
 * The server no longer lets the caller read this workspace's inbox (removed from the team). The
 * refusal becomes the newest page's cached data instead of an error, whichever page met it, so it
 * replaces what was read before and a later failure (503, network) cannot bring it back; only a
 * successful read of the newest page replaces it.
 */
const lostAccess = (error: unknown) =>
  isAxiosError(error) && (error.response?.status === 403 || error.response?.status === 404)

async function readPage(tenantId: string, cursor: string, signal: AbortSignal): Promise<InboxPage> {
  try {
    return await listNotifications(tenantId, cursor ? { limit: PAGE_SIZE, cursor } : { limit: PAGE_SIZE }, undefined, signal)
  } catch (error) {
    if (lostAccess(error)) return deniedPage
    throw error
  }
}

/** An older page; a refusal there hides the whole inbox, recorded where the badge reads it. */
async function readOlder(client: QueryClient, tenantId: string, cursor: string, signal: AbortSignal): Promise<InboxPage> {
  const page = await readPage(tenantId, cursor, signal)
  if (page.denied) {
    // A newest-page read already in flight may have been admitted before the removal; its late
    // answer must not overwrite the refusal. Only a read started after it can show the bell again.
    await client.cancelQueries({ queryKey: newestKey(tenantId) })
    client.setQueryData(newestKey(tenantId), deniedPage)
    void client.cancelQueries({ queryKey: olderKey(tenantId) })
  }
  return page
}

const formatTime = (iso: string): string =>
  new Intl.DateTimeFormat('zh-CN', { dateStyle: 'medium', timeStyle: 'short' }).format(new Date(iso))

/**
 * The bell in the icon rail: the caller's unread count, and a list of task notifications, newest
 * first and paged, that opens the task and marks the notice read. It disappears, cache and all,
 * once the server says the caller lost access to the workspace.
 */
export function NotificationBell() {
  const tenants = useGetApiV1MeTenants()
  const tenantId = tenants.data?.items?.[0]?.id ?? ''
  const newest = useQuery({
    queryKey: newestKey(tenantId),
    queryFn: ({ signal }) => readPage(tenantId, '', signal),
    enabled: !!tenantId,
    refetchInterval: POLL_MS,
  })
  // The newest page keeps polling here, so the bell comes back if access does. The list below is
  // unmounted meanwhile: whether it was open and what it had paged through are gone with it.
  if (!tenantId || !newest.data || newest.data.denied) return null
  return <NotificationPanel tenantId={tenantId} newest={newest.data} newestFailed={newest.isError} />
}

function NotificationPanel({ tenantId, newest, newestFailed }: { tenantId: string; newest: InboxPage; newestFailed: boolean }) {
  const [open, setOpen] = useState(false)
  const navigate = useNavigate()
  const client = useQueryClient()
  // Browsing starts with the first 加载更多 and ends when the list closes. It freezes the newest
  // page as it was then and pages on from where it ended, so notifications arriving meanwhile do
  // not shift the history out from under the person; the badge keeps updating.
  const [browse, setBrowse] = useState<{ items: Notification[]; from: string } | null>(null)
  const browsing = browse !== null
  const from = browse?.from ?? ''
  const older = useInfiniteQuery({
    queryKey: [...olderKey(tenantId), from],
    queryFn: ({ pageParam, signal }) => readOlder(client, tenantId, pageParam, signal),
    initialPageParam: from,
    getNextPageParam: last => last.nextCursor || undefined,
    enabled: open && browsing && !!from,
    staleTime: Infinity,
    refetchOnWindowFocus: false,
    // The person retries from the list; a failed page must not look like the end.
    retry: 1,
  })
  // Opening or closing the list starts the history over. A page still in flight would otherwise
  // land after the list closed: cancel it and drop the history, so nothing but the newest page is
  // read until the person pages again.
  const setListOpen = (next: boolean) => {
    setOpen(next)
    setBrowse(null)
    void client.cancelQueries({ queryKey: olderKey(tenantId) })
    client.removeQueries({ queryKey: olderKey(tenantId) })
  }
  const markRead = useMutation({
    mutationFn: (body: { ids?: string[]; all?: boolean }) => readNotifications(tenantId, body),
    onSuccess: ({ unread }, body) => {
      const now = new Date().toISOString()
      const mark = (page: InboxPage): InboxPage => ({
        ...page,
        items: page.items.map(n => (body.all || body.ids?.includes(n.id)) && !n.readAt ? { ...n, readAt: now } : n),
      })
      client.setQueryData<InboxPage>(newestKey(tenantId), prev => prev && { ...mark(prev), unread })
      client.setQueriesData<Older>({ queryKey: olderKey(tenantId) }, prev => prev && { ...prev, pages: prev.pages.map(mark) })
      setBrowse(prev => prev && { ...prev, items: mark({ items: prev.items, unread: 0, nextCursor: '' }).items })
    },
  })
  const items = browse
    ? [...browse.items, ...(older.data?.pages ?? []).flatMap(page => page.items)]
    : newest.items
  const unread = newest.unread
  // Reached the end only when the last page read says so; a failed read keeps the way to retry.
  const loadFailed = browsing && older.isError
  const hasMore = browsing ? older.hasNextPage || older.isPending || loadFailed : !!newest.nextCursor
  const loadingMore = browsing && (older.isFetching)
  const loadMore = () => {
    if (!browse) {
      if (newest.nextCursor) setBrowse({ items: newest.items, from: newest.nextCursor })
    } else if (!older.data) void older.refetch()
    else void older.fetchNextPage()
  }
  const label = unread > 0 ? `提醒，${unread} 条未读` : '提醒'

  const openTask = (n: Notification) => {
    if (!n.readAt) markRead.mutate({ ids: [n.id] })
    setListOpen(false)
    void navigate(`/collaboration/tasks/${n.taskId}`)
  }

  return (
    <Popover open={open} onOpenChange={setListOpen}>
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
          ? <p className="px-3 py-8 text-center text-xs text-muted-foreground">{newestFailed ? '提醒加载失败' : '暂无提醒'}</p>
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
                {hasMore && (
                  <li className="px-3 py-1.5">
                    <button
                      type="button"
                      disabled={loadingMore}
                      onClick={loadMore}
                      className="w-full rounded-md py-1 text-xs text-muted-foreground hover:bg-accent hover:text-foreground disabled:opacity-50"
                    >
                      {loadingMore ? '加载中…' : loadFailed ? '加载失败，重试' : '加载更多'}
                    </button>
                  </li>
                )}
              </ul>
            )}
      </PopoverContent>
    </Popover>
  )
}
