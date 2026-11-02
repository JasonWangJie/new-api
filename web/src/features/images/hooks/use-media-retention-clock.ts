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
import { useEffect, useState } from 'react'

import { imageTaskResultExpired } from '../lib/task-presentation'
import type { ImageTask } from '../types'

export function useMediaRetentionClock(tasks?: readonly ImageTask[]): number {
  const [now, setNow] = useState(() => Date.now() / 1000)
  let nextExpiry = Infinity
  for (const task of tasks || []) {
    if (
      !imageTaskResultExpired(task, now) &&
      task.status === 'succeeded' &&
      Number.isFinite(task.expires_at) &&
      task.expires_at > now
    ) {
      nextExpiry = Math.min(nextExpiry, task.expires_at)
    }
  }

  useEffect(() => {
    if (!Number.isFinite(nextExpiry)) return
    const timer = window.setTimeout(
      () => setNow(Date.now() / 1000),
      // Browser timeouts wrap above the signed 32-bit boundary. Wake at the
      // cap and reschedule long retention periods rather than polling rows.
      Math.min(2_147_483_647, Math.max(1, nextExpiry * 1000 - Date.now()))
    )
    return () => window.clearTimeout(timer)
  }, [nextExpiry, now])

  return now
}
