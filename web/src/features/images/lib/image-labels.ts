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
  uploading: 'Saving images',
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
}
export function imageLabel(value: string): string {
  return IMAGE_LABELS[value] || value
}
