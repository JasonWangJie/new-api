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

test('xAI examples explain native image geometry and duration-based async billing', () => {
  render(<AsyncImageGuide />)
  const image = screen.queryByRole('group', {
    name: 'xAI: generate a 2K image',
  })
  expect(image).not.toBeNull()
  expect(image).toHaveTextContent('"provider": "xai"')
  expect(image).toHaveTextContent('"resolution": "2K"')
  expect(image).toHaveTextContent('/v1/images/generations_async')
  const edit = screen.getByRole('group', {
    name: 'xAI: edit an image from a URL',
  })
  expect(edit).toHaveTextContent('/v1/images/edits_async')
  expect(edit).toHaveTextContent('"image"')
  const video = screen.getByRole('group', {
    name: 'xAI: generate a video from text',
  })
  expect(video).toHaveTextContent('"duration": 5')
  expect(video).toHaveTextContent('/v1/videos/generations_async')
  expect(
    screen.getByText(/Video billing uses valid video.duration/)
  ).toBeVisible()
  const references = screen.getByRole('group', {
    name: 'xAI 1.5: combine references and pinned frames',
  })
  expect(references).toHaveTextContent('"keyframes"')
  expect(references).toHaveTextContent('"timestamp_s": 2')
  expect(screen.getByText(/Generation defaults to 5 seconds/)).toBeVisible()
  expect(
    screen.getByText(/grok-imagine-image-quality and its grok-imagine alias/)
  ).toBeVisible()
})

test('xAI edit and extension examples inherit source geometry and distinguish extension billing', () => {
  render(<AsyncImageGuide />)
  const edit = screen.getByRole('group', {
    name: 'xAI: edit a video as a durable task',
  })
  const extension = screen.getByRole('group', {
    name: 'xAI: extend a video as a durable task',
  })
  expect(edit).toHaveTextContent('8.7')
  expect(extension).toHaveTextContent('/v1/videos/extensions_async')
  expect(extension).toHaveTextContent('additional seconds')
  for (const example of [edit, extension]) {
    const command = example.querySelector('pre code')?.textContent ?? ''
    const body = JSON.parse(command.match(/--data '([\s\S]*)'$/)?.[1] ?? '{}')
    expect(body).toMatchObject({
      provider: 'xai',
      model: 'grok-imagine-video',
      video: { url: 'https://cdn.example/source.mp4' },
    })
    expect(body).not.toHaveProperty('resolution')
    expect(body).not.toHaveProperty('aspect_ratio')
    expect(body).not.toHaveProperty('seconds')
    if (example === edit) expect(body).not.toHaveProperty('duration')
    else expect(body.duration).toBe(2)
  }
})

test.each(['E', 'F', 'G'])(
  'Drama %s provides text, image, video, audio and combined reference examples',
  (series) => {
    render(<AsyncImageGuide />)
    for (const scenario of [
      'generate a video from text',
      'use reference image URLs',
      'use a reference video URL',
      'use a reference audio URL',
      'combine image, video and audio references',
    ]) {
      const title = `Drama ${series}: ${scenario}`
      const example = screen.getByRole('group', { name: title })
      expect(example).toHaveTextContent(`seedance2.5-${series}`)
      expect(
        within(example).getByRole('button', { name: `Copy ${title}` })
      ).toBeEnabled()
      const code = example.querySelector('pre code')?.textContent ?? ''
      const body = JSON.parse(code.match(/--data '([\s\S]*)'$/)?.[1] ?? '{}')
      expect(body).toMatchObject({
        provider: 'drama',
        model: `seedance2.5-${series}`,
        resolution: '720p',
      })
      if (scenario === 'combine image, video and audio references') {
        expect(
          body.references.map((reference: { role: string }) => reference.role)
        ).toEqual(
          series === 'G'
            ? ['reference_image', 'reference_video', 'reference_audio']
            : ['reference', 'reference', 'reference']
        )
      }
    }
  }
)

test('video examples explain file uploads and URL references and copy only the selected command', async () => {
  render(<AsyncImageGuide />)
  const upload = screen.getByRole('group', {
    name: 'Sora: upload a local reference image',
  })
  expect(
    within(upload).getByRole('heading', {
      level: 3,
      name: 'Sora: upload a local reference image',
    })
  ).toBeVisible()
  expect(
    within(upload).getByText(/input_reference=@reference.png/)
  ).toBeVisible()
  expect(
    screen.getByRole('group', { name: 'xAI: generate from one image URL' })
  ).toHaveTextContent('output inherits the source image aspect ratio')
  expect(
    screen.getByRole('group', { name: 'Drama F: use reference image URLs' })
  ).toHaveTextContent('reference_images')
  expect(
    screen.getByRole('group', { name: 'Drama G: use a first-frame image URL' })
  ).toHaveTextContent('adaptive')

  const user = userEvent.setup()
  await user.click(
    within(upload).getByRole('button', {
      name: 'Copy Sora: upload a local reference image',
    })
  )
  expect(copyToClipboard).toHaveBeenCalledOnce()
  expect(copyToClipboard).toHaveBeenCalledWith(
    expect.stringContaining(
      "-F 'input_reference=@reference.png;type=image/png'"
    )
  )
  expect(vi.mocked(copyToClipboard).mock.calls[0][0]).not.toContain(
    'Sora: upload a local reference image'
  )
})

test('Drama A and H3 explain their separate reference rules and copy the paired-frame command', async () => {
  render(<AsyncImageGuide />)
  const numbered = screen.getByRole('group', {
    name: 'Drama A: reference images, video and audio by number',
  })
  expect(numbered).toHaveTextContent(
    'Every ordinary reference must be mentioned'
  )
  expect(numbered).toHaveTextContent('@图1')
  const h3 = screen.getByRole('group', {
    name: 'Drama minimax-h3: use a reference image without audio',
  })
  expect(h3).toHaveTextContent('numbered prompt references are optional')
  expect(h3).toHaveTextContent('generate_audio=false')
  const paired = screen.getByRole('group', {
    name: 'Drama A 2.0: provide paired first and last frames',
  })
  expect(paired).toHaveTextContent('aspect_ratio=Auto')
  const user = userEvent.setup()
  await user.click(
    within(paired).getByRole('button', {
      name: 'Copy Drama A 2.0: provide paired first and last frames',
    })
  )
  expect(copyToClipboard).toHaveBeenCalledWith(
    expect.stringContaining('"aspect_ratio": "Auto"')
  )
  expect(vi.mocked(copyToClipboard).mock.calls[0][0]).not.toContain(
    'This mode requires'
  )
})

test('Traditional Chinese shows translated video explanations while retaining the video section link', async () => {
  i18next.addResourceBundle('zhTW', 'translation', zhTW.translation)
  await i18next.changeLanguage('zhTW')
  render(<AsyncImageGuide />)

  const referenceExample = screen.getByRole('group', {
    name: 'Drama F：使用一般參考圖 URL',
  })
  expect(referenceExample).toHaveTextContent('不固定為首幀')
  expect(referenceExample).toHaveTextContent('reference_images')
  expect(
    screen.getByRole('link', {
      name: '非同步影片：生成、編輯、延長與本機儲存',
    })
  ).toHaveAttribute('href', '#image-guide-video')
  expect(
    screen.getByRole('group', { name: 'xAI：以持久化任務延長影片' })
  ).toHaveTextContent('新增 2–10 秒')
  expect(screen.getByText(/本站生成預設 5 秒/)).toBeVisible()
})
