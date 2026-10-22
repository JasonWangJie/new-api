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
import assert from 'node:assert/strict'

import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { createElement } from 'react'
import { afterEach, describe, expect, test, vi } from 'vitest'

import { api } from '@/lib/api'
import { useAuthStore } from '@/stores/auth-store'

import { TaskArtifactsCell } from '../components/task-artifacts'
import {
  getSafePluginAuthorUrl,
  parseTaskArtifactsResponse,
  resolveTaskPreviewMode,
  shouldLoadTaskArtifacts,
  TaskArtifactApiError,
} from '../lib/task-artifacts'
import type { TaskLog } from '../types'

const artifactAccessToken = `${'A'.repeat(41)}-_`

vi.mock('@/lib/api', () => ({ api: { get: vi.fn(), request: vi.fn() } }))

afterEach(() => {
  vi.resetAllMocks()
  vi.unstubAllGlobals()
  useAuthStore.getState().auth.reset()
})

function artifactContentUrl(
  artifactKey: string,
  baseUrl = 'https://media.example.com/media-prefix'
): string {
  return `${baseUrl}/v1/tasks/task-public/artifacts/${artifactKey}/content?access=${artifactAccessToken}`
}

function taskFixture(overrides: Partial<TaskLog> = {}): TaskLog {
  return {
    id: 1,
    user_id: 7,
    platform: 'openrouter',
    task_id: 'task-public',
    action: 'generate',
    channel_id: 3,
    group: 'default',
    quota: 100,
    submit_time: 1,
    status: 'SUCCESS',
    admin_info: {
      task_plugin: {
        key: 'openrouter-video',
        name: 'OpenRouter Video',
        version: '1.0.0',
      },
    },
    ...overrides,
  }
}

describe('task artifact projection', () => {
  test('enables projection only after a successful artifact viewer opens', () => {
    const successfulPluginTask = taskFixture()

    assert.equal(shouldLoadTaskArtifacts(successfulPluginTask, false), false)
    assert.equal(shouldLoadTaskArtifacts(successfulPluginTask, true), true)
    assert.equal(
      shouldLoadTaskArtifacts(taskFixture({ status: 'IN_PROGRESS' }), true),
      false
    )
    assert.equal(
      shouldLoadTaskArtifacts(taskFixture({ admin_info: undefined }), true),
      true
    )
  })

  test('accepts an empty artifact result without inventing a preview', () => {
    assert.deepEqual(
      parseTaskArtifactsResponse({
        success: true,
        data: { artifacts: [] },
      }),
      { artifacts: [] }
    )
  })

  test('preserves authentication on same-origin protected media results', () => {
    const artifact = {
      key: 'video',
      type: 'video',
      mime_type: 'video/mp4',
      authenticated: true,
      content_url: '/api/user/media-tasks/task-public/results/video/view',
    }
    expect(
      parseTaskArtifactsResponse({
        success: true,
        data: { artifacts: [artifact] },
      })
    ).toEqual({ artifacts: [artifact] })
  })

  test.each([
    'https://attacker.example/api/user/media-tasks/task-public/results/video/view',
    '//attacker.example/api/user/media-tasks/task-public/results/video/view',
    '/api/user/self',
    '/api/user/media-tasks/task-public/results/video/view?token=secret',
    '/api/user/media-tasks/task-public/results/video/view#fragment',
  ])('rejects authenticated media outside the protected route: %s', (url) => {
    expect(() =>
      parseTaskArtifactsResponse({
        success: true,
        data: {
          artifacts: [
            {
              key: 'video',
              type: 'video',
              authenticated: true,
              content_url: url,
            },
          ],
        },
      })
    ).toThrow(TaskArtifactApiError)
  })

  test('keeps stable absolute cross-origin content URLs', () => {
    assert.deepEqual(
      parseTaskArtifactsResponse({
        success: true,
        data: {
          artifacts: [
            {
              key: 'video-main',
              type: 'video',
              mime_type: 'video/mp4',
              content_url: artifactContentUrl('video-main'),
            },
            {
              key: 'poster~main',
              type: 'image',
              mime_type: 'image/webp',
              content_url: artifactContentUrl(
                'poster~main',
                'http://127.0.0.1:3001/nginx/tasks'
              ),
            },
            {
              key: 'result-file',
              type: 'file',
              content_url: artifactContentUrl(
                'result-file',
                'https://files.example.net'
              ),
            },
          ],
          legacy_content_url: artifactContentUrl(
            'video',
            'https://legacy-media.example.com/public'
          ),
        },
      }),
      {
        artifacts: [
          {
            key: 'video-main',
            type: 'video',
            mime_type: 'video/mp4',
            content_url: artifactContentUrl('video-main'),
          },
          {
            key: 'poster~main',
            type: 'image',
            mime_type: 'image/webp',
            content_url: artifactContentUrl(
              'poster~main',
              'http://127.0.0.1:3001/nginx/tasks'
            ),
          },
          {
            key: 'result-file',
            type: 'file',
            content_url: artifactContentUrl(
              'result-file',
              'https://files.example.net'
            ),
          },
        ],
        legacyContentUrl: artifactContentUrl(
          'video',
          'https://legacy-media.example.com/public'
        ),
      }
    )
  })

  test('rejects failed, malformed, or duplicate artifact results', () => {
    assert.throws(
      () =>
        parseTaskArtifactsResponse({
          success: false,
          message: 'plugin unavailable',
        }),
      TaskArtifactApiError
    )
    assert.throws(
      () =>
        parseTaskArtifactsResponse({
          success: true,
          data: {
            artifacts: [
              {
                key: 'video-main',
                type: 'video',
                content_url: artifactContentUrl('video-main'),
              },
              {
                key: 'video-main',
                type: 'image',
                content_url: artifactContentUrl('poster-main'),
              },
            ],
          },
        }),
      TaskArtifactApiError
    )
    assert.throws(
      () =>
        parseTaskArtifactsResponse({
          success: true,
          data: {
            artifacts: [
              {
                key: 'video:0',
                type: 'video',
                content_url: artifactContentUrl('video-main'),
              },
            ],
          },
        }),
      TaskArtifactApiError
    )
    assert.throws(
      () =>
        parseTaskArtifactsResponse({
          success: true,
          data: {
            artifacts: [
              {
                key: ' video-main',
                type: 'video',
                content_url: artifactContentUrl('video-main'),
              },
            ],
          },
        }),
      TaskArtifactApiError
    )
  })

  test('rejects unsafe or missing content URLs', () => {
    const validContentUrl = artifactContentUrl('video-main')
    const unsafeUrls: unknown[] = [
      undefined,
      'javascript:alert(1)',
      'data:text/plain,artifact',
      'https:media.example.com/task',
      '//media.example.com/task',
      `/v1/tasks/task-public/artifacts/video-main/content?access=${artifactAccessToken}`,
      '/\\media.example.com/task',
      validContentUrl.replace('https://', 'https://user:secret@'),
      validContentUrl.replace('https://', 'https://@'),
      `${validContentUrl}#fragment`,
      `${validContentUrl}#`,
      ` ${validContentUrl}`,
      `${validContentUrl}\n`,
      'https://media.example.com/video.mp4',
      `https://media.example.com/v1/videos/task-public/content?access=${artifactAccessToken}`,
      `https://media.example.com/v1/tasks/task-public/artifacts/video-main/content?token=${artifactAccessToken}`,
      `https://media.example.com/v1/tasks/task-public/artifacts/video-main/content?access=${'A'.repeat(42)}`,
      `${validContentUrl}&access=${artifactAccessToken}`,
      `${validContentUrl}&download=1`,
    ]

    for (const contentUrl of unsafeUrls) {
      assert.throws(
        () =>
          parseTaskArtifactsResponse({
            success: true,
            data: {
              artifacts: [
                {
                  key: 'video-main',
                  type: 'video',
                  content_url: contentUrl,
                },
              ],
            },
          }),
        TaskArtifactApiError
      )
    }

    assert.throws(
      () =>
        parseTaskArtifactsResponse({
          success: true,
          data: {
            artifacts: [],
            legacy_content_url: `https://media.example.com/v1/videos/task-public/content?access=${artifactAccessToken}`,
          },
        }),
      TaskArtifactApiError
    )
  })
})

describe('legacy task preview compatibility', () => {
  test('preserves old Suno and video previews without duplicating plugin previews', () => {
    assert.equal(
      resolveTaskPreviewMode(
        taskFixture({
          platform: 'suno',
          admin_info: undefined,
          data: [{ audio_url: 'https://media.example/audio.mp3' }],
        })
      ),
      'legacy-suno'
    )
    assert.equal(
      resolveTaskPreviewMode(
        taskFixture({
          admin_info: undefined,
          legacy_video_available: true,
        })
      ),
      'legacy-video'
    )
    assert.equal(
      resolveTaskPreviewMode(
        taskFixture({
          admin_info: undefined,
          legacy_video_available: true,
        }),
        true
      ),
      'plugin'
    )
    assert.equal(
      resolveTaskPreviewMode(
        taskFixture({
          legacy_video_available: true,
        })
      ),
      'plugin'
    )
    assert.equal(
      resolveTaskPreviewMode(
        taskFixture({
          status: 'FAILURE',
          legacy_video_available: true,
        })
      ),
      'none'
    )
    assert.equal(
      resolveTaskPreviewMode(
        taskFixture({
          admin_info: undefined,
          legacy_video_available: false,
        })
      ),
      'plugin'
    )
  })
})

describe('protected task artifact viewer', () => {
  test('loads private video bytes with the session and releases the preview URL', async () => {
    const contentUrl = '/api/user/media-tasks/task-public/results/video/view'
    const blob = new Blob(['private generated video'], { type: 'video/mp4' })
    const createObjectURL = vi.fn(() => 'blob:private-task-artifact')
    const revokeObjectURL = vi.fn()
    vi.stubGlobal(
      'URL',
      class extends URL {
        static createObjectURL = createObjectURL
        static revokeObjectURL = revokeObjectURL
      }
    )
    useAuthStore.getState().auth.setUser({ id: 7, username: 'owner', role: 1 })
    vi.mocked(api.get).mockResolvedValue({
      data: {
        success: true,
        data: {
          artifacts: [
            {
              key: 'video',
              type: 'video',
              authenticated: true,
              content_url: contentUrl,
            },
          ],
        },
      },
    })
    vi.mocked(api.request).mockResolvedValue({ data: blob })
    const client = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    })
    const view = render(
      createElement(
        QueryClientProvider,
        { client },
        createElement(TaskArtifactsCell, { log: taskFixture() })
      )
    )
    await userEvent
      .setup()
      .click(screen.getByRole('button', { name: 'Artifacts' }))
    const download = (await screen.findByText('Download')).closest('a')
    await waitFor(() =>
      expect(download).toHaveAttribute('href', 'blob:private-task-artifact')
    )
    expect(document.querySelector('video')).toHaveAttribute(
      'src',
      'blob:private-task-artifact'
    )
    expect(api.request).toHaveBeenCalledWith(
      expect.objectContaining({
        url: contentUrl,
        responseType: 'blob',
        headers: { Accept: 'video/*' },
      })
    )
    expect(createObjectURL).toHaveBeenCalledWith(blob)
    view.unmount()
    expect(revokeObjectURL).toHaveBeenCalledWith('blob:private-task-artifact')
    client.clear()
  })

  test('keeps ordinary signed artifact previews and downloads anonymous', async () => {
    const contentUrl = artifactContentUrl('video')
    vi.mocked(api.get).mockResolvedValue({
      data: {
        success: true,
        data: {
          artifacts: [{ key: 'video', type: 'video', content_url: contentUrl }],
        },
      },
    })
    const client = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    })
    const view = render(
      createElement(
        QueryClientProvider,
        { client },
        createElement(TaskArtifactsCell, { log: taskFixture() })
      )
    )
    await userEvent
      .setup()
      .click(screen.getByRole('button', { name: 'Artifacts' }))
    expect((await screen.findByText('Download')).closest('a')).toHaveAttribute(
      'href',
      contentUrl
    )
    expect(document.querySelector('video')).toHaveAttribute('src', contentUrl)
    expect(api.request).not.toHaveBeenCalled()
    view.unmount()
    client.clear()
  })

  test('shows a private transfer failure and retries without exposing a raw download', async () => {
    const contentUrl = '/api/admin/media-tasks/task-public/results/video/view'
    const blob = new Blob(['retried private video'], { type: 'video/mp4' })
    const revokeObjectURL = vi.fn()
    vi.stubGlobal(
      'URL',
      class extends URL {
        static createObjectURL = vi.fn(() => 'blob:retried-task-artifact')
        static revokeObjectURL = revokeObjectURL
      }
    )
    useAuthStore.getState().auth.setUser({ id: 9, username: 'admin', role: 10 })
    vi.mocked(api.get).mockResolvedValue({
      data: {
        success: true,
        data: {
          artifacts: [
            {
              key: 'video',
              type: 'video',
              authenticated: true,
              content_url: contentUrl,
            },
          ],
        },
      },
    })
    vi.mocked(api.request)
      .mockRejectedValueOnce(new Error('private transfer unavailable'))
      .mockResolvedValueOnce({ data: blob })
    const client = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    })
    const view = render(
      createElement(
        QueryClientProvider,
        { client },
        createElement(TaskArtifactsCell, { log: taskFixture() })
      )
    )
    const user = userEvent.setup()
    await user.click(screen.getByRole('button', { name: 'Artifacts' }))
    expect(
      await screen.findByText('Media preview failed. Please try again.')
    ).toBeVisible()
    expect(screen.getByText('Download').closest('a')).not.toHaveAttribute(
      'href'
    )
    expect(document.querySelector('video')).toBeNull()
    await user.click(screen.getByRole('button', { name: 'Retry' }))
    await waitFor(() =>
      expect(document.querySelector('video')).toHaveAttribute(
        'src',
        'blob:retried-task-artifact'
      )
    )
    expect(screen.getByText('Download').closest('a')).toHaveAttribute(
      'href',
      'blob:retried-task-artifact'
    )
    view.unmount()
    expect(revokeObjectURL).toHaveBeenCalledWith('blob:retried-task-artifact')
    client.clear()
  })
})

describe('plugin author links', () => {
  test('allows HTTP authors and rejects executable URL schemes', () => {
    assert.equal(
      getSafePluginAuthorUrl({
        name: 'Community Maintainer',
        url: 'https://plugins.example.com/maintainer',
      }),
      'https://plugins.example.com/maintainer'
    )
    assert.equal(
      getSafePluginAuthorUrl({
        name: 'Unsafe Maintainer',
        url: 'javascript:alert(1)',
      }),
      undefined
    )
  })
})
