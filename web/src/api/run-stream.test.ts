import { afterEach, describe, expect, it, vi } from 'vitest'
import { followRun } from './run-stream'
import type { Run } from './types'

class FakeEventSource extends EventTarget {
  static instances: FakeEventSource[] = []
  onopen?: () => void
  onerror?: () => void
  onmessage?: (event: MessageEvent<string>) => void
  closed = false
  constructor(public url: string) {
    super()
    FakeEventSource.instances.push(this)
  }
  close() {
    this.closed = true
  }
  emit(type: string, seq: number, data: Record<string, unknown> = {}) {
    this.dispatchEvent(
      new MessageEvent(type, { data: JSON.stringify(data), lastEventId: String(seq) }),
    )
  }
}
const snapshot = (status: string, seq: number): Run => ({
  run_id: 'run-1',
  content_version: 'version-1',
  status,
  latest_event_seq: seq,
})
const tick = () => new Promise<void>((resolve) => setTimeout(resolve, 0))

afterEach(() => {
  vi.unstubAllGlobals()
  FakeEventSource.instances = []
})

describe('SSE 快照与重连协议', () => {
  it('先读快照、只订阅后续事件，终态再次取快照并关闭连接', async () => {
    const fetch = vi
      .fn()
      .mockResolvedValueOnce(Response.json(snapshot('RUNNING', 4)))
      .mockResolvedValueOnce(Response.json(snapshot('SUCCEEDED', 7)))
    vi.stubGlobal('fetch', fetch)
    vi.stubGlobal('EventSource', FakeEventSource)
    const observed = { snapshot: vi.fn(), event: vi.fn(), connection: vi.fn(), error: vi.fn() }
    const stop = followRun('run-1', observed)
    await tick()
    const source = FakeEventSource.instances[0]
    expect(source.url).toBe('/v1/runs/run-1/events?after_seq=4')
    source.emit('tool.completed', 5)
    source.emit('tool.completed', 5)
    expect(observed.event).toHaveBeenCalledTimes(1)
    source.emit('answer.completed', 7)
    await tick()
    expect(fetch).toHaveBeenCalledTimes(2)
    expect(observed.snapshot.mock.calls[1][0].status).toBe('SUCCEEDED')
    expect(source.closed).toBe(true)
    expect(FakeEventSource.instances).toHaveLength(1)
    stop()
  })
  it('游标过期读取新快照，已完成运行不再建立流', async () => {
    vi.stubGlobal(
      'fetch',
      vi
        .fn()
        .mockResolvedValueOnce(Response.json(snapshot('RUNNING', 2)))
        .mockResolvedValueOnce(Response.json(snapshot('SUCCEEDED', 20))),
    )
    vi.stubGlobal('EventSource', FakeEventSource)
    const observed = { snapshot: vi.fn(), event: vi.fn(), connection: vi.fn(), error: vi.fn() }
    const stop = followRun('run-1', observed)
    await tick()
    FakeEventSource.instances[0].emit('snapshot.required', 0)
    await tick()
    expect(observed.snapshot.mock.calls[1][0].latest_event_seq).toBe(20)
    expect(FakeEventSource.instances).toHaveLength(1)
    expect(observed.event).not.toHaveBeenCalled()
    stop()
  })
  it('进入已完成页面时仅显示快照，不创建 EventSource', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(Response.json(snapshot('FAILED', 11))))
    vi.stubGlobal('EventSource', FakeEventSource)
    const stop = followRun('run-1', {
      snapshot: vi.fn(),
      event: vi.fn(),
      connection: vi.fn(),
      error: vi.fn(),
    })
    await tick()
    expect(FakeEventSource.instances).toHaveLength(0)
    stop()
  })
})
