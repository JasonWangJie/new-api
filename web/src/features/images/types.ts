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
export type ImagePlatform = 'openai' | 'gemini'
export type ImageModel = {
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
  id: string
  task_id: string
  platform: ImagePlatform
  protocol: string
  model: string
  request_type: string
  status: string
  billing_status: string
  progress: number
  image_count: number
  result_count: number
  cost: number
  prompt_summary: string
  requested_size: string
  actual_size: string
  aspect_ratio: string
  group: string
  api_key_id: number
  user_id?: number
  channel_id?: number
  attempts?: string
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
export type ImageResult = {
  id: number
  image_index: number
  width: number
  height: number
  content_type: string
  byte_size: number
  checksum: string
  view_url: string
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
  stats: Record<string, number>
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
