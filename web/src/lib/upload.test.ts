import { afterEach, describe, expect, it, vi } from 'vitest'
import { fingerprint, putPart } from './upload'

class FakeXHR {
  static latest: FakeXHR
  status = 200
  timeout = 0
  etag: string | null = '"part-etag"'
  upload: { onprogress?: (event: { loaded: number }) => void } = {}
  onerror?: () => void
  ontimeout?: () => void
  onabort?: () => void
  onload?: () => void
  open = vi.fn()
  send = vi.fn()
  setRequestHeader = vi.fn()
  constructor() {
    FakeXHR.latest = this
  }
  abort() {
    this.onabort?.()
  }
  getResponseHeader() {
    return this.etag
  }
}
afterEach(() => vi.unstubAllGlobals())

describe('分片传输边界', () => {
  it('保留存储实际 ETag 并报告字节进度', async () => {
    vi.stubGlobal('XMLHttpRequest', FakeXHR)
    const progress = vi.fn()
    const upload = putPart(
      'http://storage.test/part',
      new Blob(['abc']),
      new AbortController().signal,
      progress,
    )
    FakeXHR.latest.onload?.()
    expect(await upload).toBe('"part-etag"')
    expect(progress).toHaveBeenLastCalledWith(3)
    expect(FakeXHR.latest.timeout).toBe(120_000)
  })
  it('没有 CORS 暴露的 ETag 时不能假定上传完成', async () => {
    vi.stubGlobal('XMLHttpRequest', FakeXHR)
    const upload = putPart(
      'http://storage.test/part',
      new Blob(['abc']),
      new AbortController().signal,
      vi.fn(),
    )
    FakeXHR.latest.etag = null
    FakeXHR.latest.onload?.()
    await expect(upload).rejects.toThrow('ETag')
  })
  it('暂停与超时均退出传输，保留服务端可续传状态', async () => {
    vi.stubGlobal('XMLHttpRequest', FakeXHR)
    const controller = new AbortController()
    const upload = putPart(
      'http://storage.test/part',
      new Blob(['abc']),
      controller.signal,
      vi.fn(),
    )
    controller.abort()
    await expect(upload).rejects.toMatchObject({ name: 'AbortError' })
    const timedOut = putPart(
      'http://storage.test/part',
      new Blob(['abc']),
      new AbortController().signal,
      vi.fn(),
    )
    FakeXHR.latest.ontimeout?.()
    await expect(timedOut).rejects.toThrow('超时')
  })
  it('拒绝上传 HTTP 失败', async () => {
    vi.stubGlobal('XMLHttpRequest', FakeXHR)
    const upload = putPart(
      'http://storage.test/part',
      new Blob(['abc']),
      new AbortController().signal,
      vi.fn(),
    )
    FakeXHR.latest.status = 403
    FakeXHR.latest.onload?.()
    await expect(upload).rejects.toThrow('403')
  })
  it('续传选择的文件指纹同时关联内容与元数据', async () => {
    const original = new File(['abc'], 'video.mp4', { lastModified: 100 })
    const replacement = new File(['xyz'], 'video.mp4', { lastModified: 100 })
    expect(await fingerprint(original)).toBe(await fingerprint(original))
    expect(await fingerprint(original)).not.toBe(await fingerprint(replacement))
  })
})
