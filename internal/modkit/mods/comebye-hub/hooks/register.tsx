import { atom, read, update } from 'claude-code'
import type { Register, EngineInterface as Engine } from 'claude-code'

import type { Band, Warning } from '../types'
import { splitArgs, COMMANDS } from './argv'
import { bandText, toBand, warningText, WARNING_MS } from './band-model'
import type { RawBand } from './band-model'
import { check } from './guard-rules'

const bandAtom = atom({ plugin: 'comebye-hub', key: 'band' } as const, null as Band | null, { shape: 'band-1' })
const warnAtom = atom({ plugin: 'comebye-hub', key: 'warning' } as const, null as Warning | null, { shape: 'warning-1' })

const MAX_OUTPUT = 6000

type State = { failures: number; tick: number; lastKey: string; lastAt: number; inFlight: boolean }

// One light call to the comebye CLI, which holds the agent's token. After
// three failures in a row (not an agent, no server) it asks every tenth tick.
async function refresh($: Engine, st: State, force: boolean) {
  if (st.inFlight) return
  st.tick += 1
  if (!force && st.failures >= 3 && st.tick % 10 !== 0) return
  st.inFlight = true
  try {
    const r = await $.process.run(['comebye', 'mod', 'band', '--json'], { timeoutMs: 8000 })
    if (r.exitCode !== 0) throw new Error('exit ' + r.exitCode)
    const band = toBand(JSON.parse(r.stdout) as RawBand)
    st.failures = 0
    const key = JSON.stringify(band)
    if (key !== st.lastKey) {
      st.lastKey = key
      await update($, bandAtom, () => band)
    }
  } catch {
    st.failures += 1
    if (st.failures === 3 && st.lastKey !== '') {
      st.lastKey = ''
      await update($, bandAtom, () => null)
    }
  } finally {
    st.inFlight = false
    st.lastAt = await $.clock.now()
  }
}

export const register: Register = (on, options) => {
  const pollMs = Math.max(30, Number(options?.poll_seconds ?? 30)) * 1000
  const ownOwners = String(options?.own_owners ?? '')
    .split(',')
    .map(s => s.trim().toLowerCase())
    .filter(Boolean)

  const st: State = { failures: 0, tick: 0, lastKey: '', lastAt: 0, inFlight: false }

  on('session.start', async ($, e, next) => {
    for (const [name, c] of Object.entries(COMMANDS)) {
      await $.command.register({ name, description: c.description, argumentHint: c.hint })
    }
    void refresh($, st, true)
    $.clock.every(pollMs, () => void refresh($, st, false))

    return next(e)
  })

  on('turn.complete', async ($, e, next) => {
    if ((await $.clock.now()) - st.lastAt > 10_000) void refresh($, st, true)

    return next(e)
  })

  // ---- instant commands: no model turn, the CLI's output is the answer ----
  on('command.run', async ($, e, next) => {
    const c = COMMANDS[e.command]
    if (!c) return next(e)
    const r = await $.process.run(['comebye', ...c.argv(splitArgs(e.args))], { stdin: '', timeoutMs: 60_000 })
    let text = (r.stdout + (r.stderr ? '\n' + r.stderr : '')).trim()
    if (text.length > MAX_OUTPUT) text = text.slice(0, MAX_OUTPUT) + '\n… (cut)'
    if (r.exitCode !== 0) text = `comebye ${e.command} failed (exit ${r.exitCode})\n${text}`
    void refresh($, st, true)

    return { text: text || '(no output)', isTextDisplayOnly: true }
  })

  // ---- warn-only guard: never denies, never rewrites ----
  on('tool.call', { tool: 'Bash' }, async ($, e, next) => {
    try {
      const hits = await check(e.command, {
        ownOwners,
        remoteUrl: async (name, dir) => {
          const r = await $.process.run(['git', 'remote', 'get-url', name], { cwd: dir, timeoutMs: 5000 })
          return r.exitCode === 0 ? r.stdout.trim() : undefined
        },
      })
      if (hits.length) {
        const text = hits.map(h => h.text).join(' ')
        const w: Warning = { rule: hits[0].rule, text, at: await $.clock.now() }
        await update($, warnAtom, () => w)
        $.ui.toast(text)
        $.clock.after(WARNING_MS + 1000, () => void update($, warnAtom, cur => (cur && cur.at === w.at ? null : cur)))
      }
    } catch {
      // A guard that cannot judge says nothing: it only warns.
    }

    return next(e)
  })

  on('ui.render', { component: 'AbovePrompt' }, async ($, e, next) => {
    const text = bandText(await read($, bandAtom))
    const warn = warningText(await read($, warnAtom), await $.clock.now())
    if (e.props.hasSurvey || (!text && !warn)) return next(e)

    const { Box, Text } = $.ui.resolve(e)

    return (
      <Box>
        {text ? <Text dimColor>{text}</Text> : null}
        {warn ? <Text color="yellow">{(text ? '  ' : '') + '⚠ ' + warn}</Text> : null}
      </Box>
    )
  })
}
