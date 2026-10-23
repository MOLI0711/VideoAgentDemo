import type { CompletedPart } from '../api/types'

export const PART_BYTES = 8 * 1024 * 1024
export const MAX_BYTES = 2 * 1024 * 1024 * 1024
export interface UploadCheckpoint {
  uploadID: string
  videoID: string
  filename: string
  size: number
  lastModified: number
  fingerprint: string
  partBytes: number
  completeParts?: CompletedPart[]
}
export function partPlan(size: number, partBytes = PART_BYTES) {
  if (!Number.isSafeInteger(size) || size <= 0 || size > MAX_BYTES)
    throw new Error('文件大小需大于 0 且不超过 2 GiB。')
  if (!Number.isSafeInteger(partBytes) || partBytes < 5 * 1024 * 1024)
    throw new Error('服务端返回了无效的分片大小。')
  return Array.from({ length: Math.ceil(size / partBytes) }, (_, i) => ({
    part_number: i + 1,
    start: i * partBytes,
    end: Math.min(size, (i + 1) * partBytes),
  }))
}
export function pendingParts(size: number, partBytes: number, completed: CompletedPart[]) {
  const plan = partPlan(size, partBytes)
  const valid = new Set(
    completed
      .filter((part) => {
        const range = plan[part.part_number - 1]
        return (
          range &&
          part.etag &&
          (part.size_bytes === undefined || part.size_bytes === range.end - range.start)
        )
      })
      .map((part) => part.part_number),
  )
  return plan.filter((part) => !valid.has(part.part_number))
}
export function uploadedBytes(size: number, partBytes: number, completed: CompletedPart[]): number {
  const remaining = pendingParts(size, partBytes, completed).reduce(
    (sum, part) => sum + part.end - part.start,
    0,
  )
  return size - remaining
}
export async function fingerprint(file: File): Promise<string> {
  // Detect accidental file replacement without loading a 2 GiB file into memory.
  // The server still validates the actual multipart manifest and media contents.
  const edge = 1024 * 1024
  const bytes = await new Blob([
    file.slice(0, edge),
    file.slice(Math.max(0, file.size - edge)),
    `${file.name}:${file.size}:${file.lastModified}`,
  ]).arrayBuffer()
  const hash = await crypto.subtle.digest('SHA-256', bytes)
  return Array.from(new Uint8Array(hash), (value) => value.toString(16).padStart(2, '0')).join('')
}
export function putPart(
  url: string,
  blob: Blob,
  signal: AbortSignal,
  progress: (bytes: number) => void,
  headers: Record<string, string> = {},
): Promise<string> {
  return new Promise((resolve, reject) => {
    const xhr = new XMLHttpRequest()
    const abort = () => xhr.abort()
    signal.addEventListener('abort', abort, { once: true })
    const finish = () => signal.removeEventListener('abort', abort)
    xhr.open('PUT', url)
    xhr.timeout = 120_000
    Object.entries(headers).forEach(([name, value]) => xhr.setRequestHeader(name, value))
    xhr.upload.onprogress = (event) => progress(event.loaded)
    xhr.onerror = () => {
      finish()
      reject(new Error('分片网络传输失败，可选择原文件继续上传。'))
    }
    xhr.ontimeout = () => {
      finish()
      reject(new Error('分片传输超时，请检查网络后继续上传。'))
    }
    xhr.onabort = () => {
      finish()
      reject(new DOMException('上传已暂停', 'AbortError'))
    }
    xhr.onload = () => {
      finish()
      if (xhr.status < 200 || xhr.status >= 300) {
        reject(new Error(`分片上传失败（HTTP ${xhr.status}），请重新签名后续传。`))
        return
      }
      const etag = xhr.getResponseHeader('ETag')
      if (!etag) {
        reject(
          new Error(
            '存储未向浏览器暴露 ETag，请检查 bucket 的 CORS 配置；已上传分片可从服务端找回。',
          ),
        )
        return
      }
      progress(blob.size)
      resolve(etag)
    }
    if (signal.aborted) {
      finish()
      reject(new DOMException('上传已暂停', 'AbortError'))
      return
    }
    xhr.send(blob)
  })
}
