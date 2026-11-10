// Warn-only intent rules the sandbox cannot see. Nothing here blocks: a hit
// is a line in the band and a toast. The match is on the command's words with
// quoted text removed, so a commit message that mentions `git add -A` is quiet.
export type Hit = { rule: string; text: string }

export type GuardContext = {
  ownOwners: string[]
  // The URL of a git remote, or undefined when it is unknown.
  remoteUrl: (remote: string, dir?: string) => Promise<string | undefined>
}

const stripQuoted = (s: string) =>
  s.replace(/"(?:[^"\\]|\\.)*"/g, '""').replace(/'[^']*'/g, "''")

function segments(command: string): string[][] {
  return stripQuoted(command)
    .split(/&&|\|\||;|\||\n|\(|\)|`|\$\(/)
    .map(s => s.trim().split(/\s+/).filter(Boolean))
    .filter(w => w.length > 0)
}

// `git [-C dir] [-c k=v …] <sub> args`; undefined when the words are not git.
function gitParts(w: string[]): { sub: string; args: string[]; dir?: string } | undefined {
  let i = w[0] === 'sudo' || w[0] === 'env' ? 1 : 0
  if (w[i] !== 'git') return undefined
  i++
  let dir: string | undefined
  while (i < w.length && w[i].startsWith('-')) {
    if (w[i] === '-C' && i + 1 < w.length) dir = w[i + 1]
    i += w[i] === '-C' || w[i] === '-c' ? 2 : 1
  }
  if (i >= w.length) return undefined
  return { sub: w[i], args: w.slice(i + 1), dir }
}

export function ownerOf(url: string): string | undefined {
  const m = /github\.com[:/]+([^/\s]+)\/[^/\s]+/i.exec(url)
  return m ? m[1].toLowerCase() : undefined
}

export async function check(command: string, ctx: GuardContext): Promise<Hit[]> {
  const hits: Hit[] = []
  for (const w of segments(command)) {
    const g = gitParts(w)
    if (g?.sub === 'add') {
      const broad = g.args.some(
        a => a === '.' || a === '--all' || a === '*' || (/^-[A-Za-z]+$/.test(a) && a.includes('A')),
      )
      if (broad)
        hits.push({
          rule: 'git-add-all',
          text: 'git add -A / . stages everything, secrets and scratch files included: add files by name.',
        })
    }
    if (g?.sub === 'push') {
      const flags = g.args.filter(a => a.startsWith('-'))
      const isForce =
        flags.some(f => f === '--force' || f.startsWith('--force-with-lease') || f === '--force-if-includes' || f === '--mirror') ||
        flags.some(f => /^-[A-Za-z]*f[A-Za-z]*$/.test(f)) ||
        g.args.some(a => !a.startsWith('-') && a.startsWith('+'))
      if (isForce)
        hits.push({ rule: 'force-push', text: 'This push forces: it can rewrite history others have. Is that what was asked?' })
      const pos = g.args.filter(a => !a.startsWith('-'))
      const remote = pos[0] ?? 'origin'
      const url = /^[a-z+]+:\/\/|^git@/.test(remote) ? remote : await ctx.remoteUrl(remote, g.dir)
      const owner = url ? ownerOf(url) : undefined
      const isUpstream = ctx.ownOwners.length
        ? owner !== undefined && !ctx.ownOwners.includes(owner)
        : remote === 'upstream'
      if (isUpstream)
        hits.push({
          rule: 'upstream-push',
          text: `This pushes to ${owner ?? remote}, which is not yours: push to your fork and open a PR.`,
        })
    }
    const gh = w[0] === 'gh' ? w : undefined
    if (gh && gh[1] === 'pr' && (gh[2] === 'comment' || gh[2] === 'merge'))
      hits.push({
        rule: 'gh-pr-write',
        text: `gh pr ${gh[2]} writes to a shared PR: it needs the user's OK (an answered to-do).`,
      })
  }
  return hits
}
