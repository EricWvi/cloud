import { render, screen } from '@testing-library/react'
import { expect, it } from 'vitest'
import { McpFailureDetail } from './mcp-failure-detail'

it('shows the stable reason and identifiers received from a failed MCP setup', () => {
  render(<McpFailureDetail detail={{
    kind: 'missing_agent_config', message: '', sourceChain: [], attempt: 1,
    resumable: true, recordedAt: 0, autoRetryable: false,
    code: 'mcp_setting_missing', pluginId: 'official/acme.search', settingId: 'api_key', transport: 'http',
  }} />)
  expect(screen.getByText('MCP 配置缺失或无效，请检查设置。')).toBeInTheDocument()
  expect(screen.getByText('插件 official/acme.search')).toBeInTheDocument()
  expect(screen.getByText('设置 api_key')).toBeInTheDocument()
  expect(screen.getByText('连接方式 http')).toBeInTheDocument()
})
