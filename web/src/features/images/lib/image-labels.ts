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
