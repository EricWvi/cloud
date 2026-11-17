import type { Snapshot } from './types'

export function seed(): Snapshot {
  const now = new Date().toISOString()
  return {
    schema: 6,
    revision: 1,
    connections: [
      {
        id: 'demo-connection',
        name: '团队日常模型 · 示例',
        protocol: 'openai-completions',
        authMode: 'bearer',
        baseUrl: 'https://models.example.com/v1',
        enabled: true,
        credentialConfigured: true,
        models: [
          {
            id: 'demo/general',
            name: '通用模型 · 示例',
            contextWindow: 128000,
            maxTokens: 8192,
          },
        ],
        version: 1,
        createdAt: now,
        updatedAt: now,
      },
    ],
    modelDefault: {
      connectionId: 'demo-connection',
      modelId: 'demo/general',
      version: 1,
    },
  }
}
