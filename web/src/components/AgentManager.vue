<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { errorMessage, request, SubmissionKey } from '../api/client'
import type { Agent, AgentConfig, AgentVersion, List } from '../api/types'
import StatusBadge from './StatusBadge.vue'
const agents = ref<Agent[]>([])
const selected = ref<Agent>()
const versions = ref<AgentVersion[]>([])
const editing = ref<AgentVersion>()
const name = ref('')
const description = ref('')
const error = ref('')
const message = ref('')
const busy = ref(false)
const createKey = new SubmissionKey()
const draftKey = new SubmissionKey()
const allTools = ['search_segments', 'read_segments', 'inspect_frames', 'get_video_outline']
const draft = ref<AgentConfig>({
  prompt: '你是严谨的视频学习助手。根据实际读取的字幕和画面证据回答问题，证据不足时明确说明。',
  model_ref: 'default',
  tools: [...allTools],
  budgets: { max_tool_calls: 8, max_vision_calls: 2, max_model_iterations: 6 },
})
const locked = computed(() => editing.value?.status === 'PUBLISHED' || busy.value)
async function load() {
  try {
    agents.value = (await request<List<Agent>>('/agents')).items
  } catch (failure) {
    error.value = errorMessage(failure)
  }
}
async function select(agent: Agent) {
  selected.value = agent
  editing.value = undefined
  versions.value = []
  error.value = ''
  message.value = ''
  try {
    const result = await request<List<AgentVersion>>(`/agents/${agent.id}/versions`)
    if (selected.value?.id !== agent.id) return
    versions.value = result.items
    if (versions.value[0]) edit(versions.value[0])
  } catch (failure) {
    error.value = errorMessage(failure)
  }
}
function edit(version: AgentVersion) {
  editing.value = version
  draft.value = {
    prompt: version.prompt,
    model_ref: version.model_ref,
    tools: [...(version.tools ?? [])],
    budgets: { ...version.budgets },
  }
  message.value = ''
}
async function create() {
  busy.value = true
  error.value = ''
  try {
    const body = { name: name.value, description: description.value }
    const agent = await request<Agent>('/agents', {
      method: 'POST',
      body,
      key: createKey.for(body),
    })
    createKey.clear()
    name.value = ''
    description.value = ''
    await load()
    await select(agent)
    message.value = 'Agent 和第一个草稿已创建，可编辑后发布。'
  } catch (failure) {
    error.value = errorMessage(failure)
  } finally {
    busy.value = false
  }
}
async function save(asNew = false) {
  if (!selected.value) return
  busy.value = true
  error.value = ''
  message.value = ''
  try {
    const body = { ...draft.value, budgets: { ...draft.value.budgets } }
    if (new TextEncoder().encode(body.prompt).byteLength > 8000)
      throw new Error('提示词超过服务端 8,000 字节上限，请缩短后再保存。')
    const isNew = asNew || !editing.value
    const result = await request<AgentVersion>(
      isNew ? `/agents/${selected.value.id}/versions` : `/agent-versions/${editing.value!.id}`,
      {
        method: isNew ? 'POST' : 'PATCH',
        body,
        key: isNew ? draftKey.for({ agent: selected.value.id, ...body }) : undefined,
      },
    )
    draftKey.clear()
    await select(selected.value)
    edit(result)
    message.value = '草稿已保存，发布后才能用于新会话。'
  } catch (failure) {
    error.value = errorMessage(failure)
  } finally {
    busy.value = false
  }
}
async function publish() {
  if (!editing.value || !selected.value) return
  busy.value = true
  error.value = ''
  try {
    await request(`/agent-versions/${editing.value.id}/publish`, { method: 'POST' })
    await select(selected.value)
    message.value = '配置已发布。已有会话继续使用创建时的版本。'
  } catch (failure) {
    error.value = errorMessage(failure)
  } finally {
    busy.value = false
  }
}
onMounted(load)
</script>
<template>
  <div class="page-heading">
    <div>
      <p class="eyebrow">配置与版本</p>
      <h1>Agent 工作室</h1>
      <p class="muted">为不同学习任务维护助手配置，发布的版本供新会话选择。</p>
    </div>
  </div>
  <p v-if="error" class="error" role="alert">{{ error }}</p>
  <p v-if="message" class="notice" role="status">{{ message }}</p>
  <div class="agent-grid">
    <aside class="panel">
      <h2>我的 Agent</h2>
      <ul class="agent-list">
        <li v-for="agent in agents" :key="agent.id">
          <button :class="{ selected: selected?.id === agent.id }" @click="select(agent)">
            <strong>{{ agent.name }}</strong
            ><span>{{ agent.description || '视频分析助手' }}</span>
          </button>
        </li>
      </ul>
      <p v-if="!agents.length" class="muted">暂无 Agent，创建一个开始。</p>
      <form class="new-agent" @submit.prevent="create">
        <h3>新建 Agent</h3>
        <label
          >名称<input v-model="name" required maxlength="100" placeholder="课程学习助手" /></label
        ><label
          >用途<input
            v-model="description"
            maxlength="500"
            placeholder="整理知识点并解释课程图示" /></label
        ><button class="primary" :disabled="busy || !name.trim()">创建</button>
      </form>
    </aside>
    <section class="panel">
      <template v-if="selected"
        ><div class="section-heading">
          <h2>{{ selected.name }}</h2>
          <StatusBadge v-if="editing" :status="editing.status" />
        </div>
        <label v-if="versions.length"
          >查看版本<select
            :value="editing?.id"
            @change="
              edit(
                versions.find(
                  (version) => version.id === ($event.target as HTMLSelectElement).value,
                )!,
              )
            "
          >
            <option v-for="version in versions" :key="version.id" :value="version.id">
              {{
                version.version_no || version.version
                  ? `版本 ${version.version_no || version.version}`
                  : version.id.slice(0, 8)
              }}
              · {{ version.status }}
            </option>
          </select></label
        >
        <form @submit.prevent="save()">
          <label
            >系统提示词<textarea
              v-model="draft.prompt"
              rows="6"
              :disabled="locked"
              required
              maxlength="8000"
            /></label
          ><label
            >模型配置引用<input v-model="draft.model_ref" :disabled="locked" required /><small
              class="muted"
              >使用服务器已配置的引用名称，不填写密钥或供应商 URL。</small
            ></label
          >
          <fieldset :disabled="locked">
            <legend>允许使用的工具</legend>
            <label v-for="tool in allTools" :key="tool" class="checkbox-label"
              ><input v-model="draft.tools" type="checkbox" :value="tool" />{{ tool }}</label
            >
          </fieldset>
          <div class="budget-grid">
            <label
              >工具调用上限<input
                v-model.number="draft.budgets.max_tool_calls"
                type="number"
                min="1"
                max="8"
                :disabled="locked"
                required /></label
            ><label
              >看图次数上限<input
                v-model.number="draft.budgets.max_vision_calls"
                type="number"
                min="0"
                max="2"
                :disabled="locked"
                required /></label
            ><label
              >模型迭代上限<input
                v-model.number="draft.budgets.max_model_iterations"
                type="number"
                min="1"
                max="6"
                :disabled="locked"
                required
            /></label>
          </div>
          <div class="actions">
            <button
              v-if="editing?.status !== 'PUBLISHED'"
              type="submit"
              class="primary"
              :disabled="busy"
            >
              {{ editing ? '保存草稿' : '创建草稿' }}</button
            ><button
              v-if="editing?.status === 'DRAFT'"
              type="button"
              :disabled="busy"
              @click="publish"
            >
              发布已保存的草稿</button
            ><button v-if="editing" type="button" :disabled="busy" @click="save(true)">
              复制为新草稿
            </button>
          </div>
          <p v-if="editing?.status === 'PUBLISHED'" class="muted">
            已发布版本只读。需要调整时先复制为新草稿。
          </p>
        </form></template
      >
      <div v-else class="empty-state">
        <span class="empty-symbol" aria-hidden="true">✦</span>
        <h2>选择或创建一个 Agent</h2>
        <p>每次问答会固定使用一个已发布版本。</p>
      </div>
    </section>
  </div>
</template>
