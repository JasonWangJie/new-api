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
export type RetryAction =
  | 'same_channel'
  | 'other_channel'
  | 'local_same_channel'
  | 'local_other_channel'
  | 'stop'
  | 'manual_review'
  | 'poll_same_job'
  | 'resume_download'
  | 'resume_billing'
export type RetryStep = {
  id: string
  action: RetryAction
  max_attempts: number
  delay_seconds: number
  backoff?: 'fixed' | 'exponential'
  max_delay_seconds?: number
}
export type RetryRule = {
  id: string
  name: string
  updated_at?: number
  enabled: boolean
  category: string
  scope: {
    media_types?: string[]
    providers?: string[]
    models?: string[]
    channel_ids?: number[]
    stages?: string[]
    categories?: string[]
  }
  match: {
    http_statuses?: number[]
    error_codes?: string[]
    contains_any?: string[]
    contains_all?: string[]
    excludes?: string[]
    regex?: string
  }
  max_attempts?: number
  steps: RetryStep[]
  on_exhausted: RetryAction
  allowed_channel_ids?: number[]
  excluded_channel_ids?: number[]
}
export type RetryPolicy = {
  schema_version?: number
  revision: number
  pause_resubmissions: boolean
  legacy_timeout_enabled: boolean
  legacy_timeout_scope_keys?: string[]
  legacy_timeout_config?: Record<string, number>
  legacy_video_channel_ids?: number[]
  rules: RetryRule[]
}
export type RetryFact = {
  media_type?: string
  provider?: string
  model?: string
  channel_id?: number
  stage?: string
  category?: string
  http_status?: number
  error_code?: string
  message: string
  unknown?: boolean
  accepted?: boolean
  terminal?: boolean
  has_output?: boolean
  has_upstream_task?: boolean
  has_references?: boolean
  local_supported?: boolean
  deadline?: number
}
export type RetryDecision = {
  matched: boolean
  rule_id?: string
  step_id?: string
  revision: number
  category?: string
  action: string
  requested_action?: string
  delay_seconds?: number
  blocked_reason?: string
  exhausted?: boolean
  rule_used?: number
  rule_limit?: number
  step_used?: number
  step_limit?: number
  total_used?: number
  total_limit?: number
}
export type RetryEligibility = {
  checked: boolean
  eligible: boolean
  reason?: string
  candidate_channel_ids?: number[]
  warnings?: string[]
}
export type RetryState = {
  total_used: number
  total_limit: number
  rule_uses?: Record<string, number>
  step_uses?: Record<string, number>
  now?: number
}
export type RetryVersion = {
  revision: number
  created_at: number
  actor_id: number
  action: string
  source_revision?: number
  policy?: RetryPolicy
}
export type RetrySample = {
  id?: number
  fingerprint: string
  media_type: string
  provider: string
  model: string
  channel_id: number
  stage: string
  http_status: number
  error_code: string
  message: string
  category: string
  count: number
  task_count: number
  last_seen: number
  rule_id?: string
  task_id?: string
  executed_count?: number
  succeeded_task_count?: number
}
export type RetryDraftSource = {
  task_id?: string
  event_id?: number
  fact: RetryFact
}
export type RetrySampleEvent = {
  id: number
  task_id: string
  created_at: number
  channel_id: number
  category: string
  message: string
  rule_id: string
  step_id: string
  revision: number
  action: string
  executed: boolean
  outcome: string
}
