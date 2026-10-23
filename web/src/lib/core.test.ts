import { describe, expect, it } from 'vitest'
import { acceptEvent, snapshotCursor, type EventState } from '../stores/run-events'
import { PART_BYTES, partPlan, pendingParts, uploadedBytes } from './upload'
import { seekSeconds, timestamp } from './timeline'
import { SubmissionKey } from '../api/client'

describe('持久 SSE 事件', () => {
  it('快照后的重复与过期事件不重复应用', () => {
    const state: EventState = { cursor: 0, attempt: 1, events: [] }
    snapshotCursor(state, 5)
    expect(acceptEvent(state, { seq: 5, type: 'tool.completed', data: {} })).toBe(false)
    expect(acceptEvent(state, { seq: 6, type: 'tool.completed', data: {} })).toBe(true)
    expect(acceptEvent(state, { seq: 6, type: 'tool.completed', data: {} })).toBe(false)
    expect(state.events).toHaveLength(1)
  })
  it('新 attempt 清空旧过程而且保留递增游标', () => {
    const state: EventState = {
      cursor: 7,
      attempt: 1,
      events: [{ seq: 7, type: 'tool.started', data: {} }],
    }
    acceptEvent(state, { seq: 8, type: 'run.retrying', data: {} })
    expect(state.attempt).toBe(2)
    expect(state.events.map((event) => event.seq)).toEqual([8])
    expect(acceptEvent(state, { seq: Number.NaN, type: 'answer.completed', data: {} })).toBe(false)
  })
})

describe('上传续传', () => {
  it('按真实文件大小规划末片，已完成分片不重传', () => {
    const size = PART_BYTES * 2 + 15
    expect(partPlan(size).map((part) => part.end - part.start)).toEqual([
      PART_BYTES,
      PART_BYTES,
      15,
    ])
    const completed = [
      { part_number: 1, etag: 'a', size_bytes: PART_BYTES },
      { part_number: 3, etag: 'b', size_bytes: 15 },
    ]
    expect(pendingParts(size, PART_BYTES, completed).map((part) => part.part_number)).toEqual([2])
    expect(uploadedBytes(size, PART_BYTES, completed)).toBe(PART_BYTES + 15)
  })
  it('拒绝将大小不正确、重复和越界分片计算为进度', () => {
    const completed = [
      { part_number: 1, etag: 'a', size_bytes: 3 },
      { part_number: 2, etag: 'b', size_bytes: PART_BYTES },
      { part_number: 2, etag: 'b', size_bytes: PART_BYTES },
      { part_number: 99, etag: 'c' },
    ]
    expect(uploadedBytes(PART_BYTES * 2, PART_BYTES, completed)).toBe(PART_BYTES)
    expect(
      pendingParts(PART_BYTES * 2, PART_BYTES, completed).map((part) => part.part_number),
    ).toEqual([1])
    expect(() => partPlan(0)).toThrow()
    expect(() => partPlan(2 ** 31 + 1)).toThrow()
  })
})

describe('字幕和证据定位', () => {
  it('毫秒转换为秒，限制在可播放时长内', () => {
    expect(seekSeconds(12345, 60)).toBe(12.345)
    expect(seekSeconds(12345, Number.NaN)).toBe(12.345)
    expect(seekSeconds(90000, 60)).toBeCloseTo(59.99)
    expect(seekSeconds(-100, 60)).toBe(0)
    expect(seekSeconds(Number.NaN, 60)).toBe(0)
    expect(timestamp(3_723_456)).toBe('1:02:03')
  })
})

describe('结果不明的提交', () => {
  it('同请求重试沿用幂等键，改问题或确认成功后生成新键', () => {
    const key = new SubmissionKey()
    const first = key.for({ question: '原问题' })
    expect(key.for({ question: '原问题' })).toBe(first)
    const changed = key.for({ question: '新问题' })
    expect(changed).not.toBe(first)
    key.clear()
    expect(key.for({ question: '新问题' })).not.toBe(changed)
  })
})
