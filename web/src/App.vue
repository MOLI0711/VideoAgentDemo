<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { auth, errorMessage, request, restoreSession } from './api/client'
import type { List, Video } from './api/types'
import { timestamp } from './lib/timeline'
import AgentManager from './components/AgentManager.vue'
import StatusBadge from './components/StatusBadge.vue'
import UploadPanel from './components/UploadPanel.vue'
import VideoWorkspace from './components/VideoWorkspace.vue'
const page = ref<'library' | 'agents'>('library')
const videos = ref<Video[]>([])
const selectedID = ref('')
const error = ref('')
const loadingVideos = ref(false)
const query = ref('')
const meta = ref<{ model_mode?: string; asr_mode?: string; search_enabled?: boolean }>()
const visible = computed(() =>
  videos.value.filter((video) =>
    video.title.toLocaleLowerCase().includes(query.value.trim().toLocaleLowerCase()),
  ),
)
const mockMode = computed(
  () => meta.value?.model_mode === 'mock' || meta.value?.asr_mode === 'mock',
)
const disabledMode = computed(
  () => meta.value?.model_mode === 'disabled' || meta.value?.asr_mode === 'disabled',
)
async function loadVideos() {
  const owner = auth.user?.id
  loadingVideos.value = true
  try {
    const result = await request<List<Video>>('/videos')
    if (auth.user?.id === owner) videos.value = result.items
  } catch (failure) {
    if (auth.user?.id === owner) error.value = errorMessage(failure)
  } finally {
    if (auth.user?.id === owner) loadingVideos.value = false
  }
}
async function loadMeta() {
  const owner = auth.user?.id
  try {
    const result = await request<NonNullable<typeof meta.value>>('/meta')
    if (auth.user?.id === owner) meta.value = result
  } catch (failure) {
    if (auth.user?.id === owner) error.value = errorMessage(failure)
  }
}
async function connect() {
  try {
    await restoreSession()
  } catch {
    // The recovery screen reads auth.error, including service and rate-limit errors.
  }
}
async function uploaded(id: string) {
  await loadVideos()
  selectedID.value = id
}
function openLibrary() {
  page.value = 'library'
  selectedID.value = ''
}
watch(
  () => auth.user?.id,
  (owner, previousOwner) => {
    if (owner === previousOwner) return
    selectedID.value = ''
    videos.value = []
    page.value = 'library'
    query.value = ''
    error.value = ''
    meta.value = undefined
  },
)
watch(
  () => auth.ready && auth.user?.id,
  (owner) => {
    if (owner) void Promise.all([loadVideos(), loadMeta()])
  },
)
onMounted(connect)
</script>
<template>
  <main v-if="!auth.ready" class="connection-screen" aria-busy="true">
    <div class="connection-card">
      <p class="wordmark">vedioagent</p>
      <p class="muted" role="status">正在连接你的工作台…</p>
    </div>
  </main>
  <main v-else-if="!auth.user" class="connection-screen">
    <section class="connection-card panel">
      <p class="wordmark">vedioagent</p>
      <h1>暂时无法打开工作台</h1>
      <p class="error" role="alert">{{ auth.error || '连接暂时不可用，请稍后重试。' }}</p>
      <button class="primary" @click="connect">重新连接</button>
    </section>
  </main>
  <div v-else class="app-shell">
    <aside class="sidebar">
      <a href="#" class="wordmark" @click.prevent="openLibrary"
        >vedioagent<span class="wordmark-detail">VIDEO KNOWLEDGE</span></a
      >
      <nav aria-label="主导航">
        <button :class="{ active: page === 'library' }" @click="openLibrary">
          <span aria-hidden="true">▤</span> 视频资料库</button
        ><button :class="{ active: page === 'agents' }" @click="page = 'agents'">
          <span aria-hidden="true">✦</span> Agent 工作室
        </button>
      </nav>
      <div class="sidebar-info">
        <span class="eyebrow">工作方式</span>
        <p>视频处理一次<br />每次回答，回到证据</p>
      </div>
      <div class="account">
        <span class="avatar" aria-hidden="true">{{ auth.user.is_guest ? '访' : '我' }}</span>
        <div>
          <strong>{{ auth.user.is_guest ? '访客空间' : '我的工作台' }}</strong>
          <span class="account-note">{{
            auth.user.is_guest ? '当前浏览器专属' : '已保留现有资料'
          }}</span>
        </div>
      </div>
    </aside>
    <div class="main-shell">
      <header class="topbar">
        <span
          >vedioagent /
          {{ page === 'agents' ? 'Agent 工作室' : selectedID ? '视频工作台' : '视频资料库' }}</span
        ><span class="environment-tag">{{
          mockMode ? '合成测试模式' : disabledMode ? '模型服务未启用' : '视频知识工作台'
        }}</span>
      </header>
      <div v-if="mockMode" class="mock-banner" role="status">
        当前使用合成测试供应商，输出用于功能联调，不能作为视频真实内容或效果评测结果。
      </div>
      <div v-else-if="disabledMode" class="mock-banner" role="status">
        当前模型或 ASR 服务尚未启用。可上传和管理资料；完整解析与问答需要先配置并启用相关服务。
      </div>
      <main class="main-content">
        <p v-if="auth.user.is_guest" class="guest-note">
          内容保存在当前浏览器的访客空间；清除站点数据或会话到期后无法恢复访问。
        </p>
        <p v-if="auth.notice" class="notice global-error" role="status">
          {{ auth.notice }}<button class="text-button" @click="auth.notice = ''">关闭</button>
        </p>
        <p v-if="error" class="error global-error" role="alert">
          {{ error }}<button class="text-button" @click="error = ''">关闭</button>
        </p>
        <AgentManager v-if="page === 'agents'" /><template v-else
          ><button v-if="selectedID" class="back-button" @click="selectedID = ''">
            ← 返回资料库</button
          ><VideoWorkspace
            v-if="selectedID"
            :video-i-d="selectedID"
            @refresh="loadVideos"
          /><template v-else
            ><div class="page-heading">
              <div>
                <p class="eyebrow">你的知识，从这里开始</p>
                <h1>
                  视频资料库<span class="count">{{ videos.length }}</span>
                </h1>
                <p class="muted">上传、整理和理解视频里的知识。</p>
              </div>
              <button @click="loadVideos" :disabled="loadingVideos">刷新列表</button>
            </div>
            <UploadPanel @completed="uploaded" />
            <section class="library-section">
              <div class="section-heading">
                <h2>全部视频</h2>
                <label class="search-label"
                  ><span class="sr-only">搜索视频</span
                  ><input v-model="query" type="search" placeholder="搜索标题…"
                /></label>
              </div>
              <p v-if="loadingVideos" class="muted" role="status">正在读取资料库…</p>
              <div v-if="visible.length" class="video-grid">
                <button
                  v-for="(video, index) in visible"
                  :key="video.id"
                  class="video-card"
                  @click="selectedID = video.id"
                >
                  <div class="video-cover" :class="`cover-${index % 3}`">
                    <span class="cover-label">VIDEO / {{ String(index + 1).padStart(2, '0') }}</span
                    ><span class="cover-play" aria-hidden="true">▷</span
                    ><span v-if="video.duration_ms" class="cover-duration">{{
                      timestamp(video.duration_ms)
                    }}</span>
                  </div>
                  <div class="video-card-body">
                    <h3>{{ video.title }}</h3>
                    <div class="video-card-footer">
                      <StatusBadge :status="video.status" /><span>打开工作台 ↗</span>
                    </div>
                  </div>
                </button>
              </div>
              <div v-else-if="!loadingVideos" class="empty-state panel">
                <span class="empty-symbol" aria-hidden="true">▤</span>
                <h3>{{ query ? '没有匹配的视频' : '第一段视频，第一份知识' }}</h3>
                <p>{{ query ? '尝试其他关键词。' : '从上方上传一段课程或技术分享开始。' }}</p>
              </div>
            </section></template
          ></template
        >
      </main>
    </div>
  </div>
</template>
