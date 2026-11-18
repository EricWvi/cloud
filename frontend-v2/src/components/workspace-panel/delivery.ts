import { isAxiosError } from 'axios'
import type { WorkspaceDelivery, WorkspaceDeliveryState } from '@/lib/workspace-api/generated'

/** What the change view says about the deliveries behind it. */
export interface DeliveryNotice {
  /** A turn is running: 'previous' when the list shows the delivery before it, 'first' when there is none yet. */
  inProgress: 'previous' | 'first' | null
  /** A failed delivery newer than the one shown, or one with nothing good shown at all. */
  failure?: WorkspaceDelivery
  /** The shown delivery, which a patch is downloaded from; undefined when there is none. */
  view?: WorkspaceDelivery
}

/**
 * Reads the server's delivery state for the change view. A legacy source (`working_directory`)
 * has no deliveries, so it says nothing and offers no download.
 */
export function deliveryNotice(state: WorkspaceDeliveryState | undefined): DeliveryNotice | null {
  if (!state || state.mode !== 'delivery') return null
  const view = state.viewDelivery
  const failed = state.latestFailedDelivery
  const failure = failed && (!view || Date.parse(failed.createdAt) > Date.parse(view.createdAt)) ? failed : undefined
  return { inProgress: state.inProgress ? (view ? 'previous' : 'first') : null, failure, view }
}

/** The file name the server names a delivery's patch with. */
export const patchFileName = (deliveryId: string) => `delivery-${deliveryId}.patch`

/** Saves a downloaded patch through the browser under its delivery's file name. */
export function savePatch(blob: Blob, deliveryId: string): void {
  const url = URL.createObjectURL(blob)
  const link = document.createElement('a')
  link.href = url
  link.download = patchFileName(deliveryId)
  link.click()
  URL.revokeObjectURL(url)
}

/**
 * What a failed patch download means to the person. The patch is requested as a blob, so the
 * server's JSON fault arrives as one too and is read here.
 */
export async function patchFailureText(error: unknown): Promise<string> {
  const fallback = '下载失败，请稍后重试。'
  if (!isAxiosError(error)) return fallback
  let body: unknown = error.response?.data
  if (body instanceof Blob) {
    try {
      body = JSON.parse(await body.text())
    } catch {
      body = undefined
    }
  }
  const fault = body as { code?: unknown; params?: { message?: unknown } } | undefined
  if (fault?.code === 'patch_too_large') return typeof fault.params?.message === 'string' ? fault.params.message : '改动过大，无法在线下载'
  if (fault?.code === 'delivery_failed') return '这一轮交付失败，没有可下载的改动。'
  return fallback
}
