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
export const IMAGE_LABELS: Record<string, string> = {
  token_required_for_auto_group:
    'Select an API key to preview automatic routing groups',
  policy_missing: 'No media policy configured',
  policy_disabled: 'Media policy is disabled',
  async_disabled: 'Background task APIs are disabled by this policy',
  synchronous_protocol_not_supported: 'Channel supports background tasks only or does not accept this synchronous API',
  model_not_in_policy: 'Model is not in the media policy',
  resolution_not_in_policy: 'Resolution is not in the media policy',
  mixed_pool_missing: 'No mixed channel pool binding is configured',
  token_disabled: 'API key is disabled',
  token_expired: 'API key has expired',
  token_exhausted: 'API key quota is exhausted',
  token_model_not_allowed: 'API key does not allow this model',
  token_group_not_allowed: 'API key does not allow this group',
  user_disabled: 'User is disabled',
  channel_disabled: 'Channel is disabled',
  model_not_enabled: 'Model is not enabled on this channel',
  platform_mismatch: 'Channel provider does not match',
  resolution_mapping_missing: 'Resolution mapping is missing',
  operation_not_supported: 'Generation mode is not supported',
  image_parameters_not_supported: 'Image parameters are not supported',
  video_parameters_not_supported: 'Video parameters are not supported',
  not_in_pool: 'Channel is outside this pool',
  no_enabled_account: 'No enabled account is available',
  lower_priority: 'A higher priority channel is available',
  zero_weight: 'Channel has zero weight',
  invalid_model_mapping: 'Model mapping is invalid',
  prepare: 'Request preparation',
  submit: 'Upstream submission',
  poll: 'Polling',
  storage: 'Saving media',
  postprocess: 'Output processing',
  download: 'Server download',
  billing: 'Confirming billing',
  scheduling: 'Scheduling',
  routing: 'Scheduling',
  recovery: 'Scheduling',
  wait_local_dependency: 'Waiting',
  routing_controls_unavailable: 'Waiting for image retry controls',
  supplier_retry_budget_unavailable: 'Waiting for image retry controls',
  quota_cache_unavailable: 'Image quota checks are temporarily unavailable',
  quota_cache_reservation_unknown:
    'Image quota reservation could not be confirmed',
  quota_confirmation_unknown: 'Image quota reservation could not be confirmed',
  quota_confirmed: 'Resolved',
  local_capacity_busy: 'Image capacity is temporarily unavailable',
  initial_submission: 'First submission',
  same_account_retry: 'Retry same account',
  switch_account: 'Switch account',
  retry_submission: 'Retry generation submission',
  poll_same_job: 'Query accepted upstream task',
  resume_output: 'Resume output processing',
  local_reference_fallback: 'Download references locally and retry',
  wait_account_cooldown: 'Wait for account cooldown',
  wait_supplier_budget: 'Wait for supplier retry budget',
  wait_capacity: 'Wait for capacity',
  manual_recovery: 'Manual recovery required',
  reconcile_unknown: 'Check upstream execution before retrying',
  stop: 'Stop automatic retries',
  eligible_account: 'Eligible account selected',
  reference_transport_fallback: 'Remote reference fetch failed',
  temporary_failure: 'Temporary failure is retryable',
  submission_rejected_without_diagnostic: 'Temporary failure is retryable',
  no_available_capacity: 'No available capacity',
  incompatible_image_route: 'The selected image route is incompatible',
  incompatible_image_reference: 'The selected image route is incompatible',
  compatibility_retry_budget_exhausted:
    'Image compatibility retry budget exhausted',
  no_compatible_account: 'No compatible image account is available',
  accepted_job: 'Upstream task already accepted',
  content_policy: 'Content policy rejection',
  invalid_input: 'Invalid input parameters',
  unknown_error: 'Unclassified failure',
  execution_deadline: 'Execution deadline reached',
  total_retry_budget_exhausted: 'Total retry budget exhausted',
  reference_retry_budget_exhausted: 'Reference retry budget exhausted',
  capacity_retry_budget_exhausted: 'Capacity retry budget exhausted',
  transient_retry_budget_exhausted: 'Temporary failure retry budget exhausted',
  storage_retry_budget_exhausted: 'Storage retry budget exhausted',
  billing_retry_budget_exhausted: 'Billing retry budget exhausted',
  account_switch_limit: 'Account switch limit reached',
  admin_terminated: 'Terminated by administrator',
  accepted_output: 'Upstream output already received',
  not_dispatched: 'Submission did not start',
  invalid_output: 'Invalid upstream output',
  supplier_retry_budget_exhausted: 'Supplier retry budget exhausted',
  account_cooldown: 'Account is cooling down',
  mixed: 'Mixed reference transport',
  unknown: 'Unknown',
  remote: 'Upstream URL fetch',
  original: 'Upstream URL fetch',
  local_download: 'Server download',
  retry_control_wait: 'Waiting for image retry controls',
  queued: 'Queued',
  invoking: 'Processing',
  processing: 'Processing',
  upstream_succeeded: 'Output received',
  uploading: 'Saving media',
  billing_pending: 'Confirming billing',
  storage_failed: 'Storage failed',
  billing_failed: 'Billing failed',
  succeeded: 'Succeeded',
  failed: 'Failed',
  expired: 'Expired',
  execution_unknown: 'Execution unknown',
  upstream_unconfirmed: 'Upstream execution could not be confirmed',
  pending: 'Pending',
  not_billable: 'No charge',
  pending_review: 'Pending review',
  approved_pending_sync: 'Approved, waiting for sync',
  synced: 'Synced',
  published: 'Published',
  hidden: 'Hidden',
  rejected: 'Rejected',
  withdrawn: 'Withdrawn',
  open: 'Open',
  resolved: 'Resolved',
  dismissed: 'Dismissed',
  running: 'Running',
  completed: 'Completed',
  completed_with_errors: 'Completed with errors',
  active: 'Active',
  copyright: 'Copyright',
  privacy: 'Privacy',
  illegal: 'Illegal content',
  inappropriate: 'Inappropriate content',
  other: 'Other',
  deleted: 'Deleted',
  user: 'User',
  async_results: 'Async results',
  all: 'All',
  total: 'Total',
  image_count: 'Images',
  openai: 'OpenAI',
  gemini: 'Gemini',
  text_to_image: 'Text to image',
  image_to_image: 'Image to image',
  text_to_video: 'Text to video',
  image_to_video: 'Image to video',
  reference_to_video: 'References to video',
  first_last_frame: 'First and last frame',
  edit_video: 'Edit video',
  extend_video: 'Extend video',
  video: 'Video',
  image: 'Images',
  generating: 'Generating',
  saving: 'Saving media',
  reserved: 'Reserved',
  reserving: 'Confirming billing',
  refunded: 'Refunded',
  openai_images: 'OpenAI',
  openai_video: 'Video',
  gemini_native: 'Gemini',
  not_charged: 'No charge',
  settled: 'Settled',
  bb: 'BB',
  sc: 'SC',
  temporary: 'Temporary inputs and results',
  durable: 'Durable images',
  resolution: 'Resolution',
  model: 'Model',
  model_resolution: 'Model and resolution',
  passthrough: 'Upstream URL fetch',
  local: 'Server download',
  passthrough_fallback_local: 'Upstream fetch with local fallback',
  accepted: 'Queued',
  selected: 'Selected',
  claimed: 'Task claimed',
  attempt_started: 'Channel selected',
  dispatched: 'Dispatched to upstream',
  submitted: 'Upstream task already accepted',
  upstream_failed: 'Invocation failed',
  retry_scheduled: 'Retry scheduled',
  reference_ready: 'Reference media ready',
  storage_saving: 'Saving media',
  storage_ready: 'Output received',
  storage_stored: 'Storage confirmed',
  storage_expired: 'Expired',
  storage_succeeded: 'Storage confirmed',
  storage_upstream: 'Upstream link',
  billing_reserving: 'Confirming billing',
  billing_reserved: 'Reserved',
  billing_settled: 'Settled',
  billing_refunded: 'Refunded',
  billing_unknown: 'Billing status unknown',
  billing_not_billable: 'No charge',
  reconciliation: 'Reconciliation status',
  resume_requested: 'Resumed',
  channel_selected: 'Channel selected',
  upstream_dispatched: 'Dispatched to upstream',
  invocation_failed: 'Invocation failed',
  storage_confirmed: 'Storage confirmed',
  billing_retry: 'Billing retry',
  recovered: 'Recovered to queue',
  resumed: 'Resumed',
  terminated: 'Terminated',
  postprocessing_failed: 'Post-processing failed',
}
export function imageLabel(value: string): string {
  return IMAGE_LABELS[value] || value
}

export function imageRoutingPlatforms(providers?: string[]): string[] {
  return [...new Set([...(providers || ['openai', 'gemini']), 'mixed'])]
}

export function imagePlatformLabel(value: string): string {
  return value === 'mixed' ? 'Mixed scheduling' : value
}
export function imageTimelineLabel(event: {
  event_type?: string
  status: string
}): string {
  const eventType = event.event_type?.trim()
  if (eventType && IMAGE_LABELS[eventType]) {
    return IMAGE_LABELS[eventType]
  }
  return imageLabel(event.status)
}

export function imageTimelineDotClass(event: {
  event_type?: string
  status: string
}): string {
  const keys = [event.event_type, event.status]
    .map((value) => value?.trim())
    .filter((value): value is string => Boolean(value))
  if (keys.some((key) => ['queued', 'accepted', 'recovered'].includes(key))) {
    return 'bg-warning'
  }
  if (
    keys.some((key) =>
      ['succeeded', 'completed', 'settled', 'billing_settled'].includes(key)
    )
  ) {
    return 'bg-success'
  }
  if (
    keys.some((key) =>
      [
        'failed',
        'expired',
        'execution_unknown',
        'invocation_failed',
        'terminated',
        'postprocessing_failed',
        'storage_failed',
        'billing_failed',
        'upstream_failed',
        'billing_unknown',
      ].includes(key)
    )
  ) {
    return 'bg-destructive'
  }
  return 'bg-blue-500'
}
