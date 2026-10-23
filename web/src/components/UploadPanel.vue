<script setup lang="ts">
import { computed, onBeforeUnmount, ref } from 'vue'
import { APIError, auth, errorMessage, request, SubmissionKey } from '../api/client'
import type { CompletedPart, List, PartURL, UploadCreated } from '../api/types'
import {
  fingerprint,
  MAX_BYTES,
  PART_BYTES,
  partPlan,
  pendingParts,
  putPart,
  uploadedBytes,
  type UploadCheckpoint,
} from '../lib/upload'

const emit = defineEmits<{ completed: [videoID: string] }>()
const storageKey = `moli-upload:${auth.user?.id}`
const file = ref<File>()
const title = ref('')
const checkpoint = ref<UploadCheckpoint>()
const bytes = ref(0)
const busy = ref(false)
const error = ref('')
const message = ref('')
const createKey = new SubmissionKey()
let controller: AbortController | undefined
try {
  const saved = localStorage.getItem(storageKey)
  if (saved) checkpoint.value = JSON.parse(saved) as UploadCheckpoint
} catch {
  /* Storage may be unavailable in private browsing. */
}
const percent = computed(() =>
  checkpoint.value ? Math.min(100, Math.floor((bytes.value / checkpoint.value.size) * 100)) : 0,
)
const save = () => {
  try {
    checkpoint.value
      ? localStorage.setItem(storageKey, JSON.stringify(checkpoint.value))
      : localStorage.removeItem(storageKey)
  } catch {
    message.value = '浏览器存储不可用，请保持本页打开以便续传。'
  }
}
function selectFile(event: Event) {
  file.value = (event.target as HTMLInputElement).files?.[0]
  if (file.value && !title.value) title.value = file.value.name.replace(/\.[^.]+$/, '')
  error.value = ''
}
async function finishUpload(current: UploadCheckpoint, signal: AbortSignal) {
  try {
    const result = await request<{ video_id: string; job_id: string }>(
      `/uploads/${current.uploadID}/complete`,
      {
        method: 'POST',
        body: { parts: current.completeParts },
        key: `complete-${current.uploadID}`,
        signal,
      },
    )
    checkpoint.value = undefined
    save()
    file.value = undefined
    title.value = ''
    bytes.value = 0
    message.value = '上传完成，视频已进入处理队列。'
    emit('completed', result.video_id)
  } catch (failure) {
    if (
      failure instanceof APIError &&
      ['UPLOAD_INCOMPLETE', 'UPLOAD_PART_MISMATCH'].includes(failure.code)
    ) {
      delete current.completeParts
      save()
    }
    throw failure
  }
}

async function upload() {
  if ((!file.value && !checkpoint.value?.completeParts) || busy.value) return
  busy.value = true
  error.value = ''
  message.value = ''
  controller = new AbortController()
  const signal = controller.signal
  const selected = file.value
  try {
    if (checkpoint.value?.completeParts) {
      await finishUpload(checkpoint.value, signal)
      return
    }
    if (!selected) throw new Error('请选择原视频文件以继续上传。')
    if (selected.size > MAX_BYTES) throw new Error('首版最大支持 2 GiB 视频。')
    partPlan(selected.size)
    const hash = await fingerprint(selected)
    if (
      checkpoint.value &&
      (checkpoint.value.size !== selected.size || checkpoint.value.fingerprint !== hash)
    )
      throw new Error('所选文件与待续传文件不一致，请选择原文件，或先放弃旧上传。')
    if (!checkpoint.value) {
      const body = {
        filename: selected.name,
        size_bytes: selected.size,
        content_type: selected.type || 'video/mp4',
        title: title.value || selected.name,
      }
      const created = await request<UploadCreated>('/uploads', {
        method: 'POST',
        body,
        key: createKey.for(body),
        signal,
      })
      checkpoint.value = {
        uploadID: created.upload_id,
        videoID: created.video_id,
        filename: selected.name,
        size: selected.size,
        lastModified: selected.lastModified,
        fingerprint: hash,
        partBytes: created.part_size_bytes || PART_BYTES,
      }
      save()
      createKey.clear()
    }
    const current = checkpoint.value
    if (current.completeParts) {
      await finishUpload(current, signal)
      return
    }
    const completed = (
      await request<List<CompletedPart>>(`/uploads/${current.uploadID}/parts`, { signal })
    ).items
    bytes.value = uploadedBytes(selected.size, current.partBytes, completed)
    const waiting = pendingParts(selected.size, current.partBytes, completed)
    const confirmedBytes = bytes.value
    const progress = new Map<number, number>()
    let cursor = 0
    const worker = async () => {
      while (cursor < waiting.length && !signal.aborted) {
        const part = waiting[cursor++]
        const signed = await request<List<PartURL>>(`/uploads/${current.uploadID}/part-urls`, {
          method: 'POST',
          body: { part_numbers: [part.part_number] },
          signal,
        })
        const target = signed.items.find((item) => item.part_number === part.part_number)
        if (!target) throw new Error('服务端未返回所需分片的上传地址。')
        await putPart(
          target.url,
          selected.slice(part.start, part.end),
          signal,
          (uploaded) => {
            progress.set(part.part_number, uploaded)
            bytes.value =
              confirmedBytes + [...progress.values()].reduce((sum, item) => sum + item, 0)
          },
          target.headers,
        )
      }
    }
    try {
      await Promise.all(Array.from({ length: Math.min(3, waiting.length) }, worker))
    } catch (failure) {
      controller.abort()
      throw failure
    }
    if (signal.aborted) throw new DOMException('上传已暂停', 'AbortError')
    const actual = (
      await request<List<CompletedPart>>(`/uploads/${current.uploadID}/parts`, { signal })
    ).items
    if (pendingParts(selected.size, current.partBytes, actual).length)
      throw new Error('存储仍有未完成分片，请继续上传。')
    const validNumbers = new Set(
      partPlan(selected.size, current.partBytes).map((part) => part.part_number),
    )
    // Persist before Complete: the remote merge or DB commit can succeed even if
    // this browser loses the response. ListParts is no longer valid after merge.
    current.completeParts = actual
      .filter((part) => validNumbers.has(part.part_number))
      .sort((a, b) => a.part_number - b.part_number)
      .map(({ part_number, etag }) => ({ part_number, etag }))
    save()
    await finishUpload(current, signal)
  } catch (failure) {
    error.value =
      failure instanceof DOMException && failure.name === 'AbortError' ? '' : errorMessage(failure)
    if (!error.value) message.value = '上传已暂停，已完成的分片会保留。'
  } finally {
    busy.value = false
  }
}
async function discard() {
  if (!checkpoint.value || busy.value) return
  busy.value = true
  error.value = ''
  try {
    await request(`/uploads/${checkpoint.value.uploadID}`, { method: 'DELETE' })
    checkpoint.value = undefined
    bytes.value = 0
    save()
    message.value = '已放弃上传。'
  } catch (failure) {
    error.value = errorMessage(failure)
  } finally {
    busy.value = false
  }
}
onBeforeUnmount(() => controller?.abort())
</script>

<template>
  <section class="panel upload-panel" aria-labelledby="upload-title">
    <div class="section-heading">
      <div>
        <p class="eyebrow">资料入口</p>
        <h2 id="upload-title">上传一段视频</h2>
      </div>
      <span class="muted">MP4 · 最大 2 GiB</span>
    </div>
    <p class="muted">支持有语音的课程和技术分享。文件按 8 MiB 分片，最多并行上传 3 片。</p>
    <div v-if="checkpoint" class="notice">
      {{
        checkpoint.completeParts
          ? '分片已传完，请确认合并结果，无需重传文件。'
          : `待续传：${checkpoint.filename}。刷新页面后需重新选择原文件。`
      }}
    </div>
    <form @submit.prevent="upload">
      <label
        >视频标题<input
          v-model="title"
          maxlength="200"
          placeholder="例如：理解 Kafka 的消费与重试"
          :disabled="busy || !!checkpoint"
      /></label>
      <label class="file-drop"
        >{{ file ? file.name : '选择本地视频文件'
        }}<input type="file" accept="video/mp4,.mp4" :disabled="busy" @change="selectFile"
      /></label>
      <div v-if="checkpoint" class="progress-line">
        <progress :value="percent" max="100" :aria-label="`上传进度 ${percent}%`" /><span
          >{{ percent }}%</span
        >
      </div>
      <p v-if="error" class="error" role="alert">{{ error }}</p>
      <p v-if="message" class="muted" role="status">{{ message }}</p>
      <div class="actions">
        <button
          class="primary"
          type="submit"
          :disabled="busy || (!file && !checkpoint?.completeParts)"
        >
          {{
            busy
              ? '正在传输…'
              : checkpoint?.completeParts
                ? '确认合并结果'
                : checkpoint
                  ? '继续上传'
                  : '上传并处理'
          }}</button
        ><button v-if="busy" type="button" @click="controller?.abort()">暂停</button
        ><button v-if="checkpoint && !busy" type="button" class="text-button" @click="discard">
          放弃此次上传
        </button>
      </div>
    </form>
  </section>
</template>
