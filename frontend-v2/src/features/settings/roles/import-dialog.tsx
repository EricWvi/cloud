import { useId, useState } from 'react'
import { FileText, Upload, X } from 'lucide-react'
import { Button } from '@/components/ui/button'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import type { ImportRoleInput, Role } from '@/lib/roles-api/generated'
import { ActionError } from '../components/page-parts'
import { isRoleNameConflict, roleError } from './use-roles'

const MAX_FILE_BYTES = 1 << 20

/**
 * Imports one role Markdown file. The server owns front-matter validation and name conflicts; the
 * dialog only asks for an explicit overwrite after the first attempt returns a conflict.
 */
export function RoleImportDialog({
  onClose,
  onImport,
  onImported,
}: {
  onClose: () => void
  pending: boolean
  error: unknown
  onImport: (input: ImportRoleInput) => Promise<Role>
  onImported: (name: string, overwritten: boolean) => void
}) {
  const inputId = useId()
  const [file, setFile] = useState<File>()
  const [content, setContent] = useState('')
  const [conflict, setConflict] = useState(false)
  const [pending, setPending] = useState(false)
  const [error, setError] = useState<unknown>()

  const choose = async (next?: File) => {
    setError(undefined)
    setConflict(false)
    if (!next) return
    if (!/\.(md|markdown)$/i.test(next.name)) {
      setError(new Error('请选择 .md 格式的角色文件。'))
      return
    }
    if (next.size > MAX_FILE_BYTES) {
      setError(new Error('角色文件不能超过 1 MiB。'))
      return
    }
    try {
      const text = await next.text()
      setFile(next)
      setContent(text)
    } catch {
      setError(new Error('无法读取该文件，请重新选择。'))
    }
  }

  const submit = async (overwrite: boolean) => {
    if (!file || !content) return
    setPending(true)
    setError(undefined)
    try {
      const imported = await onImport({
        fileName: file.name,
        content,
        overwrite,
      })
      onImported(imported.name, overwrite)
    } catch (cause) {
      if (!overwrite && isRoleNameConflict(cause)) setConflict(true)
      else setError(roleError(cause))
    } finally {
      setPending(false)
    }
  }

  return (
    <Dialog
      open
      onOpenChange={(open) => {
        if (!open && !pending) onClose()
      }}
    >
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>导入角色</DialogTitle>
          <DialogDescription>
            选择一个带 YAML front matter 的角色 Markdown 文件。导入后，工作区成员都能在工作流里选择它。
          </DialogDescription>
        </DialogHeader>
        <div className="flex flex-col gap-4">
          {file ? (
            <div className="flex items-start gap-3 rounded-lg border p-4">
              <FileText className="mt-0.5 size-5 shrink-0 text-muted-foreground" />
              <div className="min-w-0 flex-1">
                <p className="truncate text-sm font-medium">{file.name}</p>
                <p className="mt-1 text-xs text-muted-foreground">
                  {Math.max(1, Math.ceil(file.size / 1024))} KiB · Markdown
                </p>
              </div>
              <Button
                type="button"
                variant="ghost"
                size="icon-sm"
                aria-label="重新选择文件"
                disabled={pending}
                onClick={() => {
                  setFile(undefined)
                  setContent('')
                  setConflict(false)
                  setError(undefined)
                }}
              >
                <X />
              </Button>
            </div>
          ) : (
            <label
              htmlFor={inputId}
              className="flex min-h-40 cursor-pointer flex-col items-center justify-center gap-3 rounded-xl border border-dashed p-6 text-center transition-colors hover:bg-muted/40 focus-within:ring-2 focus-within:ring-ring"
            >
              <span className="flex size-10 items-center justify-center rounded-full bg-muted">
                <Upload className="size-5" />
              </span>
              <span>
                <span className="block text-sm font-medium">选择角色 Markdown 文件</span>
                <span className="mt-1 block text-xs text-muted-foreground">
                  文件最大 1 MiB，名称与描述写在 YAML front matter 中
                </span>
              </span>
            </label>
          )}
          <input
            id={inputId}
            aria-label="角色 Markdown 文件"
            className="sr-only"
            type="file"
            accept=".md,.markdown,text/markdown"
            disabled={pending}
            onChange={(event) => {
              void choose(event.currentTarget.files?.item(0) ?? undefined)
              event.currentTarget.value = ''
            }}
          />
          {conflict && (
            <div
              role="alert"
              className="rounded-lg border border-warning/30 bg-warning/10 p-3 text-sm"
            >
              <p className="font-medium">已有同名角色</p>
              <p className="mt-1 text-muted-foreground">
                覆盖会用这份文件更新现有角色，并保留它的角色 ID。
              </p>
            </div>
          )}
          <ActionError error={error} />
        </div>
        <DialogFooter>
          <Button type="button" variant="ghost" disabled={pending} onClick={onClose}>
            取消
          </Button>
          {conflict ? (
            <Button type="button" disabled={pending} onClick={() => void submit(true)}>
              {pending ? '导入中…' : '覆盖并导入'}
            </Button>
          ) : (
            <Button
              type="button"
              disabled={!file || pending}
              onClick={() => void submit(false)}
            >
              {pending ? '导入中…' : '导入'}
            </Button>
          )}
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
