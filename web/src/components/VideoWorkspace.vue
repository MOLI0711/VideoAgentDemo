<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { APIError, auth, errorMessage, request, SubmissionKey } from '../api/client'
import type {
  Agent,
  AgentVersion,
  Conversation,
  List,
  Message,
  RunCreated,
  Segment,
  Video,
} from '../api/types'
import { isTerminal, seekSeconds, timestamp } from '../lib/timeline'
import JobProgress from './JobProgress.vue'
import RunAnswer from './RunAnswer.vue'
import RunPanel from './RunPanel.vue'
import StatusBadge from './StatusBadge.vue'

const props = defineProps<{ videoID: string }>()
const emit = defineEmits<{ refresh: [] }>()
const video = ref<Video>()
const segments = ref<Segment[]>([])
const player = ref<HTMLVideoElement>()
const playback = ref('')
const playError = ref('')
const error = ref('')
const loading = ref(true)
const segmentQuery = ref('')
const currentMs = ref(0)
const pendingSeek = ref<number>()
const options = ref<{ id: string; label: string }[]>([])
const chosenAgent = ref('')
const conversation = ref<Conversation>()
const messages = ref<Message[]>([])
const runID = ref('')
const runStatus = ref('')
const question = ref('')
const submitting = ref(false)
const questionKey = new SubmissionKey()
const sessionKey = new SubmissionKey()
const summaryKey = new SubmissionKey()
const summaryJobID = ref('')
const jobOverride = ref('')
let generation = 0
let disposed = false
const contentVersion = computed(
  () => conversation.value?.content_version || video.value?.active_content_version,
)
const filteredSegments = computed(() => {
  const query = segmentQuery.value.trim().toLocaleLowerCase()
  return segments.value.filter(
    (segment) =>
      !query ||
      `${segment.transcript || segment.text || ''} ${segment.ocr_text || ''}`
        .toLocaleLowerCase()
        .includes(query),
  )
})
const canAsk = computed(
  () =>
    !!contentVersion.value &&
    !!chosenAgent.value &&
    !submitting.value &&
    (!runID.value || isTerminal(runStatus.value)),
)
const storageKey = () => `moli-conversation:${auth.user?.id}:${props.videoID}`
function remember() {
  try {
    localStorage.setItem(
      storageKey(),
      JSON.stringify({ conversation: conversation.value, runID: runID.value }),
    )
  } catch {
    /* Storage is optional; session itself is persisted by the API. */
  }
}
async function refreshVideo() {
  const selected = props.videoID
  try {
    const result = await request<Video>(`/videos/${encodeURIComponent(selected)}`)
    if (selected === props.videoID && !disposed) video.value = result
  } catch (failure) {
    error.value = errorMessage(failure)
  }
}
async function loadPlayback() {
  if (!contentVersion.value) return
  const selected = props.videoID
  const version = contentVersion.value
  try {
    const result = await request<{ url: string }>(
      `/videos/${encodeURIComponent(selected)}/playback-url?content_version=${encodeURIComponent(version)}`,
    )
    if (selected === props.videoID && version === contentVersion.value && !disposed) {
      playback.value = result.url
      playError.value = ''
    }
  } catch (failure) {
    playError.value = errorMessage(failure)
  }
}
async function loadContent() {
  if (!contentVersion.value) return
  const selected = props.videoID
  const version = contentVersion.value
  try {
    const result = await request<List<Segment>>(
      `/videos/${encodeURIComponent(selected)}/segments?content_version=${encodeURIComponent(version)}&limit=1000`,
    )
    if (selected === props.videoID && version === contentVersion.value && !disposed)
      segments.value = result.items
  } catch (failure) {
    error.value = errorMessage(failure)
  }
  await loadPlayback()
}
async function loadMessages() {
  if (!conversation.value) return
  const id = conversation.value.id
  try {
    const result = await request<List<Message>>(`/sessions/${encodeURIComponent(id)}/messages`)
    if (id === conversation.value?.id && !disposed) messages.value = result.items
  } catch (failure) {
    error.value = errorMessage(failure)
  }
}
async function loadAgents() {
  try {
    const agents = (await request<List<Agent>>('/agents')).items
    const result = await Promise.all(
      agents.map(async (agent) => ({
        agent,
        versions: (await request<List<AgentVersion>>(`/agents/${agent.id}/versions`)).items,
      })),
    )
    if (disposed) return
    options.value = result.flatMap(({ agent, versions }) =>
      versions
        .filter((version) => version.status === 'PUBLISHED')
        .map((version) => ({
          id: version.id,
          label: `${agent.name} · ${version.version_no ?? version.version ?? version.id.slice(0, 6)}`,
        })),
    )
    if (!chosenAgent.value && options.value[0]) chosenAgent.value = options.value[0].id
  } catch (failure) {
    error.value = errorMessage(failure)
  }
}
async function loadVideo(id: string) {
  const current = ++generation
  loading.value = true
  video.value = undefined
  playback.value = ''
  segments.value = []
  conversation.value = undefined
  messages.value = []
  runID.value = ''
  runStatus.value = ''
  question.value = ''
  summaryJobID.value = ''
  jobOverride.value = ''
  error.value = ''
  playError.value = ''
  pendingSeek.value = undefined
  questionKey.clear()
  sessionKey.clear()
  try {
    const result = await request<Video>(`/videos/${encodeURIComponent(id)}`)
    if (current !== generation || disposed) return
    video.value = result
    try {
      const saved = localStorage.getItem(storageKey())
      if (saved) {
        const parsed = JSON.parse(saved) as { conversation?: Conversation; runID?: string }
        if (parsed.conversation?.video_id === id) {
          conversation.value = parsed.conversation
          chosenAgent.value = parsed.conversation.agent_version_id
          runID.value = parsed.runID || ''
          runStatus.value = runID.value ? 'QUEUED' : ''
        }
      }
    } catch {
      /* Invalid local metadata does not change server state. */
    }
    await Promise.all([loadContent(), loadMessages()])
  } catch (failure) {
    if (current === generation) error.value = errorMessage(failure)
  } finally {
    if (current === generation) loading.value = false
  }
}
watch(() => props.videoID, loadVideo, { immediate: true })
onMounted(loadAgents)
onBeforeUnmount(() => {
  disposed = true
  generation++
})
async function seek(ms: number) {
  if (!Number.isFinite(ms) || ms < 0) return
  pendingSeek.value = ms
  await nextTick()
  if (!player.value || player.value.readyState < 1) return
  player.value.currentTime = seekSeconds(ms, player.value.duration)
  pendingSeek.value = undefined
  player.value.focus()
}
function metadataLoaded() {
  if (pendingSeek.value !== undefined) void seek(pendingSeek.value)
}
async function renewPlayback() {
  pendingSeek.value = currentMs.value
  await loadPlayback()
}
async function ask() {
  if (!question.value.trim() || !canAsk.value) return
  submitting.value = true
  error.value = ''
  try {
    if (!conversation.value) {
      const body = {
        video_id: props.videoID,
        content_version: contentVersion.value,
        agent_version_id: chosenAgent.value,
      }
      conversation.value = await request<Conversation>('/sessions', {
        method: 'POST',
        body,
        key: sessionKey.for(body),
      })
      sessionKey.clear()
      remember()
    }
    const body = { question: question.value.trim() }
    const run = await request<RunCreated>(`/sessions/${conversation.value.id}/runs`, {
      method: 'POST',
      body,
      key: questionKey.for(body),
    })
    runID.value = run.run_id
    runStatus.value = run.status
    question.value = ''
    questionKey.clear()
    remember()
    await loadMessages()
  } catch (failure) {
    error.value = errorMessage(failure)
    if (
      failure instanceof APIError &&
      failure.code === 'ACTIVE_RUN_CONFLICT' &&
      typeof (failure.details.current_run_id ?? failure.details.run_id) === 'string'
    ) {
      runID.value = (failure.details.current_run_id ?? failure.details.run_id) as string
      runStatus.value = 'RUNNING'
      remember()
    }
  } finally {
    submitting.value = false
  }
}
async function newConversation() {
  conversation.value = undefined
  messages.value = []
  runID.value = ''
  runStatus.value = ''
  question.value = ''
  error.value = ''
  sessionKey.clear()
  questionKey.clear()
  remember()
  if (!options.value.some((option) => option.id === chosenAgent.value))
    chosenAgent.value = options.value[0]?.id || ''
  await loadContent()
}
function viewRun(id: string) {
  runID.value = id
  runStatus.value = 'QUEUED'
  remember()
}
async function processed() {
  await refreshVideo()
  await loadContent()
  emit('refresh')
}
async function retryVideoJob(id: string) {
  jobOverride.value = id
  await refreshVideo()
  emit('refresh')
}
async function summarize() {
  if (!video.value?.active_content_version || !chosenAgent.value) return
  error.value = ''
  try {
    const body = {
      content_version: video.value.active_content_version,
      agent_version_id: chosenAgent.value,
    }
    const result = await request<{ job_id: string }>(`/videos/${props.videoID}/summaries`, {
      method: 'POST',
      body,
      key: summaryKey.for(body),
    })
    summaryJobID.value = result.job_id
    summaryKey.clear()
  } catch (failure) {
    error.value = errorMessage(failure)
  }
}
</script>

<template>
  <div v-if="loading" class="empty-state" role="status">正在打开视频工作台…</div>
  <template v-else-if="video">
    <div class="page-heading">
      <div>
        <p class="eyebrow">视频知识工作台</p>
        <h1>{{ video.title }}</h1>
        <p class="muted">一次处理，反复检索。每条回答都可以回到视频证据。</p>
      </div>
      <StatusBadge :status="video.status" />
    </div>
    <p v-if="error" class="error" role="alert">{{ error }}</p>
    <p v-if="video.mock" class="notice">
      当前视频内容版本使用合成转录，仅用于功能联调，字幕与回答不能作为视频真实内容。
    </p>
    <div class="workspace-grid">
      <div class="video-column">
        <section class="panel player-panel">
          <video
            v-if="playback"
            ref="player"
            :src="playback"
            controls
            preload="metadata"
            tabindex="0"
            aria-label="视频播放器"
            @loadedmetadata="metadataLoaded"
            @timeupdate="currentMs = ($event.target as HTMLVideoElement).currentTime * 1000"
            @error="playError = '视频链接可能已过期或当前格式无法播放，请刷新播放链接。'"
          />
          <div v-else class="player-empty">
            <span aria-hidden="true">▷</span>
            <p>
              {{ video.active_content_version ? '正在获取播放地址' : '视频处理完成后可播放和定位' }}
            </p>
          </div>
          <div class="player-footer">
            <span class="muted"
              >{{ timestamp(currentMs)
              }}{{ video.duration_ms ? ` / ${timestamp(video.duration_ms)}` : '' }}</span
            ><button v-if="contentVersion" class="text-button" @click="renewPlayback">
              刷新播放链接
            </button>
          </div>
          <p v-if="playError" class="error" role="alert">{{ playError }}</p>
          <p
            v-if="conversation && conversation.content_version !== video.active_content_version"
            class="notice"
          >
            当前会话固定使用旧内容版本，播放器和字幕已同步到该版本。
          </p>
        </section>
        <section v-if="jobOverride || video.latest_job_id" class="panel">
          <JobProgress
            :job-i-d="jobOverride || video.latest_job_id!"
            @finished="processed"
            @retried="retryVideoJob"
          />
        </section>
        <section class="panel">
          <div class="section-heading">
            <h2>章节与概要</h2>
            <button v-if="video.active_content_version" :disabled="!chosenAgent" @click="summarize">
              {{ video.summary ? '重新生成概要' : '生成概要' }}
            </button>
          </div>
          <JobProgress
            v-if="summaryJobID"
            :job-i-d="summaryJobID"
            @finished="refreshVideo"
            @retried="summaryJobID = $event"
          /><template
            v-if="
              video.summary &&
              (!conversation || conversation.content_version === video.active_content_version)
            "
            ><ol v-if="video.summary.chapters?.length" class="chapter-list">
              <li v-for="chapter in video.summary.chapters" :key="chapter.start_ms">
                <button @click="seek(chapter.start_ms)">
                  <time>{{ timestamp(chapter.start_ms) }}</time
                  ><span>{{ chapter.title }}</span>
                </button>
              </li>
            </ol>
            <p v-else class="muted">当前概要尚无章节划分。</p>
            <RunAnswer :answer="video.summary" @seek="seek"
          /></template>
          <p v-else class="muted">
            {{
              conversation && conversation.content_version !== video.active_content_version
                ? '当前页面概要属于最新版本；旧会话请使用下方固定版本字幕定位。'
                : '处理完成后，可选择已发布的 Agent 生成视频概要。'
            }}
          </p>
        </section>
        <section class="panel">
          <div class="section-heading">
            <h2>字幕与画面文字</h2>
            <span class="muted">{{ segments.length }} 个片段</span>
          </div>
          <label class="search-label"
            ><span class="sr-only">搜索当前字幕</span
            ><input v-model="segmentQuery" type="search" placeholder="在当前字幕中搜索…"
          /></label>
          <div class="transcript-list">
            <button
              v-for="segment in filteredSegments"
              :key="segment.id"
              class="transcript-row"
              :class="{ current: currentMs >= segment.start_ms && currentMs < segment.end_ms }"
              @click="seek(segment.start_ms)"
            >
              <time>{{ timestamp(segment.start_ms) }}</time
              ><span
                >{{ segment.transcript || segment.text || '（无语音转写）'
                }}<small v-if="segment.ocr_text">画面：{{ segment.ocr_text }}</small></span
              >
            </button>
            <p v-if="!filteredSegments.length" class="muted">
              {{ segments.length ? '没有匹配的字幕。' : '字幕尚未就绪。' }}
            </p>
          </div>
        </section>
      </div>
      <aside class="panel chat-column">
        <div class="section-heading">
          <div>
            <p class="eyebrow">用证据回答</p>
            <h2>视频问答</h2>
          </div>
          <button
            v-if="conversation"
            class="text-button"
            :disabled="!!runID && !isTerminal(runStatus)"
            @click="newConversation"
          >
            新会话
          </button>
        </div>
        <label
          >使用 Agent<select v-model="chosenAgent" :disabled="!!conversation || submitting">
            <option value="" disabled>选择已发布版本</option>
            <option v-for="option in options" :key="option.id" :value="option.id">
              {{ option.label }}
            </option>
            <option
              v-if="conversation && !options.some((item) => item.id === chosenAgent)"
              :value="chosenAgent"
            >
              当前会话固定版本 {{ chosenAgent.slice(0, 8) }}
            </option>
          </select></label
        >
        <p v-if="!options.length" class="notice">
          请先到 Agent 工作室创建并发布配置。<button class="text-button" @click="loadAgents">
            刷新列表
          </button>
        </p>
        <details v-if="messages.length" class="history">
          <summary>会话记录（{{ messages.length }} 条）</summary>
          <article v-for="item in messages" :key="item.id" :class="item.role">
            <strong>{{ item.role === 'user' ? '你' : '助手' }}</strong>
            <p>
              {{
                item.role === 'assistant' && item.run_id
                  ? '回答已保存，可打开对应运行查看引用。'
                  : item.content
              }}
            </p>
            <button
              v-if="item.run_id && item.role !== 'user'"
              class="text-button"
              :disabled="!!runID && !isTerminal(runStatus)"
              @click="viewRun(item.run_id)"
            >
              查看回答
            </button>
          </article>
        </details>
        <RunPanel
          v-if="runID"
          :key="runID"
          :run-i-d="runID"
          @seek="seek"
          @completed="loadMessages"
          @status="runStatus = $event"
        />
        <div v-else class="chat-empty">
          <span aria-hidden="true">✦</span>
          <h3>从一个具体问题开始</h3>
          <p>例如：这段课程的关键观点是什么？<br />图中的两个模块如何协作？</p>
        </div>
        <form class="question-form" @submit.prevent="ask">
          <label
            ><span class="sr-only">向视频提问</span
            ><textarea
              v-model="question"
              rows="3"
              maxlength="4000"
              placeholder="输入问题，回答将附带视频证据…"
              :disabled="submitting"
              @keydown.ctrl.enter.prevent="ask"
              @keydown.meta.enter.prevent="ask"
            />
          </label>
          <div class="question-footer">
            <small class="muted">⌘ / Ctrl + Enter 发送</small
            ><button class="primary" :disabled="!canAsk || !question.trim()">
              {{ submitting ? '正在提交…' : '发送问题 ↗' }}
            </button>
          </div>
          <p v-if="!contentVersion" class="muted">视频处理完成后即可提问。</p>
        </form>
      </aside>
    </div>
  </template>
  <p v-else class="error" role="alert">{{ error || '无法读取视频。' }}</p>
</template>
