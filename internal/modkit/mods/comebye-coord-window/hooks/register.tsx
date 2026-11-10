import type { Register, EngineInterface as Engine } from 'claude-code'

import { MIN_VERSION, trimWindow, turnCount, versionOk } from './window'

type State = {
  // Why the mod does nothing this session, or '' when it is working.
  off: string
  // Set by turn.complete for the one compaction it asks for.
  armed: boolean
  // Set by our session.compact hook when it answered.
  answered: boolean
  // Set when our session.compact hook ran at all.
  ran: boolean
  lastTrimAt: number
  trims: number
  keep: number
  trimTokens: number
  cooldownMs: number
}

async function isCoordinator($: Engine): Promise<boolean> {
  try {
    const r = await $.process.run(['comebye', 'mod', 'band', '--json'], { timeoutMs: 8000 })
    return r.exitCode === 0 && JSON.parse(r.stdout).coordinator === true
  } catch {
    return false
  }
}

async function maybeTrim($: Engine, st: State) {
  const now = await $.clock.now()
  if (st.off || now - st.lastTrimAt < st.cooldownMs) return
  const usage = await $.session.usage()
  if ((usage.context.tokens ?? 0) <= st.trimTokens) return
  const before = (await $.session.messages()).length
  if (before < 2) return
  st.armed = true
  st.answered = false
  st.ran = false
  st.lastTrimAt = now
  let done: Awaited<ReturnType<Engine['session']['compact']>> | undefined
  try {
    done = await $.session.compact()
  } finally {
    st.armed = false
  }
  if (done && 'messages' in done && done.messages && st.answered) {
    st.trims += 1
    $.ui.toast(`Window trimmed in place: ${before} → ${done.messages.length} messages`)
  } else if (!st.ran) {
    // The engine compacted by itself, or a guard above skipped our hook:
    // a managed profile (sec-default) does that. Stop trying.
    st.off = 'another layer answers compaction (managed profile?)'
    $.ui.toast('Comebye window: compaction was not mine, so the mod is off for this session')
  }
}

export const register: Register = (on, options) => {
  const st: State = {
    off: '',
    armed: false,
    answered: false,
    ran: false,
    lastTrimAt: 0,
    trims: 0,
    keep: Math.max(1, Number(options?.keep_turns ?? 10)),
    trimTokens: Math.max(20000, Number(options?.trim_tokens ?? 120000)),
    cooldownMs: Math.max(1, Number(options?.cooldown_minutes ?? 20)) * 60_000,
  }

  on('session.start', async ($, e, next) => {
    await $.command.register({ name: 'window', description: 'Comebye coordinator window: status of the in-place trim' })
    const { version } = await $.session.version()
    if (!versionOk(version)) st.off = `needs Claude Code ${MIN_VERSION.join('.')} or newer (this is ${version})`
    else if (!(await isCoordinator($))) st.off = 'not a coordinator'

    return next(e)
  })

  on('turn.complete', async ($, e, next) => {
    if (!e.agentId) {
      try {
        await maybeTrim($, st)
      } catch {
        // Trimming is best effort: the next turn tries again after the cooldown.
      }
    }

    return next(e)
  })

  // Only the compaction this mod asked for (trigger 'plugin'); a person's
  // /compact and the engine's own auto-compact are never touched.
  on('session.compact', { trigger: 'plugin' }, async ($, e, next) => {
    if (!st.armed || st.off) return next(e)
    st.ran = true
    const r = await $.process.run(['comebye', 'coordinator', 'digest'], { timeoutMs: 20_000 })
    const digest = r.stdout.trim()
    if (r.exitCode !== 0 || !digest.startsWith('comebye-digest:v1')) return { skip: 'no digest' }
    const messages = trimWindow(e.messages ?? [], digest, st.keep)
    if (!messages) return { skip: 'nothing older to drop' }
    st.answered = true

    return { messages }
  })

  on('command.run', { command: 'window' }, async $ => {
    const usage = await $.session.usage()
    const n = turnCount(await $.session.messages())
    const text = [
      st.off ? `off: ${st.off}` : 'on',
      `context ${usage.context.tokens ?? '?'} tokens; ${n} turns`,
      `trims above ${st.trimTokens} tokens, keeps ${st.keep} turns; ${st.trims} trims so far`,
    ].join('\n')

    return { text, isTextDisplayOnly: true }
  })
}
