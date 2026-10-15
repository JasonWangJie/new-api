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
  ).toHaveTextContent('image')
  expect(
    screen.getByRole('group', { name: 'Drama E/F: use reference image URLs' })
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

test('Traditional Chinese shows translated video explanations while retaining the video section link', async () => {
  i18next.addResourceBundle('zhTW', 'translation', zhTW.translation)
  await i18next.changeLanguage('zhTW')
  render(<AsyncImageGuide />)

  const referenceExample = screen.getByRole('group', {
    name: 'Drama E/F：使用一般參考圖 URL',
  })
  expect(referenceExample).toHaveTextContent('不固定為首幀')
  expect(referenceExample).toHaveTextContent('reference_images')
  expect(
    screen.getByRole('link', {
      name: '非同步影片：生成、編輯、延長與本機儲存',
    })
  ).toHaveAttribute('href', '#image-guide-video')
})
