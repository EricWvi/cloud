import type { WorkflowNodeErrorDetail } from '@/lib/workflows/runtime'

const reasons: Record<string, string> = {
  mcp_plugin_missing: 'MCP 插件或所选版本不可用，请重新安装。',
  mcp_setting_missing: 'MCP 配置缺失或无效，请检查设置。',
  mcp_http_capability_missing: '当前 Agent 不支持 HTTP MCP，请选择支持它的 Agent。',
  mcp_load_session_missing: '当前 Agent 不支持 MCP 所需的会话加载能力。',
  mcp_node_capability_missing: '当前 Node 缺少 MCP 生命周期能力，请更新 Node 后重试。',
  mcp_lifecycle_not_ready: '当前服务尚未支持 MCP 生命周期，暂时无法运行带 MCP 的节点。',
  mcp_selection_invalid: 'MCP 选择无效，请检查插件版本和数量。',
}

/** Displays setup diagnostics that contain identifiers only. */
export function McpFailureDetail({ detail }: { detail: WorkflowNodeErrorDetail }) {
  if (!detail.code || !reasons[detail.code]) return null
  return (
    <div className="space-y-1" aria-label="MCP 失败详情">
      <p>{reasons[detail.code]}</p>
      <p className="break-all font-mono">{detail.code}</p>
      {detail.pluginId && <p>插件 {detail.pluginId}</p>}
      {detail.settingId && <p>设置 {detail.settingId}</p>}
      {detail.transport && <p>连接方式 {detail.transport}</p>}
    </div>
  )
}
