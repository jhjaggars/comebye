export type Band = {
  coordinator: boolean
  todos: number
  firstTodo?: string
  queued: number
  openTodos?: number
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

export type Warning = { rule: string; text: string; at: number }

declare module 'claude-code' {
  interface PluginState {
    'comebye-hub': { band: Band | null; warning: Warning | null }
  }
}
