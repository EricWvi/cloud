import { defineConfig } from 'orval'

export default defineConfig({
  mcp: {
    input: { target: '../v2-server/mcp.openapi.json' },
    output: {
      target: 'src/lib/mcp-api/generated.ts',
      client: 'react-query',
      httpClient: 'axios',
      clean: true,
      override: { mutator: { path: 'src/lib/api-client.ts', name: 'customInstance' } },
    },
  },
})
