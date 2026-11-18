import { isAxiosError } from 'axios'
import type { WorkspaceDelivery, WorkspaceDeliveryState } from '@/lib/workspace-api/generated'
import type { WorkspaceSource } from './source'

/** What the change view says about the deliveries behind it. */
export interface DeliveryNotice {
  /** A turn is running: 'previous' when the list shows the delivery before it, 'first' when there is none yet. */
  inProgress: 'previous' | 'first' | null
  /** The failed delivery to report, with the sentence that reports it. */
  failure?: { delivery: WorkspaceDelivery; text: string }
  /** The shown delivery, which a patch is downloaded from; undefined when there is none. */
  view?: WorkspaceDelivery
}

/**
 * Reads the server's delivery state for the change view. A legacy source (`working_directory`)
 * has no deliveries, so it says nothing and offers no download.
 *
 * Which failure to report depends on the source. A conversation's or a task run's turns follow one
 * another, so a failure matters only while it is the newest outcome, in the server's order
 * (`latestDelivery`). A workflow run's Agent nodes share one checkout but are separate attempts: a
 * later node's success does not undo another node's failed delivery, so the newest failure is always
 * shown, naming its node. While a turn runs, the shown outcomes belong to the previous turn.
 */
export function deliveryNotice(state: WorkspaceDeliveryState | undefined, kind: WorkspaceSource['kind']): DeliveryNotice | null {
  if (!state || state.mode !== 'delivery') return null
  const view = state.viewDelivery
  const turn = state.inProgress ? '上一轮' : '本轮'
  let failure: DeliveryNotice['failure']
  if (kind === 'run') {
    const failed = state.latestFailedDelivery
    if (failed) {
      const who = failed.nodeId ? `节点 ${failed.nodeId} 的交付失败` : '交付失败'
      failure = { delivery: failed, text: `${who}：${failed.reason ?? '原因未知'}${view ? '。下面是最近一次成功的交付。' : ''}` }
    }
  } else if (state.latestDelivery?.status === 'failed') {
    const failed = state.latestDelivery
    failure = { delivery: failed, text: `${turn}交付失败：${failed.reason ?? '原因未知'}${view ? '。下面是上一次成功的交付。' : ''}` }
  }
  return { inProgress: state.inProgress ? (view ? 'previous' : 'first') : null, failure, view }
}

/** What the list says when the shown delivery has no changes. */
export const emptyDeliveryText = (inProgress: boolean) => (inProgress ? '上一轮没有改动。' : '本轮没有改动。')

/** The file name the server names a delivery's patch with. */
export const patchFileName = (deliveryId: string) => `delivery-${deliveryId}.patch`

/** Saves a downloaded patch through the browser under its delivery's file name. */
export function savePatch(blob: Blob, deliveryId: string): void {
  const url = URL.createObjectURL(blob)
  const link = document.createElement('a')
  link.href = url
  link.download = patchFileName(deliveryId)
  link.click()
  // Some browsers start the download only after click() returns; revoking at once can abort it.
  setTimeout(() => URL.revokeObjectURL(url), 0)
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
