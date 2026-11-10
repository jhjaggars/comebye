import { expect, test } from 'claude-code/testing'

import { splitArgs } from './argv'
import { bandText } from './band-model'
import { check } from './guard-rules'

const BAND = {
  plugin: 'comebye-hub',
  component: 'AbovePrompt',
  props: { hasSurvey: false },
} as const

const RAW = JSON.stringify({
  agent: 'laharl/w1:p1',
  coordinator: false,
  todos: 2,
  first_todo: 'may the worker delete the old key?',
  queued: 1,
  handoff: { id: 'h1', status: 'running', progress: 'pushed, CI running', phase: 'ci', ci: 'running', landing: 'landed' },
})

function engine(on: any, stdout = RAW, calls: string[][] = [], toasts: string[] = []) {
  on('command.register', () => ({ value: { command: 'x' } }))
  on('clock.now', () => ({ value: 1_000_000 }))
  on('clock.every', () => ({ value: { cancel() {} } }))
  on('clock.after', () => ({ value: { cancel() {} } }))
  on('ui.toast', (_: any, e: any) => {
    toasts.push(JSON.stringify(e))
    return { value: undefined }
  })
  on('process.run', (_: any, e: any) => {
    calls.push(e.argv ?? e)
    return { value: { exitCode: 0, stdout, stderr: '' } }
  })
}

test('the band shows what is pending, on every surface', async ($, on) => {
  const calls: string[][] = []
  engine(on, RAW, calls)
  on('session.start', (_, e) => ({ cwd: e.cwd }))
  await $.session.start({ cwd: '/', surface: 'terminal', isInteractive: true })

  for (const surface of ['terminal', 'desktop', 'mobile'] as const) {
    const ui = await $.ui.mount({ ...BAND, surface })
    const line = await ui.find({ type: 'Text', text: /Comebye:/ })
    expect(line?.text).toContain('2 to-dos wait on you')
    expect(line?.text).toContain('1 notice queued')
    expect(line?.text).toContain('CI running')
    await ui.unmount()
  }
})

test('the band is not drawn when nothing is pending', async ($, on) => {
  engine(on, JSON.stringify({ agent: 'a', todos: 0, queued: 0 }))
  on('session.start', (_, e) => ({ cwd: e.cwd }))
  on('ui.render', ($, e) => {
    const { Text } = $.ui.resolve(e)
    return <Text>engine</Text>
  })
  await $.session.start({ cwd: '/', surface: 'terminal', isInteractive: true })
  const ui = await $.ui.mount({ ...BAND, surface: 'terminal' })
  expect(await ui.find({ type: 'Text', text: /Comebye:/ })).toBeFalsy()
  await ui.unmount()
})

test('a command runs the comebye CLI and shows its output, no model turn', async ($, on) => {
  const calls: string[][] = []
  engine(on, 'plain output', calls)
  const out = await $.command.run({ command: 'todo', args: 'add "may I?" --kind decision' })
  expect(out.text).toBe('plain output')
  const argv = calls.find(c => c[1] === 'todo')
  expect(argv).toEqual(['comebye', 'todo', 'add', 'may I?', '--kind', 'decision'])
  const bare = await $.command.run({ command: 'todo', args: '' })
  expect(calls.filter(c => c[1] === 'todo').pop()).toEqual(['comebye', 'todo', 'list'])
  expect(bare.text).toBe('plain output')
})

test('the guard warns in the band and never blocks', async ($, on) => {
  const toasts: string[] = []
  engine(on, '{}', [], toasts)
  on('tool.call', () => ({ result: {} }))
  for (const command of ['git add -A', 'git push --force origin x', 'gh pr merge 3']) {
    const ran = await $.tool.call({ tool: 'Bash', command })
    expect((ran as any).deny).toBeUndefined()
  }
  expect(toasts.length).toBe(3)
  const quiet = await $.tool.call({ tool: 'Bash', command: 'git add a.go' })
  expect((quiet as any).deny).toBeUndefined()
  expect(toasts.length).toBe(3)
  const ui = await $.ui.mount({ ...BAND, surface: 'terminal' })
  expect((await ui.find({ type: 'Text', text: /gh pr merge/ }))?.text).toContain('needs the user')
  await ui.unmount()
})

test('splitArgs reads quotes', () => {
  expect(splitArgs(`a "b c" 'd e' f\\ g ""`)).toEqual(['a', 'b c', 'd e', 'f g', ''])
})

test('bandText is null when quiet', () => {
  expect(bandText(null)).toBeNull()
  expect(bandText({ coordinator: false, todos: 0, queued: 0 })).toBeNull()
  expect(bandText({ coordinator: true, todos: 0, queued: 0, headline: 'Nothing waits on you.' })).toBe('Comebye: Nothing waits on you.')
})

const ctx = (url?: string, own: string[] = []) => ({ ownOwners: own, remoteUrl: async () => url })
const rules = async (c: string, url?: string, own: string[] = []) => (await check(c, ctx(url, own))).map(h => h.rule)

test('guard rules', async () => {
  expect(await rules('git add -A')).toEqual(['git-add-all'])
  expect(await rules('git add .')).toEqual(['git-add-all'])
  expect(await rules('git -C /x add --all')).toEqual(['git-add-all'])
  expect(await rules('git add src/a.go internal/b.go')).toEqual([])
  expect(await rules('git commit -m "do not git add -A"')).toEqual([])
  expect(await rules('git push --force-with-lease origin feat/x')).toEqual(['force-push'])
  expect(await rules('git push -f origin x')).toEqual(['force-push'])
  expect(await rules('git push origin +feat/x')).toEqual(['force-push'])
  expect(await rules('git push -u origin feat/x', 'git@github.com:me/r.git', ['me'])).toEqual([])
  expect(await rules('git push origin x', 'https://github.com/NVIDIA/OpenShell', ['me'])).toEqual(['upstream-push'])
  expect(await rules('git push upstream x', 'https://github.com/o/r')).toEqual(['upstream-push'])
  expect(await rules('git push origin x', 'https://github.com/o/r')).toEqual([])
  expect(await rules('gh pr comment 3 --body hi')).toEqual(['gh-pr-write'])
  expect(await rules('ls && gh pr merge 3 --squash')).toEqual(['gh-pr-write'])
  expect(await rules('gh pr view 3')).toEqual([])
})
