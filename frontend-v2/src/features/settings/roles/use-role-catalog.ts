import { useWorkspaceRoles } from './use-roles'
import type { Role } from '@/lib/roles-api/generated'

/**
 * The workspace role catalog as the workflow editor and run inspector read it: the same
 * server-side rows Settings edits, so a role configured here is what execution injects.
 */
export function useRoleCatalog() {
  const local = useWorkspaceRoles()
  return {
    ...local.query,
    data: local.roles as Role[],
  }
}
