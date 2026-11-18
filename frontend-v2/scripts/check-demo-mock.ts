// Checks a frontend build for the demo mocks registered in src/lib/demo/registry.ts.
//
//   node scripts/check-demo-mock.ts <dir>          fail if any mock's marker is in the build
//   node scripts/check-demo-mock.ts <dir> --demo   fail if any mock's marker is missing
//
// The first form guards every build that ships (the default build and the offline package): a mock
// gated in a way the bundler cannot fold away leaves its data, and so its marker, in the output.
// The second runs on a `VITE_DEMO_MOCK=1` build and proves each marker really reaches a bundle, so
// the first form cannot pass because a marker went stale.
import { readFileSync, readdirSync } from 'node:fs'
import { join, relative } from 'node:path'
import { demoMocks } from '../src/lib/demo/registry.ts'

const args = process.argv.slice(2)
const demo = args.includes('--demo')
const dirs = args.filter((arg) => arg !== '--demo')
if (dirs.length !== 1) {
  console.error('usage: node scripts/check-demo-mock.ts <build directory> [--demo]')
  process.exit(2)
}
const root = dirs[0]!

const files = readdirSync(root, { recursive: true, withFileTypes: true })
  .filter((entry) => entry.isFile())
  .map((entry) => join(entry.parentPath, entry.name))
if (files.length === 0) {
  console.error(`${root} has no files; build first.`)
  process.exit(2)
}
const contents = files.map((file) => ({ file: relative(root, file), text: readFileSync(file, 'utf8') }))

const problems: string[] = []
for (const mock of demoMocks) {
  const found = contents.filter(({ text }) => text.includes(mock.marker)).map(({ file }) => file)
  if (demo && found.length === 0) {
    problems.push(`${mock.module}: marker ${JSON.stringify(mock.marker)} is not in the demo build`)
  } else if (!demo && found.length > 0) {
    problems.push(`${mock.module}: marker ${JSON.stringify(mock.marker)} found in ${found.join(', ')}`)
  }
}

if (problems.length > 0) {
  console.error(
    demo
      ? 'The demo build is missing mock markers; update the marker in src/lib/demo/registry.ts:'
      : 'Demo mocks are in a build without VITE_DEMO_MOCK=1. Gate each use with DEMO_MOCK read directly (see src/lib/demo/flags.ts):',
  )
  for (const problem of problems) console.error(`  ${problem}`)
  process.exit(1)
}
console.log(
  `${demo ? 'All' : 'None of the'} ${demoMocks.length} demo mock markers found in ${root} (${files.length} files).`,
)
