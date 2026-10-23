export function timestamp(ms: number): string {
  const total = Math.max(0, Math.floor(ms / 1000))
  const seconds = String(total % 60).padStart(2, '0')
  const minutes = Math.floor(total / 60)
  return minutes >= 60
    ? `${Math.floor(minutes / 60)}:${String(minutes % 60).padStart(2, '0')}:${seconds}`
    : `${String(minutes).padStart(2, '0')}:${seconds}`
}

export function seekSeconds(ms: number, duration: number): number {
  if (!Number.isFinite(ms) || ms < 0) return 0
  const seconds = ms / 1000
  return Number.isFinite(duration) && duration > 0
    ? Math.min(seconds, Math.max(0, duration - 0.01))
    : seconds
}

export function isTerminal(status: string): boolean {
  return ['SUCCEEDED', 'FAILED', 'CANCELLED', 'SKIPPED'].includes(status)
}

const labels: Record<string, string> = {
  UPLOADING: '上传中',
  QUEUED: '等待接管',
  READY: '已就绪',
  PROCESSING: '处理中',
  RUNNING: '执行中',
  WAITING_EXTERNAL: '等待外部服务',
  RETRY_WAIT: '等待重试',
  SUCCEEDED: '已完成',
  FAILED: '失败',
  CANCELLED: '已取消',
  CANCEL_REQUESTED: '取消中',
  BLOCKED: '等待前置阶段',
  SKIPPED: '已跳过',
  DRAFT: '草稿',
  PUBLISHED: '已发布',
  TEXT_ONLY: '仅文本',
  SUBMISSION_UNKNOWN: '提交结果待核对',
}
export function statusLabel(status: string) {
  return labels[status] ?? status
}
