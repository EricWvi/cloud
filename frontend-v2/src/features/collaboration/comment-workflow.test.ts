import { AxiosError, AxiosHeaders } from 'axios'
import { describe, expect, it } from 'vitest'
import type { FormField } from '@/api/generated.schemas'
import {
  extractWorkflowInputServerError,
  formatWorkflowOutput,
  resolveFormDraftValues,
  validateAndBuildWorkflowValues,
} from './comment-workflow'

describe('comment-workflow form helpers', () => {
  const fields: FormField[] = [
    { key: 'branch', label: '分支', type: 'text', required: true },
    { key: 'notes', label: '说明', type: 'textarea', required: false, defaultValue: '默认说明' },
    { key: 'config', label: '结构化配置', type: 'textarea', required: false, defaultValue: { env: 'staging' } },
    { key: 'count', label: '数量', type: 'number', required: true },
    { key: 'mode', label: '模式', type: 'select', required: false, defaultValue: 'fast', options: [{ label: 'fast', value: 'fast' }, { label: 'safe', value: 'safe' }] },
    { key: 'dryRun', label: '演练', type: 'boolean', required: false, defaultValue: true },
    { key: 'tags', label: '标签', type: 'multi_select', required: true, options: [{ label: 'ci', value: 'ci' }, { label: 'prod', value: 'prod' }] },
  ]

  it('resolves defaults and validates typed conversions', () => {
    const defaults = resolveFormDraftValues({}, fields)
    expect(defaults).toEqual({
      branch: '',
      notes: '默认说明',
      config: '{\n  "env": "staging"\n}',
      count: '',
      mode: 'fast',
      dryRun: true,
      tags: [],
    })

    const bad = validateAndBuildWorkflowValues(fields, { ...defaults, count: 'NaN' })
    expect(bad.errors).toEqual({
      branch: '请填写此必填字段',
      count: '请输入有效的数字',
      tags: '请至少选择一项',
    })

    const good = validateAndBuildWorkflowValues(fields, {
      ...defaults,
      branch: 'main',
      count: '12.5',
      tags: ['ci'],
    })
    expect(good.errors).toEqual({})
    expect(good.values).toEqual({
      branch: 'main',
      notes: '默认说明',
      config: '{\n  "env": "staging"\n}',
      count: 12.5,
      mode: 'fast',
      dryRun: true,
      tags: ['ci'],
    })
  })

  it('extracts field errors from workflow_run_input_invalid faults', () => {
    const err = new AxiosError('bad', 'ERR_BAD_REQUEST', undefined, undefined, {
      status: 400,
      statusText: 'Bad Request',
      headers: new AxiosHeaders(),
      config: { headers: new AxiosHeaders() },
      data: { code: 'workflow_run_input_invalid', params: { variable: 'branch', reason: 'required value is missing' }, requestId: '1' },
    })
    expect(extractWorkflowInputServerError(err)).toEqual({ variable: 'branch', message: '请填写此必填字段' })
    expect(extractWorkflowInputServerError(new Error('other'))).toBeNull()
  })

  it('formats JSON and plain text workflow outputs cleanly', () => {
    expect(formatWorkflowOutput('  ')).toBe('')
    expect(formatWorkflowOutput('{"ok":true}')).toBe('{\n  "ok": true\n}')
    expect(formatWorkflowOutput('plain text result')).toBe('plain text result')
  })
})
