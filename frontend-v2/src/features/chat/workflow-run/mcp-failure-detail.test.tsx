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

it('distinguishes a missing Node lifecycle capability from a missing MCP installation', () => {
  render(<McpFailureDetail detail={{
    kind: 'missing_agent_config', message: '', sourceChain: [], attempt: 1,
    resumable: true, recordedAt: 0, autoRetryable: false,
    code: 'mcp_node_capability_missing',
  }} />)
  expect(screen.getByLabelText('MCP 失败详情')).toHaveTextContent('当前 Node 缺少 MCP 生命周期能力，请更新 Node 后重试。')
  expect(screen.getByText('mcp_node_capability_missing')).toBeInTheDocument()
  expect(screen.queryByText('MCP 插件或所选版本不可用，请重新安装。')).not.toBeInTheDocument()
  expect(screen.queryByText(/^插件 /)).not.toBeInTheDocument()
})

it('explains that a lifecycle-capable Node still requires service lifecycle support', () => {
  render(<McpFailureDetail detail={{
    kind: 'missing_agent_config', message: '', sourceChain: [], attempt: 1,
    resumable: true, recordedAt: 0, autoRetryable: false,
    code: 'mcp_lifecycle_not_ready',
  }} />)
  expect(screen.getByLabelText('MCP 失败详情')).toHaveTextContent('当前服务尚未支持 MCP 生命周期，暂时无法运行带 MCP 的节点。')
  expect(screen.getByText('mcp_lifecycle_not_ready')).toBeInTheDocument()
  expect(screen.queryByText(/请更新 Node/)).not.toBeInTheDocument()
})
