import { reactive } from 'vue'
import type { AuthSession, User } from './types'

export const auth = reactive<{
  user: User | null
  csrf: string
  ready: boolean
  error: string
  notice: string
}>({
  user: null,
  csrf: '',
  ready: false,
  error: '',
  notice: '',
})

export class APIError extends Error {
  constructor(
    public status: number,
    public code: string,
    message: string,
    public details: Record<string, unknown> = {},
    public requestID?: string,
  ) {
    super(message)
    this.name = 'APIError'
  }
}

interface RequestOptions {
  method?: string
  body?: unknown
  key?: string
  signal?: AbortSignal
}
async function send<T>(path: string, options: RequestOptions = {}): Promise<T> {
  const method = options.method ?? 'GET'
  const headers: Record<string, string> = { Accept: 'application/json' }
  if (options.body !== undefined) headers['Content-Type'] = 'application/json'
  if (method !== 'GET' && method !== 'HEAD' && auth.csrf) headers['X-CSRF-Token'] = auth.csrf
  if (options.key) headers['Idempotency-Key'] = options.key
  const response = await fetch(`/v1${path}`, {
    method,
    headers,
    credentials: 'same-origin',
    body: options.body === undefined ? undefined : JSON.stringify(options.body),
    signal: options.signal,
  })
  const text = await response.text()
  let payload: unknown
  try {
    payload = text ? JSON.parse(text) : undefined
  } catch {
    payload = undefined
  }
  if (!response.ok) {
    const body = payload as
      | {
          error?: { code?: string; message?: string; details?: Record<string, unknown> }
          request_id?: string
        }
      | undefined
    throw new APIError(
      response.status,
      body?.error?.code ?? 'HTTP_ERROR',
      body?.error?.message ?? `服务返回 ${response.status}`,
      body?.error?.details,
      body?.request_id,
    )
  }
  return payload as T
}

function applySession(session: AuthSession) {
  auth.user = session.user
  auth.csrf = session.csrf_token
}
let sessionRecovery: Promise<void> | undefined

// Concurrent expired requests share recovery. Never replay writes under a new owner.
export function restoreSession(): Promise<void> {
  if (sessionRecovery) return sessionRecovery
  const previousOwner = auth.user?.id
  auth.ready = false
  auth.error = ''
  sessionRecovery = (async () => {
    try {
      let session: AuthSession
      try {
        session = await send<AuthSession>('/auth/session')
      } catch (failure) {
        if (!(failure instanceof APIError && failure.status === 401)) throw failure
        auth.csrf = ''
        session = await send<AuthSession>('/auth/guest', { method: 'POST' })
      }
      applySession(session)
      if (previousOwner && previousOwner !== session.user.id) {
        auth.notice = '已建立新的访客空间，请重新选择文件或发起操作。'
      }
    } catch (failure) {
      auth.user = null
      auth.csrf = ''
      auth.error = errorMessage(failure)
      throw failure
    } finally {
      auth.ready = true
      sessionRecovery = undefined
    }
  })()
  return sessionRecovery
}

export async function request<T>(path: string, options: RequestOptions = {}): Promise<T> {
  const owner = auth.user?.id
  const requireSameOwner = () => {
    if (owner && owner !== auth.user?.id) {
      throw new APIError(409, 'SESSION_CHANGED', '访客空间已更新，请在当前空间重新发起操作。')
    }
  }
  if (sessionRecovery) await sessionRecovery
  requireSameOwner()
  try {
    const result = await send<T>(path, options)
    requireSameOwner()
    return result
  } catch (failure) {
    if (
      failure instanceof APIError &&
      (failure.status === 401 ||
        (failure.status === 403 && failure.code.toUpperCase().includes('CSRF')))
    ) {
      auth.notice = ''
      await restoreSession()
      auth.notice ||= '会话已恢复，请重试刚才的操作。'
      throw new APIError(
        failure.status,
        'SESSION_REFRESHED',
        auth.notice,
        failure.details,
        failure.requestID,
      )
    }
    throw failure
  }
}
export function errorMessage(error: unknown): string {
  if (error instanceof APIError) {
    const messages: Record<string, string> = {
      ACTIVE_RUN_CONFLICT: '当前会话还有问答在执行，请查看该运行或等待完成。',
      IDEMPOTENCY_KEY_CONFLICT: '请求内容与之前提交不一致，请刷新状态后重试。',
      CSRF_INVALID: '会话验证已失效，请重新连接后重试。',
      CSRF_FAILED: '会话验证已失效，请重新连接后重试。',
      VERSION_CONFLICT: '版本已发生变化，请刷新后选择可用版本。',
    }
    const message =
      messages[error.code] ??
      (error.status === 403 && error.code.toUpperCase().includes('CSRF')
        ? '会话验证无效，请重新连接后重试。'
        : error.status === 429
          ? '当前请求或任务已达到配额，请稍后重试。'
          : error.message)
    return `${message}${error.requestID ? `（请求 ${error.requestID}）` : ''}`
  }
  if (error instanceof Error)
    return error.name === 'TypeError'
      ? '网络连接失败。可重试原操作，后台任务不会因此取消。'
      : error.message
  return '操作失败，请稍后重试。'
}

// Keep the key across an uncertain network result. A changed body gets a new key.
export class SubmissionKey {
  private body = ''
  private key = ''
  for(body: unknown) {
    const encoded = JSON.stringify(body)
    if (encoded !== this.body || !this.key) {
      this.body = encoded
      this.key = crypto.randomUUID()
    }
    return this.key
  }
  clear() {
    this.body = ''
    this.key = ''
  }
}
