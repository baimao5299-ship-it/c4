// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// Model catalogue regression checks. The script transpiles the production
// helper in memory, so assertions exercise the same implementation that the
// group picker and channel monitor import.
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { dirname, join } from 'node:path'
import ts from 'typescript'

const here = dirname(fileURLToPath(import.meta.url))
const sourcePath = join(here, '../src/lib/model-sort.ts')
const source = readFileSync(sourcePath, 'utf8')
const transpiled = ts.transpileModule(source, {
  compilerOptions: { target: ts.ScriptTarget.ES2023, module: ts.ModuleKind.ESNext },
  reportDiagnostics: true,
})
const diagnostics = transpiled.diagnostics ?? []
if (diagnostics.length > 0) {
  console.error('CHECK MODEL SORT FAILED: TypeScript transpilation diagnostics')
  for (const diagnostic of diagnostics) console.error(ts.flattenDiagnosticMessageText(diagnostic.messageText, '\n'))
  process.exit(1)
}
const moduleURL = `data:text/javascript;base64,${Buffer.from(transpiled.outputText).toString('base64')}`
const { sortModelsLatestFirst } = await import(moduleURL)

const failures = []
function check(name, actual, expected) {
  const equal = JSON.stringify(actual) === JSON.stringify(expected)
  if (equal) {
    console.log(`  PASS ${name}`)
  } else {
    console.log(`  FAIL ${name}`)
    failures.push({ name, actual, expected })
  }
}
check(
  'numeric Claude versions stay distinct and newest-first',
  sortModelsLatestFirst(['claude-opus-4-6', 'claude-opus-5', 'claude-opus-4-7']),
  ['claude-opus-5', 'claude-opus-4-7', 'claude-opus-4-6']
)
check(
  'dated snapshots collapse to newest while canonical root remains visible',
  sortModelsLatestFirst(['deepseek-v4-pro', 'deepseek-v4-pro-0801', 'deepseek-v4-pro-0813']),
  ['deepseek-v4-pro-0813', 'deepseek-v4-pro']
)
check(
  'invalid dates remain separate identifiers',
  sortModelsLatestFirst(['foo-2024-13-40', 'foo-2024-12-31']),
  ['foo-2024-12-31', 'foo-2024-13-40']
)
check(
  'punctuation variants remain separate identifiers',
  sortModelsLatestFirst(['foo.bar', 'foo-bar']),
  ['foo.bar', 'foo-bar']
)
check(
  'latest alias sorts ahead of the bare model',
  sortModelsLatestFirst(['gpt-4o', 'gpt-4o-latest'])[0],
  'gpt-4o-latest'
)
check(
  'whitespace-only duplicates collapse after trimming',
  sortModelsLatestFirst(['  gpt-4o  ', 'gpt-4o']),
  ['gpt-4o']
)

if (failures.length > 0) {
  console.error('\nCHECK MODEL SORT FAILED')
  for (const failure of failures) {
    console.error(`  - ${failure.name}: expected ${JSON.stringify(failure.expected)}, got ${JSON.stringify(failure.actual)}`)
  }
  process.exit(1)
}
console.log('\nCHECK MODEL SORT OK')
