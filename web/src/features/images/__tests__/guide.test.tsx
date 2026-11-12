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
import { render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import i18next from 'i18next'
import { afterEach, expect, test, vi } from 'vitest'

import zhTW from '@/i18n/locales/zh-TW.json'
import { copyToClipboard } from '@/lib/copy-to-clipboard'

import { AsyncImageGuide } from '../guide'

vi.mock('@/hooks/use-status', () => ({
  useStatus: () => ({
    status: { server_address: 'https://gateway.example/v1/' },
  }),
}))
vi.mock('@/lib/copy-to-clipboard', () => ({
  copyToClipboard: vi.fn().mockResolvedValue(true),
}))

afterEach(async () => {
  await i18next.changeLanguage('en')
  i18next.removeResourceBundle('zhTW', 'translation')
})

test('one example per image or video scenario uses a model placeholder and copies only its request', async () => {
  render(<AsyncImageGuide />)
  const scenarios = [
    [
      'Generate an image from text',
      'YOUR_IMAGE_MODEL',
      '/v1/images/generations_async',
    ],
    [
      'Edit a reference image',
      'YOUR_IMAGE_EDIT_MODEL',
      '/v1/images/edits_async',
    ],
    [
      'Generate a video from text',
      'YOUR_VIDEO_MODEL',
      '/v1/videos/generations_async',
    ],
    [
      'Generate a video with reference media',
      'YOUR_VIDEO_REFERENCE_MODEL',
      '/v1/videos/generations_async',
    ],
    [
      'Generate a video between first and last frames',
      'YOUR_VIDEO_KEYFRAME_MODEL',
      '/v1/videos/generations_async',
    ],
    [
      'Edit an existing video',
      'YOUR_VIDEO_EDIT_MODEL',
      '/v1/videos/edits_async',
    ],
    [
      'Extend an existing video',
      'YOUR_VIDEO_EXTEND_MODEL',
      '/v1/videos/extensions_async',
    ],
  ]
  for (const [title, model, endpoint] of scenarios) {
    const example = screen.getByRole('group', { name: title })
    const command = example.querySelector('pre code')?.textContent ?? ''
    const body = JSON.parse(command.match(/--data '([\s\S]*)'$/)?.[1] ?? '{}')
    expect(command).toContain(`https://gateway.example${endpoint}`)
    expect(command).not.toContain('/v1/v1/')
    expect(body.model).toBe(model)
    expect(body).not.toHaveProperty('provider')
    expect(
      within(example).getByRole('button', { name: `Copy ${title}` })
    ).toBeEnabled()
  }
  const requests = screen
    .getAllByRole('group')
    .filter((group) =>
      group.querySelector('pre code')?.textContent?.includes('--data ')
    )
  expect(requests).toHaveLength(7)
  const edit = screen.getByRole('group', { name: 'Edit a reference image' })
  expect(edit).toHaveTextContent('"image": "https://cdn.example/source.png"')
  await userEvent
    .setup()
    .click(
      within(edit).getByRole('button', { name: 'Copy Edit a reference image' })
    )
  expect(copyToClipboard).toHaveBeenCalledWith(
    edit.querySelector('pre code')?.textContent
  )
})

test('output options state model scope and frame bounds without adding format-specific request cards', () => {
  render(<AsyncImageGuide />)
  expect(
    screen.getByText(/GIF, SVG and frame controls are limited/)
  ).toHaveTextContent('gpt-image-2.6-flare and gpt-image-2.6-sunburst')
  expect(screen.getByRole('row', { name: /frame_count/ })).toHaveTextContent(
    '2-16 frames, default 9'
  )
  expect(
    screen.getByRole('row', { name: /frame_duration_ms/ })
  ).toHaveTextContent('20-2000 milliseconds per frame, default 200')
  expect(
    screen.getByText(/Frame controls require output_format=gif/)
  ).toHaveTextContent('HTTP 400')
  expect(
    screen.getByText(/Frame values are requested settings/)
  ).toHaveTextContent('can differ upstream')
  expect(
    screen.getByText(/Poll query_url with the submitting API key/)
  ).toHaveTextContent('stage=completed and status=succeeded')
  expect(
    screen.getByText(/Statistics cover the authenticated user/)
  ).toHaveTextContent('across all API keys and workbench tasks')
  expect(
    screen.getByRole('group', { name: 'Completed image response' })
  ).toHaveTextContent('/v1/media/files/')
  expect(
    screen.getByRole('group', { name: 'Completed image response' })
  ).toHaveTextContent('https://images.example.com/i/example.png')
  expect(
    screen.getByText(/Image result links may use an external image host/)
  ).toBeVisible()
  expect(
    screen.getByText(/selected independently for each image/)
  ).toHaveTextContent('allow redirects')
  expect(
    screen.queryByRole('group', { name: /OpenAI|xAI|Drama|Sora|Doubao/ })
  ).not.toBeInTheDocument()
})

test('Traditional Chinese translates scenarios, output options and polling while retaining section links', async () => {
  i18next.addResourceBundle('zhTW', 'translation', zhTW.translation)
  await i18next.changeLanguage('zhTW')
  render(<AsyncImageGuide />)
  expect(
    screen.getByRole('group', {
      name: zhTW.translation['Generate an image from text'],
    })
  ).toHaveTextContent('YOUR_IMAGE_MODEL')
  expect(
    screen.getByRole('link', { name: zhTW.translation['Image output options'] })
  ).toHaveAttribute('href', '#image-guide-gif-svg')
  expect(
    screen.getByRole('link', {
      name: zhTW.translation['Video generation scenarios'],
    })
  ).toHaveAttribute('href', '#image-guide-video')
  expect(
    screen.getByText(
      zhTW.translation['GIF only: optional integer, 2-16 frames, default 9.']
    )
  ).toBeVisible()
  expect(
    screen.getByText(
      zhTW.translation[
        'Poll query_url with the submitting API key. Stages are queued, generating, saving, completed and failed. Download data[].url after stage=completed and status=succeeded, provided result_expired is not true and data is available.'
      ]
    )
  ).toBeVisible()
})
