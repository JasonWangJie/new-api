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
  no_available_capacity: 'No available capacity',
  incompatible_image_route: 'The selected image route is incompatible',
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
  claimed: 'Task claimed',
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
  if (keys.some((key) => ['succeeded', 'completed', 'settled'].includes(key))) {
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
      ].includes(key)
    )
  ) {
    return 'bg-destructive'
  }
  return 'bg-blue-500'
}
