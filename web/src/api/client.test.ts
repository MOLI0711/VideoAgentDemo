import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { auth, request, restoreSession } from './client'
import type { AuthSession } from './types'

const session = (id = 'guest-1', isGuest = true): AuthSession => ({
  user: { id, username: id, is_guest: isGuest },
  csrf_token: `csrf-${id}`,
})
const failure = (status: number, code: string) =>
  Response.json({ error: { code, message: 'test service response' } }, { status })

beforeEach(() => {
  Object.assign(auth, { user: null, csrf: '', ready: false, error: '', notice: '' })
})
afterEach(() => vi.unstubAllGlobals())

describe('访客空间连接', () => {
  it('有效旧会话保持原身份，不创建访客或发送密码', async () => {
    const fetch = vi.fn().mockResolvedValue(Response.json(session('existing-user', false)))
    vi.stubGlobal('fetch', fetch)
    await restoreSession()
    expect(fetch).toHaveBeenCalledTimes(1)
    expect(fetch.mock.calls[0][0]).toBe('/v1/auth/session')
    expect(auth.user?.id).toBe('existing-user')
    expect(auth.user?.is_guest).toBe(false)
    expect(auth.csrf).toBe('csrf-existing-user')
    expect(auth.ready).toBe(true)
  })

  it('401 自动创建访客，无请求体且并发恢复只建立一次会话', async () => {
    const fetch = vi
      .fn()
      .mockResolvedValueOnce(failure(401, 'UNAUTHENTICATED'))
      .mockResolvedValueOnce(Response.json(session()))
    vi.stubGlobal('fetch', fetch)
    await Promise.all([restoreSession(), restoreSession(), restoreSession()])
    expect(fetch).toHaveBeenCalledTimes(2)
    expect(fetch.mock.calls[1][0]).toBe('/v1/auth/guest')
    expect(fetch.mock.calls[1][1]).toMatchObject({
      method: 'POST',
      credentials: 'same-origin',
      body: undefined,
    })
    expect(fetch.mock.calls[1][1].headers).not.toHaveProperty('X-CSRF-Token')
    expect(auth.user?.is_guest).toBe(true)
    expect(auth.error).toBe('')
  })

  it('服务故障不创建新访客、不循环请求，重连可恢复', async () => {
    const fetch = vi
      .fn()
      .mockResolvedValueOnce(failure(503, 'RATE_LIMIT_UNAVAILABLE'))
      .mockResolvedValueOnce(Response.json(session()))
    vi.stubGlobal('fetch', fetch)
    await expect(restoreSession()).rejects.toMatchObject({ status: 503 })
    expect(fetch).toHaveBeenCalledTimes(1)
    expect(auth.ready).toBe(true)
    expect(auth.user).toBeNull()
    expect(auth.error).not.toBe('')
    await restoreSession()
    expect(auth.user?.id).toBe('guest-1')
    expect(auth.error).toBe('')
  })

  it('访客接口拒绝时停止请求，显示可重试状态', async () => {
    const fetch = vi
      .fn()
      .mockResolvedValueOnce(failure(401, 'UNAUTHENTICATED'))
      .mockResolvedValueOnce(failure(429, 'RATE_LIMITED'))
    vi.stubGlobal('fetch', fetch)
    await expect(restoreSession()).rejects.toMatchObject({ status: 429 })
    expect(fetch).toHaveBeenCalledTimes(2)
    expect(auth.user).toBeNull()
    expect(auth.ready).toBe(true)
    expect(auth.error).toContain('配额')
  })

  it('失效写请求恢复访客后不自动重放，避免跨身份创建重复资源', async () => {
    auth.user = session('old-guest').user
    auth.csrf = 'old-csrf'
    const fetch = vi
      .fn()
      .mockResolvedValueOnce(failure(401, 'UNAUTHENTICATED'))
      .mockResolvedValueOnce(failure(401, 'UNAUTHENTICATED'))
      .mockResolvedValueOnce(Response.json(session('new-guest')))
    vi.stubGlobal('fetch', fetch)
    await expect(
      request('/uploads', { method: 'POST', body: { filename: 'test.mp4' }, key: 'upload-key' }),
    ).rejects.toMatchObject({ status: 401, code: 'SESSION_REFRESHED' })
    expect(fetch.mock.calls.map(([path]) => path)).toEqual([
      '/v1/uploads',
      '/v1/auth/session',
      '/v1/auth/guest',
    ])
    expect(fetch.mock.calls[0][1].headers).toMatchObject({
      'X-CSRF-Token': 'old-csrf',
      'Idempotency-Key': 'upload-key',
    })
    expect(auth.user?.id).toBe('new-guest')
    expect(auth.notice).toContain('新的访客空间')
  })

  it('CSRF 拒绝恢复原会话 token，不另建访客或重放写请求', async () => {
    auth.user = session().user
    auth.csrf = 'stale-csrf'
    const fetch = vi
      .fn()
      .mockResolvedValueOnce(failure(403, 'CSRF_INVALID'))
      .mockResolvedValueOnce(Response.json(session()))
    vi.stubGlobal('fetch', fetch)
    await expect(
      request('/agents', { method: 'POST', body: { name: 'test' } }),
    ).rejects.toMatchObject({ status: 403, code: 'SESSION_REFRESHED' })
    expect(fetch.mock.calls.map(([path]) => path)).toEqual(['/v1/agents', '/v1/auth/session'])
    expect(auth.csrf).toBe('csrf-guest-1')
    expect(auth.notice).toContain('重试')
  })

  it('恢复期间排队的写请求不能以新访客身份继续发送', async () => {
    auth.user = session('old-guest').user
    let release!: (response: Response) => void
    const fetch = vi
      .fn()
      .mockImplementationOnce(
        () =>
          new Promise<Response>((resolve) => {
            release = resolve
          }),
      )
      .mockResolvedValueOnce(Response.json(session('new-guest')))
    vi.stubGlobal('fetch', fetch)
    const recovery = restoreSession()
    const pending = request('/agents', { method: 'POST', body: { name: 'old request' } })
    const rejected = expect(pending).rejects.toMatchObject({ code: 'SESSION_CHANGED' })
    release(failure(401, 'UNAUTHENTICATED'))
    await recovery
    await rejected
    expect(fetch.mock.calls.map(([path]) => path)).toEqual(['/v1/auth/session', '/v1/auth/guest'])
  })

  it('身份更新后丢弃旧请求的成功响应，避免将旧资料写入新空间状态', async () => {
    auth.user = session('old-guest').user
    let release!: (response: Response) => void
    vi.stubGlobal(
      'fetch',
      vi.fn(
        () =>
          new Promise<Response>((resolve) => {
            release = resolve
          }),
      ),
    )
    const pending = request('/videos')
    const rejected = expect(pending).rejects.toMatchObject({ code: 'SESSION_CHANGED' })
    auth.user = session('new-guest').user
    release(Response.json({ items: [{ id: 'old-video' }] }))
    await rejected
  })
})
