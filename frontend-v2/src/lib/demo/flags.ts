/**
 * Whether the demo mock layer is on.
 *
 * `VITE_DEMO_MOCK=1` turns it on for a demo run; unset or any other value keeps every page on real
 * data, so `npm run check` and `npm run e2e` never depend on mock content. This is the only place
 * that reads the variable: a demo command sets it once and every mock module follows.
 *
 * It is a constant, not a function, so a build without the flag folds every `DEMO_MOCK && …` branch
 * away and drops the mock data with it: the bundler inlines an exported constant across modules but
 * not a function's result. Read it directly at the point of use, never wrapped in another function
 * or a variable, or the mock stays in the deployed bundle. `scripts/check-demo-mock.ts` scans built
 * output for each module's `marker` (see `registry.ts`) to catch that. Tests replace this module
 * with a getter to switch it per test.
 */
export const DEMO_MOCK: boolean = import.meta.env.VITE_DEMO_MOCK === '1'
