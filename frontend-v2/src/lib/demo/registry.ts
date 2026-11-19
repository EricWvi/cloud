/**
 * The register of demo mocks for the 11-06 recording.
 *
 * Every mock lives in this directory and is on only when `DEMO_MOCK` (see `flags.ts`) says so;
 * pages never import mock data directly, they ask a module here. Each mock module is listed below
 * with the backend that replaces it. The change that lands that backend deletes the module and its
 * entry together, so this list is always the set of screens the recording still fakes.
 * `registry.test.ts` fails when a module is added without an entry or an entry outlives its module.
 *
 * Each entry also names a `marker`: text from the module's mock data that a build carries only if
 * the mock is in it. `scripts/check-demo-mock.ts` fails a build without the flag that still
 * contains any marker, and a demo build that lacks one (so a stale marker cannot pass silently).
 */
export interface DemoMock {
  /** The module in this directory, e.g. `dashboard.ts`. */
  module: string
  /** The recording scene whose screens show it. */
  scene: 'newcomer' | 'lead' | 'ops'
  /** What the mock stands in for. */
  covers: string
  /** The backend change that replaces it and deletes the module. */
  replacedBy: string
  /** Text of the module's mock data, found in a build only when the mock is included. */
  marker: string
}

export const demoMocks: readonly DemoMock[] = [
  {
    module: 'space-usage.ts',
    scene: 'lead',
    covers: 'Workspace token usage, model requests and tool categories for the Dashboard video',
    replacedBy: 'An authorized workspace usage aggregation endpoint with explicit time boundaries',
    marker: '本周 · 11-02 至 11-06',
  },
  {
    module: 'cdase.ts',
    scene: 'newcomer',
    covers: 'SFMEA review workflow and report recording fixture',
    replacedBy: 'Installed cdase workflow with real Agent execution and persisted review artifacts',
    marker: '退款申请接口 · SFMEA 评审报告',
  },
  {
    module: 'dts.ts',
    scene: 'ops',
    covers: 'DTS simulated receipts after real Cloud task creation',
    replacedBy: 'An authenticated DTS connector with durable external delivery receipts',
    marker: 'DTS-DEMO-',
  },
  {
    module: 'plugin-installs.ts',
    scene: 'lead',
    covers: 'Browser-local install state for the sample MCP/Hook and for market or preinstalled Agents, plus the install delay',
    replacedBy: 'A server-backed install and runtime for every plugin kind, including MCP and Hooks (#938)',
    marker: '生命周期 Hook 的演示安装只记录浏览器本地状态',
  },
]
