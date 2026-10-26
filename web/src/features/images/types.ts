/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program. If not, see <https://www.gnu.org/licenses/>.

For commercial licensing, please contact support@quantumnous.com
*/
export type ImagePlatform = string
export type VideoModeName =
  | 'text_to_video'
  | 'image_to_video'
  | 'reference_to_video'
  | 'first_last_frame'
  | 'edit_video'
  | 'extend_video'
export type VideoDurationCapability = {
  min?: number
  max?: number
  step?: number
  values?: number[]
  default: number
}
export type VideoInputCapability = {
  name:
    | 'image'
    | 'last_frame'
    | 'reference_images'
    | 'reference_videos'
    | 'reference_audios'
    | 'video'
  kind: 'image' | 'video' | 'audio'
  sources: ('url' | 'asset' | 'data_uri' | 'file_id' | 'voice_id' | 'upload')[]
  maxItems: number
  required?: boolean
}
export type VideoModeCapability = {
  name: VideoModeName
  duration?: VideoDurationCapability
  resolutions?: string[]
  defaultResolution?: string
  aspectRatios?: string[]
  defaultAspectRatio?: string
  extensionDirections?: ('forward' | 'backward')[]
  defaultExtensionDirection?: 'forward' | 'backward'
  inputs?: VideoInputCapability[]
  options?: {
    generateAudio?: boolean
    watermark?: boolean
    returnLastFrame?: boolean
  }
}
export type VideoCapability = {
  models: string[]
  modes: VideoModeCapability[]
}
export type VideoModel = {
  id: string
  label: string
  provider: string
  available: boolean
  availability_reason?: string
  pricing_status?:
    | 'configured'
    | 'needs_configuration'
    | 'invalid_configuration'
  protocol: string
  supported_parameters: string[]
  max_duration_seconds: number
  capability: VideoCapability
}
export type ImageModel = {
  provider?: string
  media_type?: 'image'
  supports_edit?: boolean
  supports_reference_generation?: boolean
  supported_parameters?: string[]
  id: string
  label: string
  platform: ImagePlatform
  mode: 'realtime' | 'async'
  protocol: string
  available: boolean
  capability: {
    qualities?: string[]
    resolutions?: string[]
    formats?: string[]
    backgrounds?: string[]
    max_output_images: number
    max_reference_images: number
    allow_half_k?: boolean
  }
}
export type ImageCapabilities = {
  api_key_id: number
  capability_version: string
  gateway_base_url: string
  models: ImageModel[]
  video_models?: VideoModel[]
  image_providers?: string[]
  platforms: {
    platform: ImagePlatform
    mode: string
    protocol: string
    available: boolean
    reason: string
    group: string
  }[]
}
export type ImageTask = {
  failure_code?: number
  error_summary?: string
  error_summary_key?: string
  error_summary_params?: Record<string, string | number>
  error_suggestion?: string
  error_suggestion_key?: string
  error_detail?: string
  error_retryable?: boolean
  error_stage?: string
  retry_decision?: ImageRetryDecision
  retained_request_available?: boolean
  request_retain_until?: number
  media_type?: 'image' | 'video'
  provider?: string
  stage?: string
  storage_status?: string
  id: string
  task_id: string
  platform: ImagePlatform
  protocol: string
  model: string
  request_type: string
  status: string
  billing_status: string
  progress: number
  upstream_progress?: number
  image_count: number
  result_count: number
  cost: number
  prompt_summary: string
  requested_size: string
  requested_resolution?: string
  actual_size: string
  aspect_ratio: string
  group: string
  api_key_id: number
  api_key_name?: string
  user_name?: string
  channel_name?: string
  storage_providers?: string[] | null
  user_id?: number
  channel_id?: number
  attempts?: string
  reference_urls?: string
  requested_seconds?: number
  reference_media?: {
    type: 'image' | 'video' | 'audio'
    url: string
  }[]
  request_parameters?: Record<string, unknown>
  reconciliation_status?: string
  attempt_history?: {
    channel_name: string
    key_index?: number | null
    status?:
      | 'selected'
      | 'processing'
      | 'accepted'
      | 'succeeded'
      | 'failed'
      | 'execution_unknown'
    started_at: number
    finished_at: number
    dispatched: boolean
    reference_mode: string
    error_code: number
    error_message?: string
    http_status?: number
    reference_transport_used?: string
    retry_decision?: ImageRetryDecision
  }[]
  attempt_history_unavailable?: boolean
  retry_count: number
  created_at: number
  started_at: number
  finished_at: number
  expires_at: number
  next_attempt_at: number
  error_code: string
  error_message: string
  can_resume: boolean
  can_terminate: boolean
}
export type ImageRetryDecision = {
  version: number
  stage: string
  action: string
  reason: string
  rule: string
  budget?: string
  used: number
  limit: number
  total_used: number
  total_limit: number
  next_attempt_at?: number
  deadline?: number
  recorded_at: number
}
export type ImageRetainedRequest = {
  task_id: string
  retain_until: number
  request: {
    model: string
    prompt: string
    prompt_characters: number
    prompt_bytes: number
    prompt_truncated: boolean
    platform: string
    kind: string
    count: number
    size: string
    resolution: string
    aspect_ratio: string
    reference_count: number
  }
}
export type ImageRecoveryRate = {
  succeeded: number
  resolved: number
  percent: number | null
}
export type ImageReliabilityStats = {
  total: number
  succeeded: number
  failed: number
  expired: number
  execution_unknown: number
  pending: number
  postprocess_pending: number
  admin_terminated: number
  audited_tasks: number
  final_success: ImageRecoveryRate
  first_submission_success: ImageRecoveryRate
  retry_recovery: ImageRecoveryRate
  account_switch_recovery: ImageRecoveryRate
  local_fallback_recovery: ImageRecoveryRate
}
export type ImageReliabilityReport = {
  totals: ImageReliabilityStats
  groups: (ImageReliabilityStats & {
    provider: string
    model: string
    channel_id: number
    channel_name: string
  })[]
}
export type ImageResult = {
  id: number
  image_index: number
  width: number
  height: number
  content_type: string
  byte_size: number
  checksum: string
  view_url: string
  /** Signed object URL when the detail API could mint one. */
  url?: string
  /** Result bytes require the dashboard session client. */
  authenticated?: boolean
  source?: 'upstream'
  expires_at: number
}
export type ImageTaskDetail = {
  task: ImageTask
  results: ImageResult[]
  events: {
    id: number
    event_type: string
    status: string
    message: string
    created_at: number
  }[]
}
export type ImageTaskList = {
  items: ImageTask[]
  total: number
  stats: Record<string, number | null>
  pages: number
}
export type ImageMetadata = {
  title: string
  private_prompt: string
  public_title: string
  share_prompt: boolean
  platform: ImagePlatform
  generation_mode: string
  source_type: string
  model: string
  requested_size: string
  aspect_ratio: string
  quality: string
  content_type: string
  byte_size: number
  checksum_sha256: string
  client_blob_key: string
}
export type LocalImage = {
  id: string
  userId: number
  blob: Blob
  createdAt: number
  expiresAt: number
  metadata: ImageMetadata
  submissionId?: string
  submissionOperationKey?: string
}
export type ImageSubmission = {
  id: string
  user_id: number
  client_blob_key: string
  status: string
  reason?: string
  metadata: string
  checksum: string
  byte_size: number
  content_type: string
  asset_id?: string
  publication_id?: string
  created_at: number
  expires_at: number
}
export type ImagePublication = {
  id: string
  asset_id: string
  user_id?: number
  status?: string
  title: string
  prompt?: string
  creator?: string
  is_owner?: boolean
  platform?: ImagePlatform
  model?: string
  aspect_ratio?: string
  image_url?: string
  published_at?: number
  created_at?: number
  expires_at: number
}
export type CursorPage<T> = { items: T[]; next_cursor: string }
export type ImageStorageProfile = {
  profile_id: string
  class: string
  backend: string
  provider: string
  root: string
  endpoint: string
  bucket: string
  region: string
  prefix: string
  active: boolean
  path_style: boolean
}
export type ImageChannelPool = {
  id?: number
  binding_key: string
  group: string
  platform: string
  mode: string
  model: string
  resolution: string
  channel_id: number
  /** Relative pool weight; the API retains its legacy field name. */
  priority: number
}
export type VideoReadiness = {
  ready: boolean
  checks: { key: string; ready: boolean; reason?: string }[]
}
export type VideoPluginTemplate = {
  provider: string
  name: string
  models: string[]
  video_profiles: VideoCapability[]
}
export type ImageAdminConfiguration = {
  routing_platforms?: string[]
  image_providers?: string[]
  media_providers?: string[]
  runtime: Record<string, string | number | boolean>
  storage_profiles: ImageStorageProfile[]
  policies: {
    group: string
    platform: string
    pool_mode: string
    enabled: boolean
    async_enabled: boolean
    models: string
    version: number
  }[]
  pools: ImageChannelPool[]
  storage_providers: string[]
  video_readiness?: VideoReadiness
  video_plugin_templates?: VideoPluginTemplate[]
}
export type ImagePoolSelectionOptions = {
  models: {
    id: string
    label: string
    media_types?: ('image' | 'video')[]
    channel_ids: number[]
  }[]
  channels: { id: number; name: string }[]
}
