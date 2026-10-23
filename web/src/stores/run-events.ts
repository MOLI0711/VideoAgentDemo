export interface RunEvent {
  seq: number
  type: string
  data: Record<string, unknown>
}
export interface EventState {
  cursor: number
  attempt: number
  events: RunEvent[]
}
export function acceptEvent(state: EventState, event: RunEvent): boolean {
  if (!Number.isSafeInteger(event.seq) || event.seq <= state.cursor) return false
  state.cursor = event.seq
  if (event.type === 'run.retrying') {
    state.attempt++
    state.events = []
  }
  state.events.push(event)
  if (state.events.length > 60) state.events.shift()
  return true
}
export function snapshotCursor(state: EventState, seq: number) {
  state.cursor = Math.max(0, Number.isSafeInteger(seq) ? seq : 0)
}
