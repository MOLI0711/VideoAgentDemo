<script setup lang="ts">
import { onBeforeUnmount, ref, watch } from 'vue'
import { APIError, errorMessage, request } from '../api/client'
import { followRun } from '../api/run-stream'
import type { Run } from '../api/types'
import type { RunEvent } from '../stores/run-events'
import { isTerminal } from '../lib/timeline'
import RunAnswer from './RunAnswer.vue'
import StatusBadge from './StatusBadge.vue'
const props = defineProps<{ runID: string }>()
const emit = defineEmits<{ seek: [ms: number]; completed: []; status: [status: string] }>()
const run = ref<Run>()
const events = ref<RunEvent[]>([])
const error = ref('')
const connection = ref('正在读取快照…')
const cancelling = ref(false)
let stop: (() => void) | undefined
let finalID = ''
function connect() {
  stop?.()
  error.value = ''
  stop = followRun(props.runID, {
    snapshot: (snapshot) => {
      run.value = snapshot
      emit('status', snapshot.status)
      if (isTerminal(snapshot.status) && finalID !== props.runID) {
        finalID = props.runID
        emit('completed')
      }
    },
    event: (event) => {
      if (event.type === 'run.retrying') events.value = []
      events.value.push(event)
      if (events.value.length > 30) events.value.shift()
    },
    connection: (value) => {
      connection.value = value
    },
    error: (failure) => {
      error.value = errorMessage(failure)
      if (failure instanceof APIError && [403, 404].includes(failure.status))
        emit('status', 'FAILED')
    },
  })
}
watch(
  () => props.runID,
  () => {
    run.value = undefined
    events.value = []
    finalID = ''
    connect()
  },
  { immediate: true },
)
onBeforeUnmount(() => stop?.())
async function cancel() {
  cancelling.value = true
  try {
    await request(`/runs/${props.runID}/cancel`, { method: 'POST' })
    connect()
  } catch (failure) {
    error.value = errorMessage(failure)
  } finally {
    cancelling.value = false
  }
}
const eventLabels: Record<string, string> = {
  'run.queued': '已进入队列',
  'run.started': '开始分析',
  'run.retrying': '正在恢复执行',
  'tool.started': '调用工具',
  'tool.completed': '工具完成',
  'stage.started': '阶段开始',
  'stage.completed': '阶段完成',
  'answer.completed': '回答完成',
  'run.completed': '执行完成',
  'run.failed': '执行失败',
  'run.cancelled': '已取消',
  'run.phase': '阶段更新',
}
function eventDetail(event: RunEvent): string {
  const detail =
    event.data.tool_name ??
    event.data.tool ??
    event.data.phase ??
    event.data.stage ??
    event.data.message
  return typeof detail === 'string' ? detail : ''
}
</script>
<template>
  <section class="run-panel" aria-label="问答运行结果">
    <div class="section-heading">
      <h3>分析结果</h3>
      <StatusBadge v-if="run" :status="run.status" />
    </div>
    <p class="connection" role="status">
      <span class="connection-dot" :class="{ active: connection === '实时连接已建立' }"></span
      >{{ connection
      }}<button v-if="!run || !isTerminal(run.status)" class="text-button" @click="connect">
        重新连接
      </button>
    </p>
    <p v-if="error" class="error" role="alert">{{ error }}</p>
    <p v-if="run?.error || run?.error_message" class="error">
      {{ run.error?.message || run.error_message }}
    </p>
    <RunAnswer v-if="run?.result" :answer="run.result" @seek="emit('seek', $event)" />
    <p v-else-if="run && !isTerminal(run.status)" class="muted">
      正在检索与核验视频证据，完成后会显示可定位的回答。
    </p>
    <p v-else-if="run?.status === 'CANCELLED'" class="muted">本次问答已取消，可提交新问题。</p>
    <details v-if="events.length || run?.usage" class="execution-log">
      <summary>运行记录与用量</summary>
      <ol>
        <li v-for="event in events" :key="event.seq">
          <span>{{ eventLabels[event.type] || event.type }}</span
          ><small>{{ eventDetail(event) }}</small>
        </li>
      </ol>
      <dl v-if="run?.usage" class="usage">
        <template v-for="(value, key) in run.usage" :key="key"
          ><dt>{{ key }}</dt>
          <dd>{{ value }}</dd></template
        >
      </dl>
    </details>
    <button
      v-if="run && !isTerminal(run.status)"
      class="text-button danger"
      :disabled="cancelling"
      @click="cancel"
    >
      {{ cancelling ? '正在请求取消…' : '取消本次问答' }}
    </button>
  </section>
</template>
