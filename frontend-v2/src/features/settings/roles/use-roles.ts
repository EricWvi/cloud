import { isAxiosError } from 'axios'
import { useQueryClient } from '@tanstack/react-query'
import { useGetApiV1MeTenants } from '@/api/me/me'
import {
  getListRolesQueryKey,
  useCreateRole,
  useDeleteRole,
  useImportRole,
  useListRoles,
  useUpdateRole,
  type RoleError,
} from '@/lib/roles-api/generated'

function codeOf(error: unknown): string | undefined {
  if (!isAxiosError(error)) return undefined
  const body = error.response?.data as Partial<RoleError> | undefined
  return typeof body?.code === 'string' ? body.code : undefined
}

const messages: Record<string, string> = {
  not_found: '你已不在这个工作区，请刷新页面。',
  role_not_found: '角色已不存在，请刷新列表。',
  role_name_conflict: '已有同名角色，请改用其他名称或选择覆盖。',
  role_file_type: '请选择 Markdown 格式的角色文件。',
  role_file_too_large: '角色文件不能超过 1 MiB。',
  role_front_matter_missing:
    '角色文件缺少 YAML front matter，需要声明 name 和 description。',
  role_front_matter_invalid: '角色文件的 YAML front matter 无法解析。',
  role_name_missing: '角色缺少标题。',
  role_name_invalid: '标题需为 1–80 个字符，不能包含换行或路径分隔符。',
  role_description_missing: '角色文件缺少 description。',
  role_description_too_large: '描述最多 500 个字符。',
  role_content_too_large: '内容最多 100,000 个字符。',
}

export function roleError(error: unknown): unknown {
  const code = codeOf(error)
  if (code && messages[code]) return new Error(messages[code])
  if (isAxiosError(error) && error.response?.status === 401)
    return new Error('登录已失效，请重新登录。')
  return error
}

export function isRoleNameConflict(error: unknown) {
  return codeOf(error) === 'role_name_conflict'
}

/**
 * The workspace's server-backed role catalog. Settings and every workflow picker read the same
 * rows, so a role one member creates is selectable — and injectable at execution — for everyone
 * in the workspace.
 */
export function useWorkspaceRoles() {
  const tenants = useGetApiV1MeTenants()
  const tenantId = tenants.data?.items?.[0]?.id ?? ''
  const client = useQueryClient()
  const query = useListRoles(tenantId, {
    query: { enabled: tenantId !== '', retry: false },
  })
  const invalidate = async () => {
    if (!tenantId) return
    await client.invalidateQueries({ queryKey: getListRolesQueryKey(tenantId) })
  }
  const create = useCreateRole({ mutation: { onSuccess: invalidate } })
  const update = useUpdateRole({ mutation: { onSuccess: invalidate } })
  const remove = useDeleteRole({ mutation: { onSuccess: invalidate } })
  const importFile = useImportRole({ mutation: { onSuccess: invalidate } })
  return {
    tenantId,
    query,
    roles: query.data?.items ?? [],
    create: create.mutateAsync,
    update: update.mutateAsync,
    remove: remove.mutateAsync,
    importFile: importFile.mutateAsync,
    savePending: create.isPending || update.isPending,
    deletePending: remove.isPending,
    // A disabled catalog query stays pending forever, so wait on it only once the workspace is
    // known; otherwise a failed tenant lookup would hide its own error and retry button.
    loading: tenants.isLoading || (tenantId !== '' && query.isPending),
    error: tenants.isSuccess && !tenantId
      ? new Error('你还没有加入任何工作区，暂时无法读取角色目录。')
      : roleError(tenants.error ?? query.error),
    // Import reports its own failures inside the dialog, including the name conflict it resolves.
    mutationError: roleError(create.error ?? update.error ?? remove.error),
    resetErrors: () => {
      create.reset()
      update.reset()
      remove.reset()
      importFile.reset()
    },
    refetch: () => {
      void tenants.refetch()
      if (tenantId) void query.refetch()
    },
  }
}
