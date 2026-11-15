import { defineConfig } from 'orval'

// Task-owned workflow handling and the unified task workflow history are v2-only candidates.
export default defineConfig({
  taskWorkflows: {
    input: { target: '../v2-server/task-workflows.openapi.json' },
    output: {
      target: 'src/features/collaboration/task-workflows-api/generated.ts',
      client: 'react-query', httpClient: 'axios', clean: true,
      override: { mutator: { path: 'src/lib/api-client.ts', name: 'customInstance' }, query: { version: 5 } },
    },
  },
})
