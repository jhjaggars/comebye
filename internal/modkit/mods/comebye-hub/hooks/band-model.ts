import type { Band, Warning } from '../types'

// What the Go side (`comebye mod band --json`) prints, snake_case.
export type RawBand = {
  coordinator?: boolean
  todos?: number
  first_todo?: string
  queued?: number
  open_todos?: number
  headline?: string
  handoff?: {
    id: string
    status: string
    progress?: string
    phase?: string
    landing?: string
    ci?: string
  }
}

export function toBand(raw: RawBand): Band {
  return {
    coordinator: raw.coordinator === true,
    todos: raw.todos ?? 0,
    firstTodo: raw.first_todo,
    queued: raw.queued ?? 0,
    openTodos: raw.open_todos,
    headline: raw.headline,
    handoff: raw.handoff,
  }
}

const plural = (n: number, one: string, many: string) =>
  `${n} ${n === 1 ? one : many}`

// The handoff's landing or CI state, or '' when there is nothing to say.
export function handoffText(h: Band['handoff']): string {
  if (!h) return ''
  if (h.ci === 'failure') return 'CI failed'
  if (h.ci === 'running') return 'landed, CI running'
  if (h.ci === 'success' && h.landing === 'landed') return 'landed, CI passed'
  if (h.phase && h.status === 'running' && h.progress) return `landing: ${h.progress}`
  if (h.landing === 'failed') return 'landing check failed'
  return ''
}

// The band's one line, or null when nothing is pending: the band is then
// not drawn at all.
export function bandText(b: Band | null): string | null {
  if (!b) return null
  const parts: string[] = []
  if (b.todos > 0) parts.push(plural(b.todos, 'to-do waits', 'to-dos wait') + ' on you')
  if (b.queued > 0) parts.push(plural(b.queued, 'notice', 'notices') + ' queued')
  const h = handoffText(b.handoff)
  if (h) parts.push(h)
  if (b.coordinator) {
    if ((b.openTodos ?? 0) > 0 && b.todos === 0)
      parts.push(plural(b.openTodos ?? 0, 'open to-do', 'open to-dos'))
    if (b.headline) parts.push(b.headline)
  }
  return parts.length ? 'Comebye: ' + parts.join(' · ') : null
}

export const WARNING_MS = 120_000

export function warningText(w: Warning | null, now: number): string | null {
  if (!w || now - w.at > WARNING_MS) return null
  return w.text
}
