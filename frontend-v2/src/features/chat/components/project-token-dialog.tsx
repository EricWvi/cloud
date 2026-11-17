import { useState } from 'react'
import { KeyRoundIcon, ShieldCheckIcon, Trash2Icon } from 'lucide-react'
import { useRole, useUser } from '@/app/auth/user'
import { Button } from '@/components/ui/button'
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { useChatStore, type Project } from '../store'

const tokenTime = new Intl.DateTimeFormat('zh-CN', { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' })

/** Whether the signed-in user may manage the project's token: a team owner or the project creator. */
function canManageToken(project: Project, userId: string, role: ReturnType<typeof useRole>): boolean {
  return role === 'owner' || project.ownerUserId === userId
}

/**
 * The project's private-repository token, where it can be replaced or cleared. The token itself is
 * never shown or echoed: the dialog only reports whether one is configured and when it last
 * changed, and the input is a password that clears on success. Members see the status but neither
 * action, which only the team owner or the project creator may take.
 */
export function ProjectTokenDialog({
  project,
  open,
  onOpenChange,
}: {
  project: Project
  open: boolean
  onOpenChange: (open: boolean) => void
}) {
  const setRepositoryToken = useChatStore((s) => s.setRepositoryToken)
  const clearRepositoryToken = useChatStore((s) => s.clearRepositoryToken)
  const user = useUser()
  const role = useRole()
  const manage = canManageToken(project, user.id, role)
  const [token, setToken] = useState('')
  const [busy, setBusy] = useState(false)
  const [confirmClear, setConfirmClear] = useState(false)
  const [error, setError] = useState<string>()

  const replace = async () => {
    if (!token) return
    setBusy(true)
    const failed = await setRepositoryToken(project.id, token)
    setBusy(false)
    if (failed) setError(failed)
    else {
      setToken('')
      setConfirmClear(false)
      setError(undefined)
    }
  }
  const clear = async () => {
    setBusy(true)
    const failed = await clearRepositoryToken(project.id)
    setBusy(false)
    if (failed) setError(failed)
    else {
      setConfirmClear(false)
      setError(undefined)
    }
  }

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-md" aria-describedby={undefined}>
        <DialogHeader>
          <DialogTitle>项目设置</DialogTitle>
          <DialogDescription className="flex items-center gap-1.5">
            {project.repositoryTokenConfigured ? (
              <>
                <ShieldCheckIcon className="size-3.5" aria-hidden="true" />
                已配置令牌 · {project.repositoryTokenSetAt ? tokenTime.format(new Date(project.repositoryTokenSetAt)) : '—'}
              </>
            ) : (
              '未配置令牌'
            )}
          </DialogDescription>
        </DialogHeader>
        {manage && (
          <div className="flex flex-col gap-4">
            <div className="flex flex-col gap-2">
              <Label htmlFor="project-token">仓库令牌（私有仓库选填）</Label>
              <Input
                id="project-token"
                type="password"
                autoComplete="new-password"
                value={token}
                onChange={(e) => setToken(e.target.value)}
                placeholder="输入新的仓库令牌"
              />
              <p className="text-xs text-muted-foreground">令牌只用于拉取私有仓库，不会显示或回显；设置后立即生效。</p>
            </div>
            {error && <p role="alert" className="text-sm text-destructive">{error}</p>}
            <DialogFooter className="justify-between gap-2 sm:justify-between">
              <div className="flex items-center gap-2">
                {project.repositoryTokenConfigured && (
                  <Button variant="outline" size="sm" disabled={busy} onClick={() => (confirmClear ? void clear() : setConfirmClear(true))}>
                    <Trash2Icon className="size-3.5" aria-hidden="true" />
                    {confirmClear ? '确认清除？' : '清除令牌'}
                  </Button>
                )}
              </div>
              <Button size="sm" disabled={busy || !token} onClick={() => void replace()}>
                <KeyRoundIcon className="size-3.5" aria-hidden="true" />
                设置令牌
              </Button>
            </DialogFooter>
          </div>
        )}
      </DialogContent>
    </Dialog>
  )
}
