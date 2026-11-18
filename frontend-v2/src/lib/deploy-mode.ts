import { useQuery } from '@tanstack/react-query'
import { getProviders } from './auth-api'

/**
 * Whether a provider list marks a deployed install. `V2_LOGIN` names the login methods a server
 * offers; `email` signs in by address alone and exists only for development and demos, so a server
 * that offers login methods but not `email` is a deployment.
 */
export function isDeployedProviders(providers: string[] | undefined): boolean {
  return (providers ?? []).length > 0 && !(providers ?? []).includes('email')
}

/**
 * Reads the deployment mode through the same providers query the login screen uses. Unknown,
 * empty or failing responses stay on the development behavior (everything visible), so a gateway
 * hiccup never hides settings pages it cannot prove should be hidden.
 */
export function useDeployedVersion() {
  const query = useQuery({
    queryKey: ['auth', 'providers'],
    queryFn: getProviders,
    retry: false,
  })
  return isDeployedProviders(query.data?.providers)
}

/**
 * Whether the server offers the email login, which is also what lets an owner invite by address.
 * Like `useDeployedVersion`, an unknown answer keeps the development behavior.
 */
export function useEmailLogin() {
  const query = useQuery({
    queryKey: ['auth', 'providers'],
    queryFn: getProviders,
    retry: false,
  })
  return !isDeployedProviders(query.data?.providers)
}
