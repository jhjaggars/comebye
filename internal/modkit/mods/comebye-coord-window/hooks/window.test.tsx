import { expect, test } from 'claude-code/testing'

import { trimWindow, turnCount, versionOk } from './window'

const u = (text: string) => ({ role: 'user' as const, text, toolUses: [] })
const a = (text: string, ids: string[] = []) => ({
  role: 'assistant' as const,
  text,
  toolUses: ids.map(id => ({ tool_use_id: id, tool: 'Bash', input: {} })) as any,
})
const r = (id: string) => ({
  role: 'user' as const,
  text: '',
  toolUses: [],
  toolResults: [{ tool_use_id: id, text: 'out', isError: false }] as any,
})

// 5 turns; turn 3 has a tool call and its result.
const convo = [
  u('t1'), a('a1'), u('t2'), a('a2'), u('t3'), a('', ['x']), r('x'), a('a3'), u('t4'), a('a4'), u('t5'), a('a5'),
].map((m, i) => ({ ...m, handle: 'h' + i }))

test('versions', () => {
  expect(versionOk('2.1.321')).toBe(true)
  expect(versionOk('2.1.280')).toBe(false)
  expect(versionOk('2.2.0')).toBe(true)
  expect(versionOk('3.0.0-dev.2026')).toBe(true)
  expect(versionOk('nonsense')).toBe(false)
})

test('the window is the digest plus the last turns, cut at a prompt', () => {
  expect(turnCount(convo)).toBe(5)
  const out = trimWindow(convo, 'comebye-digest:v1\nx', 3)!
  expect(out.map(m => m.text)).toEqual(['comebye-digest:v1\nx', 'Context window trimmed in place. The digest above is current.', 't3', '', '', 'a3', 't4', 'a4', 't5', 'a5'])
  // the kept messages are the engine's own, handles intact
  expect(out.slice(2).every(m => m.handle)).toBe(true)
  // the tool_use and its result stay together
  expect(out.some(m => m.toolResults?.length)).toBe(true)
  expect(out[0].role).toBe('user')
  expect(out[1].role).toBe('assistant')
  expect(out[2].role).toBe('user')
})

test('there is nothing to drop when the conversation is short', () => {
  expect(trimWindow(convo, 'd', 5)).toBeNull()
  expect(trimWindow(convo, 'd', 9)).toBeNull()
  expect(trimWindow(convo, 'd', 0)).toBeNull()
})

function engine(on: any, tokens: number, calls: string[][] = [], stdout = 'comebye-digest:v1\nD', coordinator = true) {
  on('command.register', () => ({ value: { command: 'window' } }))
  on('clock.now', () => ({ value: 100_000_000 }))
  on('ui.toast', () => ({ value: undefined }))
  on('session.version', () => ({ value: { version: '2.1.321' } }))
  on('session.usage', () => ({ value: { startedAt: 0, context: { tokens, window: 1_000_000 }, rateLimits: [] } }))
  on('session.messages', () => ({ value: convo }))
  on('process.run', (_: any, e: any) => {
    const argv = e.argv ?? e
    calls.push(argv)
    const out = argv[1] === 'mod' ? JSON.stringify({ coordinator }) : stdout
    return { value: { exitCode: 0, stdout: out, stderr: '' } }
  })
  on('session.start', (_: any, e: any) => ({ cwd: e.cwd }))
}

test('a compaction nobody asked for is left to the engine', async ($, on) => {
  const calls: string[][] = []
  engine(on, 200_000, calls)
  on('session.compact', () => ({ messages: convo }))
  await $.session.start({ cwd: '/', surface: 'terminal', isInteractive: true })
  await $.session.compact({ trigger: 'plugin', messages: convo } as any)
  expect(calls.some(c => c[1] === 'coordinator')).toBe(false)
})

test('under the size, nothing is asked', async ($, on) => {
  const calls: string[][] = []
  engine(on, 50_000, calls)
  on('turn.complete', () => ({ text: '' }))
  await $.session.start({ cwd: '/', surface: 'terminal', isInteractive: true })
  await $.turn.complete({ answer: '', durationMs: 1, isAborted: false, turnId: 't' } as any)
  expect(calls.some(c => c[1] === 'coordinator')).toBe(false)
})

test('a worker that is not a coordinator never trims', async ($, on) => {
  const calls: string[][] = []
  engine(on, 900_000, calls, 'comebye-digest:v1\nD', false)
  on('turn.complete', () => ({ text: '' }))
  await $.session.start({ cwd: '/', surface: 'terminal', isInteractive: true })
  await $.turn.complete({ answer: '', durationMs: 1, isAborted: false, turnId: 't' } as any)
  expect(calls.some(c => c[1] === 'coordinator')).toBe(false)
})
