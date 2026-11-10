import type { SessionMessage } from 'claude-code'

export const MIN_VERSION = [2, 1, 321]

export function versionOk(v: string): boolean {
  const m = /^(\d+)\.(\d+)\.(\d+)/.exec(v)
  if (!m) return false
  for (let i = 0; i < 3; i++) {
    const n = Number(m[i + 1])
    if (n !== MIN_VERSION[i]) return n > MIN_VERSION[i]
  }
  return true
}

// A turn starts at a user message that is a prompt, not a tool result.
const isPrompt = (m: SessionMessage) =>
  m.role === 'user' && !(m.toolResults && m.toolResults.length) && m.text !== ''

export function turnCount(msgs: readonly SessionMessage[]): number {
  return msgs.filter(isPrompt).length
}

// The digest, then the last `keep` turns. Cutting at a prompt keeps every
// tool_use with its tool_result. Null when there is nothing older to drop.
export function trimWindow(
  msgs: readonly SessionMessage[],
  digest: string,
  keep: number,
): SessionMessage[] | null {
  const starts: number[] = []
  msgs.forEach((m, i) => {
    if (isPrompt(m)) starts.push(i)
  })
  if (keep < 1 || starts.length <= keep) return null
  const kept = msgs.slice(starts[starts.length - keep])
  return [
    { role: 'user', text: digest, toolUses: [] },
    { role: 'assistant', text: 'Context window trimmed in place. The digest above is current.', toolUses: [] },
    ...kept,
  ]
}
