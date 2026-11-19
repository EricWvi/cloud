import { useEffect, useState } from 'react'
import { savePatch } from '@/components/workspace-panel/delivery'
import { codeOf } from '@/lib/api-client'
import { Button } from '@/components/ui/button'
import { Checkbox } from '@/components/ui/checkbox'
import { Dialog, DialogClose, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { Label } from '@/components/ui/label'
import type { ChatDeletionPreview } from '../api/generated'
import { type Session, useChatStore } from '../store'

const undeliveredReasons: Record<string, string> = {
  legacy: '这个会话早于交付功能，改动没有交付记录。',
  delivering: '这一轮还在进行或交付中，改动还没有保存进交付。',
  last_turn_undelivered: '最后一轮的改动没有成功交付。',
  git_state: '仓库里有交付带不走的内容（stash、未检出分支上的提交或嵌套仓库）。',
  changed_after_delivery: '最后一次交付之后，工作目录里又有了改动。',
  unverifiable: '无法确认工作目录里的改动是否都已交付。',
}

const shownChanges = 20

/** What deleting does to the conversation's working directory, as the server judged it. */
function WorkspaceNote({ preview, discard, setDiscard }: { preview: ChatDeletionPreview; discard: boolean; setDiscard: (v: boolean) => void }) {
  if (preview.workspace === 'none') {
    return preview.reason === 'workspace_missing' ? <p className="text-sm text-muted-foreground">这个会话的工作目录已不存在。</p> : null
  }
  if (preview.workspace === 'recoverable') {
    return (
      <div className="grid gap-1 text-sm text-muted-foreground">
        <p>工作目录会一起删除。删除后你无法再取回它的改动，只有运维能从交付恢复；需要的话先下载改动。</p>
        {preview.ignored && <p>被忽略规则（.gitignore 与 .git/info/exclude）排除的文件不在交付里，会一起删除。</p>}
      </div>
    )
  }
  const delivering = preview.reason === 'delivering'
  return (
    <div className="grid gap-2 text-sm">
      <p className="text-muted-foreground">
        {undeliveredReasons[preview.reason ?? ''] ?? '工作目录里的改动没有交付。'}默认保留工作目录。
        {delivering && '请等这一轮结束后再决定是否丢弃。'}
      </p>
      {preview.undeliveredChanges.length > 0 && (
        <div className="grid gap-1">
          <p className="text-muted-foreground">未交付的改动（共 {preview.undeliveredTotal} 个文件）：</p>
          <ul aria-label="未交付的改动" className="max-h-32 overflow-auto rounded-md border px-2 py-1 font-mono text-xs">
            {preview.undeliveredChanges.slice(0, shownChanges).map((c) => (
              <li key={c.path}>
                {c.status === 'added' ? '+' : c.status === 'deleted' ? '−' : '~'} {c.path}
              </li>
            ))}
            {preview.undeliveredTotal > shownChanges && <li className="text-muted-foreground">……另有 {preview.undeliveredTotal - shownChanges} 个</li>}
          </ul>
        </div>
      )}
      {preview.git && (
        <div className="grid gap-1">
          <p className="text-muted-foreground">只在仓库里、不在任何交付里的内容：</p>
          <ul aria-label="仓库里未交付的内容" className="max-h-32 overflow-auto rounded-md border px-2 py-1 font-mono text-xs">
            {preview.git.stashes > 0 && <li>stash：{preview.git.stashes} 条</li>}
            {preview.git.refs.map((r) => (
              <li key={r.name}>
                {r.name}：{r.notCommit ? '不指向提交' : `${r.commits} 个提交`}
              </li>
            ))}
            {preview.git.repositories.map((p) => (
              <li key={p}>嵌套仓库：{p}</li>
            ))}
          </ul>
        </div>
      )}
      {!delivering && !preview.discardable && (
        <p className="text-muted-foreground">嵌套仓库太大，无法确认丢弃的是哪些内容，不能在这里丢弃；请先自行处理。现在删除会保留工作目录。</p>
      )}
      {!delivering && preview.discardable && (
        <div className="flex items-start gap-2">
          <Checkbox id="discard-undelivered" checked={discard} onCheckedChange={(checked) => setDiscard(checked === true)} className="mt-0.5" />
          <Label htmlFor="discard-undelivered" className="grid cursor-pointer gap-0.5 font-normal">
            同时丢弃未交付的改动
            <span className="text-xs text-muted-foreground">工作目录连同还没交付的改动一起删除，删除后无法找回。</span>
          </Label>
        </div>
      )}
    </div>
  )
}

/**
 * Confirms deleting a conversation. While open it asks the server what happens to the working
 * directory: kept when its changes are not in a delivery, unless the person chooses to discard them.
 */
export function DeleteSessionDialog({ session, open, onOpenChange }: { session: Session; open: boolean; onOpenChange: (open: boolean) => void }) {
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent>{open && <DeleteSessionConfirm session={session} close={() => onOpenChange(false)} />}</DialogContent>
    </Dialog>
  )
}

/** The preview, or why there is none: still asking, unreadable (retry), or a server that cannot judge. */
type Preview = ChatDeletionPreview | 'loading' | 'unknown' | 'unavailable'

/** The dialog's content; it mounts each time the dialog opens, so every opening asks afresh. */
function DeleteSessionConfirm({ session, close }: { session: Session; close: () => void }) {
  const { deleteSession, deletionPreview, deliveryPatch } = useChatStore.getState()
  const [preview, setPreview] = useState<Preview>('loading')
  const [asked, setAsked] = useState(0)
  const [discard, setDiscard] = useState(false)
  const [stale, setStale] = useState(false)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string>()
  useEffect(() => {
    let live = true
    deletionPreview(session.id).then(
      (p) => live && setPreview(p),
      (error) => live && setPreview(codeOf(error) === 'workspace_reclaim_unavailable' ? 'unavailable' : 'unknown'),
    )
    return () => {
      live = false
    }
  }, [session.id, deletionPreview, asked])
  const known = typeof preview === 'object' ? preview : undefined
  const discarding = known?.workspace === 'undelivered' && known.reason !== 'delivering' && known.discardable && discard
  const download = async (deliveryId: string) => {
    setError(undefined)
    try {
      savePatch(await deliveryPatch(session.id, deliveryId), deliveryId)
    } catch {
      setError('下载失败，请重试。')
    }
  }
  const confirm = async () => {
    setBusy(true)
    const result = await deleteSession(session.id, discarding ? known.revision : undefined)
    setBusy(false)
    if (result === 'stale') {
      // Something changed since the preview: show the new one and let the person decide again.
      setStale(true)
      setDiscard(false)
      setPreview('loading')
      setAsked((n) => n + 1)
      return
    }
    close()
  }
  return (
    <>
      <DialogHeader>
        <DialogTitle>删除"{session.title}"？</DialogTitle>
        <DialogDescription>会话和它的聊天记录都会删除，无法恢复。</DialogDescription>
      </DialogHeader>
      {stale && <p className="text-sm text-destructive">工作目录在你确认前有了变化，请看过下面的情况后重新确认。</p>}
      {preview === 'loading' && <p className="text-sm text-muted-foreground">正在检查工作目录…</p>}
      {preview === 'unknown' && (
        <div className="flex items-center gap-2 text-sm text-muted-foreground">
          <p>暂时读不到工作目录的情况，读到之前不能删除。</p>
          <Button variant="outline" size="sm" onClick={() => (setPreview('loading'), setAsked((n) => n + 1))}>
            重试
          </Button>
        </div>
      )}
      {preview === 'unavailable' && (
        <p className="text-sm text-muted-foreground">服务器现在无法检查工作目录：删除后先保留，等能检查时再按交付情况决定是否回收。</p>
      )}
      {known && <WorkspaceNote preview={known} discard={discard} setDiscard={setDiscard} />}
      {known?.latestDelivery && known.workspace !== 'none' && (
        <div>
          <Button variant="outline" size="sm" onClick={() => void download(known.latestDelivery!)}>
            下载改动（patch）
          </Button>
        </div>
      )}
      {error && <p className="text-sm text-destructive">{error}</p>}
      <DialogFooter>
        <DialogClose render={<Button variant="outline" />}>取消</DialogClose>
        <Button variant="destructive" disabled={busy || preview === 'loading' || preview === 'unknown'} onClick={() => void confirm()}>
          删除
        </Button>
      </DialogFooter>
    </>
  )
}
