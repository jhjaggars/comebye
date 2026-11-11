// Splits what follows a slash command into arguments, with 'single' and
// "double" quotes and backslash escapes, as a person typing it expects. No
// shell is involved: the words go to the comebye program as they are.
export function splitArgs(s: string): string[] {
  const out: string[] = []
  let cur = ''
  let has = false
  let quote: '"' | "'" | null = null
  for (let i = 0; i < s.length; i++) {
    const c = s[i]
    if (quote) {
      if (c === quote) quote = null
      else if (c === '\\' && quote === '"' && i + 1 < s.length && '"\\'.includes(s[i + 1])) cur += s[++i]
      else cur += c
    } else if (c === '"' || c === "'") {
      quote = c
      has = true
    } else if (c === '\\' && i + 1 < s.length) {
      cur += s[++i]
      has = true
    } else if (/\s/.test(c)) {
      if (has || cur) out.push(cur)
      cur = ''
      has = false
    } else {
      cur += c
      has = true
    }
  }
  if (has || cur) out.push(cur)
  return out
}

// The comebye argv a slash command stands for.
export const COMMANDS: Record<string, { description: string; hint?: string; argv: (args: string[]) => string[] }> = {
  todo: {
    description: 'Comebye to-dos: list, show ID, add …, from-last (send the questions in my last message to the To do list)',
    hint: '[list | show ID | from-last | add QUESTION …]',
    argv: a => ['todo', ...(a.length ? a : ['list'])],
  },
  handoff: {
    description: 'Hand work to a new worker (comebye handoff)',
    hint: 'PROJECT … (see comebye handoff)',
    argv: a => ['handoff', ...a],
  },
  show: {
    description: 'Put a file in the Doc panel (comebye show)',
    hint: 'FILE [--section HEADING] | --clear',
    argv: a => ['show', ...a],
  },
  queue: {
    description: 'Prompts and notices queued for agents',
    hint: '[AGENT]',
    argv: a => ['queue', ...a],
  },
  ledger: {
    description: "The hub's handoffs",
    hint: '[--active]',
    argv: a => ['ledger', ...(a.length ? a : ['--active'])],
  },
  brief: {
    description: "The coordinator's briefing",
    argv: a => ['brief', ...(a.length ? a : ['get'])],
  },
}
