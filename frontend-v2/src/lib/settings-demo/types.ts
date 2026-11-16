import type {
  ModelConnection,
  ModelDefault,
} from '@/api/generated.schemas'

export const DEMO_SPACE = 'settings-demo-space'
/** The signed-in account as the demo pages need it; only the real role decides administration. */
export interface DemoUser {
  id: string
  displayName: string
  role: 'owner' | 'member'
}
/**
 * Browser-local demo data for model connections. It holds no people and no roles: the roster is
 * real (`GET /tenants/{tid}/members`), roles live in the workspace's server-side catalog, and the
 * only role that reaches these pages is the signed-in one.
 */
export interface Snapshot {
  schema: 6
  revision: number
  connections: ModelConnection[]
  modelDefault: ModelDefault
}
export type ConnectionDraft = Pick<
  ModelConnection,
  'name' | 'protocol' | 'authMode' | 'baseUrl' | 'enabled' | 'models'
>
