<script setup lang="ts">
import { onBeforeUnmount, ref, watch } from 'vue'
import { errorMessage, request, SubmissionKey } from '../api/client'
import type { Job } from '../api/types'
import { isTerminal } from '../lib/timeline'
import StatusBadge from './StatusBadge.vue'
const props = defineProps<{ jobID: string }>()
const emit = defineEmits<{ finished: []; retried: [id: string] }>()
const job = ref<Job>()
const error = ref('')
const busy = ref(false)
const retryKey = new SubmissionKey()
let timer: ReturnType<typeof setTimeout> | undefined
let generation = 0
async function load(id: string, version: number) {
  try {
    const result = await request<Job>(`/jobs/${encodeURIComponent(id)}`)
    if (version !== generation) return
    const previous = job.value?.status
    job.value = result
    error.value = ''
    if (isTerminal(result.status)) {
      if (previous !== result.status) emit('finished')
      return
    }
    timer = setTimeout(() => {
      void load(id, version)
    }, 3000)
  } catch (failure) {
    if (version !== generation) return
    error.value = errorMessage(failure)
    timer = setTimeout(() => {
      void load(id, version)
    }, 8000)
  }
}
watch(
  () => props.jobID,
  (id) => {
    generation++
    if (timer) clearTimeout(timer)
    job.value = undefined
    if (id) void load(id, generation)
  },
  { immediate: true },
)
onBeforeUnmount(() => {
  generation++
  if (timer) clearTimeout(timer)
})
async function action(kind: 'retry' | 'cancel') {
  busy.value = true
  try {
    const result = await request<{ job_id: string }>(`/jobs/${props.jobID}/${kind}`, {
      method: 'POST',
      key: kind === 'retry' ? retryKey.for(props.jobID) : undefined,
    })
    if (kind === 'retry') {
      retryKey.clear()
      emit('retried', result.job_id)
    } else {
      if (timer) clearTimeout(timer)
      await load(props.jobID, generation)
    }
  } catch (failure) {
    error.value = errorMessage(failure)
  } finally {
    busy.value = false
  }
}
</script>
<template>
  <section class="job-progress" aria-label="任务进度">
    <div class="section-heading">
      <h3>处理进度</h3>
      <StatusBadge v-if="job" :status="job.status" />
    </div>
    <p v-if="!job && !error" class="muted" role="status">正在读取任务状态…</p>
    <ol v-if="job" class="step-list">
      <li v-for="step in job.steps" :key="step.id">
        <div>
          <span>{{ step.name || step.step_type || step.step_key || step.kind || step.id }}</span
          ><small v-if="step.attempt_no" class="muted">第 {{ step.attempt_no }} 次执行</small>
          <p v-if="step.error_message" class="error">{{ step.error_message }}</p>
        </div>
        <StatusBadge :status="step.status" />
      </li>
    </ol>
    <p v-if="job?.error_message" class="error">{{ job.error_message }}</p>
    <p v-if="error" class="error" role="alert">{{ error }}</p>
    <div v-if="job" class="actions">
      <button
        v-if="!isTerminal(job.status)"
        class="text-button"
        :disabled="busy || job.status === 'CANCEL_REQUESTED'"
        @click="action('cancel')"
      >
        取消任务</button
      ><button
        v-else-if="job.status === 'FAILED' || job.status === 'CANCELLED'"
        :disabled="busy"
        @click="action('retry')"
      >
        创建重试任务
      </button>
    </div>
  </section>
</template>
