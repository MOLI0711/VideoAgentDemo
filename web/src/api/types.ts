export interface User {
  id: string
  is_guest?: boolean
  email?: string
  username?: string
  name?: string
}
export interface AuthSession {
  user: User
  csrf_token: string
}
export interface List<T> {
  items: T[]
}
export interface Video {
  id: string
  title: string
  status: string
  mock?: boolean
  analysis_mode?: string
  active_content_version?: string
  latest_job_id?: string
  duration_ms?: number
  created_at?: string
  capabilities?: string[]
  summary?: Answer & { chapters?: Chapter[] }
}
export interface Segment {
  id: string
  start_ms: number
  end_ms: number
  transcript?: string
  text?: string
  ocr_text?: string
  frame_ids?: string[]
}
export interface Chapter {
  title: string
  start_ms: number
  end_ms: number
  summary?: string
}
export interface JobStep {
  id: string
  name?: string
  kind?: string
  step_type?: string
  step_key?: string
  status: string
  attempt_no?: number
  error_code?: string
  error_message?: string
  next_run_at?: string
}
export interface Job {
  id: string
  status: string
  steps: JobStep[]
  error_message?: string
}
export interface Agent {
  id: string
  name: string
  description?: string
  status?: string
}
export interface AgentConfig {
  prompt: string
  model_ref: string
  tools: string[]
  budgets: { max_tool_calls: number; max_vision_calls: number; max_model_iterations: number }
}
export interface AgentVersion extends AgentConfig {
  id: string
  agent_id: string
  status: string
  version?: number
  version_no?: number
  created_at?: string
}
export interface Conversation {
  id: string
  video_id: string
  content_version: string
  agent_version_id: string
}
export interface Message {
  id: string
  role: string
  content: string
  run_id?: string
  seq?: number
}
export interface Evidence {
  id: string
  kind: string
  start_ms?: number
  end_ms?: number
  timestamp_ms?: number
  asset_id?: string
  text?: string
}
export interface Answer {
  answer_status: string
  mock?: boolean
  claims: { text: string; evidence_ids: string[] }[]
  evidence: Evidence[]
  limitations: string[]
}
export interface Run {
  run_id: string
  job_id?: string
  status: string
  content_version: string
  latest_event_seq: number
  result?: Answer
  error?: { code: string; message: string }
  error_message?: string
  usage?: Record<string, number>
  phase?: string
}
export interface RunCreated {
  run_id: string
  job_id: string
  status: string
  events_url?: string
}
export interface CompletedPart {
  part_number: number
  etag: string
  size_bytes?: number
}
export interface UploadCreated {
  upload_id: string
  video_id: string
  part_size_bytes: number
}
export interface PartURL {
  part_number: number
  url: string
  headers?: Record<string, string>
}
