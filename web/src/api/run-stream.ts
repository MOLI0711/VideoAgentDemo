import { APIError, request } from './client'
import type { Run } from './types'
import { acceptEvent, snapshotCursor, type EventState, type RunEvent } from '../stores/run-events'
import { isTerminal } from '../lib/timeline'

const eventNames = [
  'run.queued',
  'run.started',
  'run.retrying',
  'run.completed',
  'run.failed',
  'run.cancelled',
  'run.phase',
  'stage.started',
  'stage.completed',
  'tool.started',
  'tool.completed',
  'answer.completed',
  'answer.failed',
  'snapshot.required',
]

export function followRun(
  id: string,
  handlers: {
    snapshot: (run: Run) => void
    event: (event: RunEvent) => void
    connection: (message: string) => void
    error: (error: unknown) => void
  },
) {
  let stopped = false
  let source: EventSource | undefined
  let timer: ReturnType<typeof setTimeout> | undefined
  let retries = 0
  const state: EventState = { cursor: 0, attempt: 1, events: [] }
  const close = () => {
    source?.close()
    source = undefined
    if (timer) clearTimeout(timer)
  }
  const schedule = () => {
    close()
    if (stopped) return
    if (retries >= 8) {
      handlers.connection('连接未恢复。后台仍在执行，可点击“重新连接”。')
      return
    }
    const wait = Math.min(15_000, 1000 * 2 ** retries++)
    handlers.connection('连接中断，正在从持久快照恢复…')
    timer = setTimeout(() => {
      void loadSnapshot()
    }, wait)
  }
  const onEvent = (message: MessageEvent<string>) => {
    if (stopped) return
    if (message.type === 'snapshot.required') {
      close()
      void loadSnapshot()
      return
    }
    const seq = Number(message.lastEventId)
    let data: Record<string, unknown>
    try {
      data = JSON.parse(message.data) as Record<string, unknown>
    } catch {
      handlers.error(new Error('收到无法解析的运行事件，正在重新读取快照。'))
      close()
      void loadSnapshot()
      return
    }
    const event = { seq, type: message.type, data }
    if (!acceptEvent(state, event)) return
    handlers.event(event)
    if (
      [
        'answer.completed',
        'run.completed',
        'run.failed',
        'run.cancelled',
        'answer.failed',
      ].includes(event.type)
    ) {
      close()
      void loadSnapshot()
    }
  }
  const loadSnapshot = async () => {
    try {
      const run = await request<Run>(`/runs/${encodeURIComponent(id)}`)
      if (stopped) return
      handlers.snapshot(run)
      snapshotCursor(state, run.latest_event_seq)
      if (isTerminal(run.status)) {
        close()
        handlers.connection('状态已同步')
        return
      }
      close()
      source = new EventSource(
        `/v1/runs/${encodeURIComponent(id)}/events?after_seq=${state.cursor}`,
        { withCredentials: true },
      )
      source.onopen = () => {
        retries = 0
        handlers.connection('实时连接已建立')
      }
      source.onerror = schedule
      source.onmessage = onEvent
      eventNames.forEach((name) => source?.addEventListener(name, onEvent as EventListener))
    } catch (error) {
      if (stopped) return
      handlers.error(error)
      if (error instanceof APIError && [401, 403, 404].includes(error.status)) {
        close()
        handlers.connection('运行不可访问，请重新连接或开启新会话。')
        return
      }
      schedule()
    }
  }
  void loadSnapshot()
  return () => {
    stopped = true
    close()
  }
}
