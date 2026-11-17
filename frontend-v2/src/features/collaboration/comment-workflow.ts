import { isAxiosError } from 'axios'

import type { FormField } from '@/api/generated.schemas'

/** Status badge presentation shared by comment workflow cards and the task workflow list. */
export const WORKFLOW_STATUS_META: Record<string, { label: string; tone: string }> = {
  pending: { label: '待启动', tone: 'pending' },
  running: { label: '运行中', tone: 'running' },
  awaitingInput: { label: '待确认', tone: 'review' },
  succeeded: { label: '已完成', tone: 'done' },
  failed: { label: '失败', tone: 'cancelled' },
  cancelled: { label: '已取消', tone: 'cancelled' },
  interrupted: { label: '已中断', tone: 'cancelled' },
}

export type FormFieldDraftValue = string | boolean | string[]

/** Derives the initial form control draft for one collaboration FormField descriptor. */
export function initialFieldDraft(field: FormField): FormFieldDraftValue {
  switch (field.type) {
    case 'boolean':
      return typeof field.defaultValue === 'boolean' ? field.defaultValue : false
    case 'number':
      return typeof field.defaultValue === 'number' && Number.isFinite(field.defaultValue)
        ? String(field.defaultValue)
        : typeof field.defaultValue === 'string'
          ? field.defaultValue
          : ''
    case 'multi_select':
      return Array.isArray(field.defaultValue)
        ? field.defaultValue.filter((v): v is string => typeof v === 'string')
        : []
    case 'textarea':
      if (typeof field.defaultValue === 'string') return field.defaultValue
      if (field.defaultValue !== undefined && field.defaultValue !== null) {
        return JSON.stringify(field.defaultValue, null, 2)
      }
      return ''
    default:
      return typeof field.defaultValue === 'string' ? field.defaultValue : ''
  }
}

/** Combines user-edited drafts with FormField defaults for every descriptor field. */
export function resolveFormDraftValues(
  raw: Record<string, FormFieldDraftValue>,
  fields: FormField[] | undefined,
): Record<string, FormFieldDraftValue> {
  const out: Record<string, FormFieldDraftValue> = {}
  for (const field of fields ?? []) {
    out[field.key] = Object.hasOwn(raw, field.key) ? raw[field.key] : initialFieldDraft(field)
  }
  return out
}

/**
 * Validates form drafts against the FormDescriptor schema and converts values to the wire
 * JSON shapes expected by `CheckStart` (`number`, `boolean`, `string`, `string[]`).
 */
export function validateAndBuildWorkflowValues(
  fields: FormField[],
  draft: Record<string, FormFieldDraftValue>,
): { values: Record<string, unknown>; errors: Record<string, string> } {
  const values: Record<string, unknown> = {}
  const errors: Record<string, string> = {}

  for (const field of fields) {
    const val = Object.hasOwn(draft, field.key) ? draft[field.key] : initialFieldDraft(field)
    switch (field.type) {
      case 'boolean': {
        values[field.key] = Boolean(val)
        break
      }
      case 'number': {
        const text = typeof val === 'string' ? val.trim() : ''
        if (text === '') {
          if (field.required) errors[field.key] = '请填写此必填字段'
          break
        }
        const parsed = Number(text)
        if (!Number.isFinite(parsed)) {
          errors[field.key] = '请输入有效的数字'
          break
        }
        values[field.key] = parsed
        break
      }
      case 'select': {
        const chosen = typeof val === 'string' ? val : ''
        if (chosen === '') {
          if (field.required) errors[field.key] = '请选择此必填字段'
          break
        }
        if (field.options && field.options.length > 0 && !field.options.some(o => o.value === chosen)) {
          errors[field.key] = '请选择有效选项'
          break
        }
        values[field.key] = chosen
        break
      }
      case 'multi_select': {
        const picked = Array.isArray(val) ? val : []
        if (picked.length === 0) {
          if (field.required) errors[field.key] = '请至少选择一项'
          break
        }
        if (field.options && field.options.length > 0 && picked.some(v => !field.options!.some(o => o.value === v))) {
          errors[field.key] = '存在无效选项，请重新选择'
          break
        }
        values[field.key] = picked
        break
      }
      default: {
        const text = typeof val === 'string' ? val : ''
        if (text.trim() === '') {
          if (field.required) errors[field.key] = '请填写此必填字段'
          break
        }
        values[field.key] = text
        break
      }
    }
  }

  return { values, errors }
}

/** Translates a server `workflow_run_input_invalid` reason code into concise Chinese feedback. */
export function translateWorkflowInputReason(reason: string): string {
  switch (reason) {
    case 'required value is missing':
      return '请填写此必填字段'
    case 'type_mismatch':
      return '输入类型与字段要求不符'
    case 'value is not one of the configured options':
      return '请选择有效选项'
    case 'length_exceeded':
      return '内容长度超出限制'
    case 'undeclared':
      return '包含工作流未声明的参数，请取消后重新 @ 工作流'
    default:
      return reason ? `输入无效：${reason}` : '输入格式不正确'
  }
}

/** Extracts field-level error metadata when the server returns `workflow_run_input_invalid`. */
export function extractWorkflowInputServerError(error: unknown): { variable: string; message: string } | null {
  if (!isAxiosError(error)) return null
  const data = error.response?.data as { code?: unknown; params?: { variable?: unknown; reason?: unknown } } | undefined
  if (data?.code !== 'workflow_run_input_invalid') return null
  const variable = typeof data.params?.variable === 'string' ? data.params.variable : ''
  const reason = typeof data.params?.reason === 'string' ? data.params.reason : ''
  if (!variable) return null
  return { variable, message: translateWorkflowInputReason(reason) }
}

/** Formats persisted workflow final output (`JSON` string or plain text) for timeline display. */
export function formatWorkflowOutput(raw: string): string {
  const trimmed = raw.trim()
  if (!trimmed) return ''
  try {
    const parsed: unknown = JSON.parse(trimmed)
    if (typeof parsed === 'string') return parsed
    return JSON.stringify(parsed, null, 2)
  } catch {
    return trimmed
  }
}
