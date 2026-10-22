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
import { Blob } from 'node:buffer'
import { webcrypto } from 'node:crypto'

import { afterEach, beforeEach, expect, test, vi } from 'vitest'

beforeEach(() => {
  sessionStorage.clear()
  vi.resetModules()
  vi.stubGlobal('Blob', Blob)
  vi.stubGlobal('crypto', webcrypto)
})
afterEach(() => vi.unstubAllGlobals())

test('reload reuses the pending submission key without persisting request content', async () => {
  const original = await import('../workbench-session')
  const body = JSON.stringify({ prompt: 'private image prompt' })
  const key = await original.workbenchSubmissionKey(
    101,
    'image',
    7,
    '/generate',
    body
  )
  const saved = sessionStorage.getItem('new-api:image-submission:v1:101')
  expect(JSON.parse(saved || '{}')).toEqual({
    key,
    fingerprint: expect.stringMatching(/^[a-f0-9]{64}$/),
  })
  expect(saved).not.toContain('private image prompt')
  vi.resetModules()
  const reloaded = await import('../workbench-session')
  expect(
    await reloaded.workbenchSubmissionKey(101, 'image', 7, '/generate', body)
  ).toBe(key)
})

test('accepted tasks restore only for their owner and a new generation gets a new key', async () => {
  const session = await import('../workbench-session')
  const first = await session.workbenchSubmissionKey(
    101,
    'video',
    7,
    '/generate',
    '{}'
  )
  session.rememberWorkbenchTask(101, 'video', 'video_accepted')
  expect(session.readWorkbenchTask(101, 'video')).toBe('video_accepted')
  expect(session.readWorkbenchTask(102, 'video')).toBe('')
  expect(session.readWorkbenchTask(101, 'image')).toBe('')
  expect(
    await session.workbenchSubmissionKey(101, 'video', 7, '/generate', '{}')
  ).not.toBe(first)
})

test('plain HTTP without SubtleCrypto keeps submission retries usable and persists only a digest', async () => {
  vi.stubGlobal('crypto', {
    getRandomValues: webcrypto.getRandomValues.bind(webcrypto),
  })
  const session = await import('../workbench-session')
  const key = await session.workbenchSubmissionKey(
    101,
    'video',
    7,
    '/generate',
    '{"prompt":"private"}'
  )
  expect(key).toHaveLength(21)
  expect(
    JSON.parse(
      sessionStorage.getItem('new-api:video-submission:v1:101') || '{}'
    )
  ).toEqual({ key, fingerprint: expect.stringMatching(/^[a-f0-9]{64}$/) })
  expect(
    await session.workbenchSubmissionKey(
      101,
      'video',
      7,
      '/generate',
      '{"prompt":"private"}'
    )
  ).toBe(key)
})

test('accepted submission clears its pending key even when browser storage is full', async () => {
  const session = await import('../workbench-session')
  const first = await session.workbenchSubmissionKey(
    101,
    'image',
    7,
    '/generate',
    '{}'
  )
  vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => {
    throw new Error('Storage full')
  })
  session.rememberWorkbenchTask(101, 'image', 'accepted')
  expect(sessionStorage.getItem('new-api:image-submission:v1:101')).toBeNull()
  expect(
    await session.workbenchSubmissionKey(101, 'image', 7, '/generate', '{}')
  ).not.toBe(first)
})

test('changing input or token creates a separate submission while storage failures preserve immediate retry', async () => {
  const session = await import('../workbench-session')
  vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => {
    throw new Error('Storage disabled')
  })
  const first = await session.workbenchSubmissionKey(
    101,
    'image',
    7,
    '/generate',
    '{}'
  )
  expect(
    await session.workbenchSubmissionKey(101, 'image', 7, '/generate', '{}')
  ).toBe(first)
  expect(
    await session.workbenchSubmissionKey(101, 'image', 8, '/generate', '{}')
  ).not.toBe(first)
  const changed = await session.workbenchSubmissionKey(
    101,
    'image',
    8,
    '/generate',
    '{"prompt":"new"}'
  )
  expect(
    await session.workbenchSubmissionKey(
      102,
      'image',
      8,
      '/generate',
      '{"prompt":"new"}'
    )
  ).not.toBe(changed)
  expect(() =>
    session.rememberWorkbenchTask(101, 'image', 'accepted')
  ).not.toThrow()
})
