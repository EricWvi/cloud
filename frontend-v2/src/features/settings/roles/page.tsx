import { useState } from 'react'
import { ArrowUpRight, Bot, Plus, Upload } from 'lucide-react'
import { toast } from 'sonner'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Separator } from '@/components/ui/separator'
import type { Role, RoleInput } from '@/lib/roles-api/generated'
import { ConfirmDialog } from '../components/confirm-dialog'
import { DetailPanel } from '../components/detail-panel'
import { MarkdownPreview } from '../components/markdown-preview'
import {
  ActionError,
  DataState,
  NoResults,
  PageHeader,
  SearchInput,
} from '../components/page-parts'
import { RoleForm } from './role-form'
import { RoleImportDialog } from './import-dialog'
import { useWorkspaceRoles } from './use-roles'

type Panel = { kind: 'create' } | { kind: 'detail' | 'edit'; id: string } | null

export default function RolesPage() {
  const local = useWorkspaceRoles()
  const [search, setSearch] = useState('')
  const [panel, setPanel] = useState<Panel>(null)
  const [importing, setImporting] = useState(false)
  const [removing, setRemoving] = useState<Role>()
  const roles = local.roles
  const filtered = roles.filter((role) =>
    `${role.name} ${role.description} ${role.content}`
      .toLowerCase()
      .includes(search.trim().toLowerCase()),
  )
  const selected =
    panel && 'id' in panel ? roles.find((role) => role.id === panel.id) : undefined
  const open = (next: Panel) => {
    local.resetErrors()
    setPanel(next)
  }
  const create = () => open({ kind: 'create' })
  const save = async (input: RoleInput, id?: string) => {
    if (!local.tenantId) return
    local.resetErrors()
    try {
      if (id) await local.update({ tid: local.tenantId, rid: id, data: input })
      else await local.create({ tid: local.tenantId, data: input })
      toast.success(id ? `已更新角色 ${input.name}` : `已创建角色 ${input.name}`)
      if (id) setPanel({ kind: 'detail', id })
      else setPanel(null)
    } catch {
      // The form renders the localized mutation error.
    }
  }
  return (
    <section>
      <PageHeader
        title="角色"
        description="管理工作区的角色目录。工作流 Agent 节点按名称引用角色，运行时会读取这里的内容并注入提示词。"
        count={roles.length}
        actions={
          <>
            <Button
              variant="outline"
              disabled={!local.tenantId || local.loading}
              onClick={() => {
                local.resetErrors()
                setImporting(true)
              }}
            >
              <Upload data-icon="inline-start" />
              导入角色
            </Button>
            <Button
              disabled={!local.tenantId || local.loading}
              onClick={create}
            >
              <Plus data-icon="inline-start" />
              新建角色
            </Button>
          </>
        }
      />
      <DataState
        pending={local.loading}
        error={local.error}
        retry={() => local.refetch()}
      >
        <div className="settings-toolbar">
          <SearchInput label="搜索角色" value={search} onChange={setSearch} />
        </div>
        <ActionError error={local.mutationError} />
        <div className="settings-resource-list">
          {filtered.map((role) => (
            <article
              key={role.id}
              aria-label={role.name}
              className="settings-resource-row"
            >
              <button
                type="button"
                className="settings-resource-main"
                aria-label={`查看角色 ${role.name}`}
                onClick={() => open({ kind: 'detail', id: role.id })}
              >
                <span className="settings-resource-icon">
                  <Bot className="size-5" />
                </span>
                <span className="min-w-0">
                  <span className="settings-resource-title">{role.name}</span>
                  <span className="settings-resource-description block">
                    {role.description || '暂无描述'}
                  </span>
                  <span className="settings-resource-meta">
                    <span>工作区角色 · 服务端</span>
                    <span aria-hidden="true">·</span>
                    <span>local</span>
                  </span>
                </span>
              </button>
              <div className="settings-resource-actions">
                <Badge variant="secondary">可用</Badge>
                <Button
                  variant="ghost"
                  size="sm"
                  onClick={() => open({ kind: 'detail', id: role.id })}
                >
                  查看详情
                  <ArrowUpRight data-icon="inline-end" />
                </Button>
              </div>
            </article>
          ))}
        </div>
        {!filtered.length && (
          <NoResults filtered={roles.length > 0} onClear={() => setSearch('')}>
            <Button onClick={create}>新建角色</Button>
          </NoResults>
        )}
        <div className="settings-list-footer">
          <span>
            显示 {filtered.length} / {roles.length} 个角色
          </span>
          <span>工作流按角色标题引用；修改标题不会改写已有图</span>
        </div>
      </DataState>
      {panel?.kind === 'create' && (
        <DetailPanel
          title="新建角色"
          description="写清楚这个角色负责什么，以及它应该怎样工作。"
          onClose={() => setPanel(null)}
        >
          <RoleForm
            pending={local.savePending}
            error={local.mutationError}
            onCancel={() => setPanel(null)}
            onSave={(draft) => void save(draft)}
          />
        </DetailPanel>
      )}
      {selected && panel?.kind === 'edit' && (
        <DetailPanel
          title={`编辑 ${selected.name}`}
          description="修改后会同步给工作区和之后启动的工作流运行。"
          onClose={() => setPanel(null)}
        >
          <RoleForm
            key={selected.id}
            role={selected}
            pending={local.savePending}
            error={local.mutationError}
            onCancel={() => setPanel({ kind: 'detail', id: selected.id })}
            onSave={(draft) => void save(draft, selected.id)}
          />
        </DetailPanel>
      )}
      {selected && panel?.kind === 'detail' && (
        <DetailPanel
          title={selected.name}
          description={selected.description || '角色说明'}
          onClose={() => setPanel(null)}
        >
          <div className="flex flex-wrap items-center justify-between gap-3">
            <Badge variant="secondary">工作区角色</Badge>
            <div className="flex gap-2">
              <Button
                variant="outline"
                onClick={() => open({ kind: 'edit', id: selected.id })}
              >
                编辑角色
              </Button>
              <Button
                variant="destructive"
                onClick={() => {
                  local.resetErrors()
                  setRemoving(selected)
                }}
              >
                删除角色
              </Button>
            </div>
          </div>
          <dl className="settings-facts">
            <dt>作用域</dt>
            <dd>工作区内所有成员共用</dd>
            <dt>引用方式</dt>
            <dd>工作流 Agent 节点按标题选择角色，运行时读取本页内容</dd>
          </dl>
          <Separator />
          <MarkdownPreview content={selected.content} />
        </DetailPanel>
      )}
      {importing && (
        <RoleImportDialog
          onClose={() => setImporting(false)}
          onImport={(input) =>
            local.importFile({ tid: local.tenantId, data: input })
          }
          onImported={(name, overwritten) => {
            toast.success(overwritten ? `已覆盖角色 ${name}` : `已导入角色 ${name}`)
            setImporting(false)
          }}
        />
      )}
      {removing && (
        <ConfirmDialog
          title={`删除“${removing.name}”？`}
          description="该角色将从工作区目录和工作流选择中移除；已有图里的名称引用不会被自动改写，引用它的运行会因找不到角色而无法启动。此操作无法撤销。"
          confirmLabel="删除角色"
          pending={local.deletePending}
          error={local.mutationError}
          onClose={() => setRemoving(undefined)}
          onConfirm={() => {
            local
              .remove({ tid: local.tenantId, rid: removing.id })
              .then(() => {
                toast.success(`已删除角色 ${removing.name}`)
                setRemoving(undefined)
                setPanel(null)
              })
              .catch(() => undefined)
          }}
        />
      )}
    </section>
  )
}
