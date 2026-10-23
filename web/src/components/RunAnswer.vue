<script setup lang="ts">
import { ref } from 'vue'
import { errorMessage, request } from '../api/client'
import type { Answer, Evidence } from '../api/types'
import { timestamp } from '../lib/timeline'
defineProps<{ answer: Answer }>()
const emit = defineEmits<{ seek: [ms: number] }>()
const images = ref<Record<string, string>>({})
const error = ref('')
const loading = ref('')
function expireImage(id: string) {
  delete images.value[id]
  error.value = '截图链接已过期或读取失败，请点击证据重新加载。'
}
async function inspect(evidence: Evidence) {
  const position = evidence.timestamp_ms ?? evidence.start_ms
  if (position !== undefined && Number.isFinite(position) && position >= 0) emit('seek', position)
  if (!evidence.asset_id) return
  loading.value = evidence.id
  error.value = ''
  try {
    const result = await request<{ url: string }>(
      `/assets/${encodeURIComponent(evidence.asset_id)}/url`,
    )
    images.value[evidence.id] = result.url
  } catch (failure) {
    error.value = errorMessage(failure)
  } finally {
    loading.value = ''
  }
}
</script>
<template>
  <article class="answer">
    <p v-if="answer.mock" class="notice">
      这份回答由合成测试供应商生成，仅用于功能联调，不代表视频真实内容。
    </p>
    <p v-if="answer.answer_status !== 'answered'" class="notice">
      {{
        answer.answer_status === 'insufficient_evidence'
          ? '当前证据不足以完整回答。'
          : `回答状态：${answer.answer_status}`
      }}
    </p>
    <div v-for="(claim, index) in answer.claims" :key="index" class="claim">
      <p>{{ claim.text }}</p>
      <div class="citation-row">
        <template v-for="id in claim.evidence_ids" :key="id"
          ><button
            v-if="answer.evidence.some((item) => item.id === id)"
            class="citation"
            @click="inspect(answer.evidence.find((item) => item.id === id)!)"
          >
            {{ answer.evidence.findIndex((item) => item.id === id) + 1 }} ·
            {{
              timestamp(
                answer.evidence.find((item) => item.id === id)!.timestamp_ms ??
                  answer.evidence.find((item) => item.id === id)!.start_ms ??
                  0,
              )
            }}
          </button></template
        >
      </div>
    </div>
    <div v-if="answer.evidence.length" class="evidence-list">
      <h4>原始证据</h4>
      <div v-for="(evidence, index) in answer.evidence" :key="evidence.id" class="evidence-item">
        <button
          class="evidence-button"
          :disabled="loading === evidence.id"
          @click="inspect(evidence)"
        >
          <span class="evidence-number">{{ index + 1 }}</span
          ><span
            >{{
              evidence.kind === 'frame'
                ? '查看画面'
                : evidence.kind === 'ocr'
                  ? '画面文字'
                  : '字幕片段'
            }}<small
              >{{ timestamp(evidence.timestamp_ms ?? evidence.start_ms ?? 0)
              }}{{ evidence.end_ms !== undefined ? ` — ${timestamp(evidence.end_ms)}` : '' }}</small
            ></span
          ><span aria-hidden="true">↗</span>
        </button>
        <p v-if="evidence.text" class="muted excerpt">{{ evidence.text }}</p>
        <img
          v-if="images[evidence.id]"
          :src="images[evidence.id]"
          :alt="`证据 ${index + 1}，视频 ${timestamp(evidence.timestamp_ms ?? 0)} 的实际截图`"
          @error="expireImage(evidence.id)"
        />
      </div>
    </div>
    <div v-if="answer.limitations.length" class="limitations">
      <h4>回答限制</h4>
      <ul>
        <li v-for="item in answer.limitations" :key="item">{{ item }}</li>
      </ul>
    </div>
    <p v-if="error" class="error" role="alert">{{ error }}</p>
  </article>
</template>
