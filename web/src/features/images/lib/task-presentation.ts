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
import type { ImageTask, ImageTaskList } from '../types'

export function imageTaskElapsedSeconds(
  task: ImageTask,
  now = Date.now() / 1000
): number | null {
  if (!task.created_at) return null
  return Math.max(0, (task.finished_at || now) - task.created_at)
}

export function imageTaskSpecifications(task: ImageTask): string {
  const requested = task.requested_resolution || task.requested_size
  if (requested && task.actual_size && requested !== task.actual_size) {
    return `${requested} → ${task.actual_size}`
  }
  return task.actual_size || requested || '—'
}

export function imageTaskSuccessRate(
  stats?: ImageTaskList['stats']
): number | null {
  const succeeded = stats?.succeeded || 0
  const finished = succeeded + (stats?.failed || 0)
  return finished > 0 ? (succeeded / finished) * 100 : null
}
