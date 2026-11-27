/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as published by
the Free Software Foundation, either version 3 of the License, or
(at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program. If not, see <https://www.gnu.org/licenses/>.

For commercial licensing, please contact support@quantumnous.com
*/
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { act, fireEvent, render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { createInstance } from 'i18next'
import { I18nextProvider } from 'react-i18next'
import { expect, it, vi } from 'vitest'

import { convertDetectedLanguage } from '@/i18n/languages'
import zhTW from '@/i18n/locales/zh-TW.json'
import zhCN from '@/i18n/locales/zh.json'
import { api } from '@/lib/api'

import { channelSchema } from '../../types'
import { ChannelsProvider, useChannels } from '../channels-provider'
import { ChannelTestDialog } from '../dialogs/channel-test-dialog'
import { InteractiveChannelTest } from '../dialogs/interactive-channel-test'

function renderTestPanel(defaultModel?: string) {
  render(
    <QueryClientProvider client={new QueryClient()}>
      <InteractiveChannelTest
        channelId={42}
        defaultModel={defaultModel}
        endpointOptions={[]}
      />
    </QueryClientProvider>
  )
}

function OpenChannelTestDialog() {
  const { open, setOpen, setCurrentRow } = useChannels()
  return (
    <>
      <button
        type='button'
        onClick={() => {
          setCurrentRow(
            channelSchema.parse({
              id: 42,
              type: 1,
              key: '',
              status: 1,
              created_time: 0,
              test_time: 0,
              response_time: 0,
              balance_updated_time: 0,
              name: 'Test channel',
              models: 'chat-model,image-model',
              test_model: 'chat-model',
            })
          )
          setOpen('test-channel')
        }}
      >
        Open channel test
      </button>
      <ChannelTestDialog
        open={open === 'test-channel'}
        onOpenChange={() => setOpen(null)}
      />
    </>
  )
}

const chatModel = {
  model: 'chat-model',
  default_mode: 'chat',
  chat_endpoint_types: ['openai-response'],
}
const imageModel = {
  model: 'image-model',
  default_mode: 'image',
  image_endpoint_types: ['image-generation'],
}

it('opens interactive chat from the channel test dialog and keeps batch testing available', async () => {
  const user = userEvent.setup()
  const get = vi.spyOn(api, 'get').mockResolvedValue({
    data: { success: true, data: { models: [imageModel, chatModel] } },
  })
  const post = vi.spyOn(api, 'post').mockResolvedValue({
    data: {
      success: true,
      time: 0.215,
      data: {
        model: 'chat-model',
        mode: 'chat',
        endpoint_type: 'openai-response',
        text: 'Hello from the selected model.',
      },
    },
  })
  render(
    <QueryClientProvider client={new QueryClient()}>
      <ChannelsProvider>
        <OpenChannelTestDialog />
      </ChannelsProvider>
    </QueryClientProvider>
  )

  await user.click(screen.getByRole('button', { name: 'Open channel test' }))
  const dialog = await screen.findByRole('dialog')
  expect(
    within(dialog).getByRole('tab', { name: 'Interactive Test' })
  ).toHaveAttribute('aria-selected', 'true')
  const prompt = await within(dialog).findByRole('textbox', {
    name: 'Chat message',
  })
  expect(prompt).toHaveValue('hi')
  expect(
    within(dialog).queryByRole('textbox', { name: 'Reference image URLs' })
  ).not.toBeInTheDocument()
  await user.clear(prompt)
  await user.type(prompt, 'Say hello')
  await user.click(within(dialog).getByRole('button', { name: 'Start test' }))

  expect(
    await within(dialog).findByText('Hello from the selected model.')
  ).toBeVisible()
  expect(within(dialog).getByText('Response time: 215ms')).toBeVisible()
  expect(get).toHaveBeenCalledWith(
    '/api/channel/test/42/options',
    expect.any(Object)
  )
  expect(post).toHaveBeenCalledWith(
    '/api/channel/test/42',
    {
      model: 'chat-model',
      mode: 'chat',
      endpoint_type: 'openai-response',
      prompt: 'Say hello',
    },
    expect.any(Object)
  )

  await user.click(within(dialog).getByRole('tab', { name: 'Batch Test' }))
  expect(
    within(dialog).getByRole('button', { name: 'Test all 2 models' })
  ).toBeVisible()
})

it.each([
  {
    referenceText: ' \n  \n',
    references: {},
    scenario: 'with only blank reference lines',
  },
  {
    referenceText:
      ' https://example.com/first.png\r\n\r\nhttps://example.com/second.webp ',
    references: {
      image_urls: [
        'https://example.com/first.png',
        'https://example.com/second.webp',
      ],
    },
    scenario: 'with multiple reference URLs',
  },
])(
  'shows the returned image after running the image prompt $scenario',
  async ({ referenceText, references }) => {
    const user = userEvent.setup()
    const src = 'data:image/png;base64,iVBORw0KGgo='
    vi.spyOn(api, 'get').mockResolvedValue({
      data: { success: true, data: { models: [imageModel] } },
    })
    const post = vi.spyOn(api, 'post').mockResolvedValue({
      data: {
        success: true,
        data: {
          model: 'image-model',
          mode: 'image',
          endpoint_type: 'image-generation',
          image: { data_url: src, width: 1, height: 1 },
        },
      },
    })
    renderTestPanel()

    const prompt = await screen.findByRole('textbox', { name: 'Image prompt' })
    expect(prompt).not.toHaveValue('')
    fireEvent.change(prompt, { target: { value: 'A red boat' } })
    const referenceInput = screen.getByRole('textbox', {
      name: 'Reference image URLs',
    })
    fireEvent.change(referenceInput, { target: { value: referenceText } })
    await user.click(screen.getByRole('button', { name: 'Start test' }))

    expect(
      await screen.findByRole('img', { name: 'Generated image' })
    ).toHaveAttribute('src', src)
    expect(post).toHaveBeenCalledWith(
      '/api/channel/test/42',
      {
        model: 'image-model',
        mode: 'image',
        endpoint_type: 'image-generation',
        prompt: 'A red boat',
        ...references,
      },
      expect.any(Object)
    )
  }
)

it('prevents oversized prompts and allows a deliberate retry after upstream failure', async () => {
  const user = userEvent.setup()
  vi.spyOn(api, 'get').mockResolvedValue({
    data: { success: true, data: { models: [chatModel] } },
  })
  const post = vi
    .spyOn(api, 'post')
    .mockResolvedValueOnce({
      data: { success: false, message: 'Upstream quota exhausted', time: 0.08 },
    })
    .mockResolvedValueOnce({
      data: {
        success: true,
        data: {
          model: 'chat-model',
          mode: 'chat',
          endpoint_type: 'openai-response',
          text: 'Retried successfully',
        },
      },
    })
  renderTestPanel()

  const prompt = await screen.findByRole('textbox', { name: 'Chat message' })
  fireEvent.change(prompt, { target: { value: '😀'.repeat(4097) } })
  expect(screen.getByRole('alert')).toHaveTextContent(
    'Use at most 4096 characters.'
  )
  expect(screen.getByRole('button', { name: 'Start test' })).toBeDisabled()
  fireEvent.change(prompt, { target: { value: 'hi' } })

  await user.click(screen.getByRole('button', { name: 'Start test' }))
  const alert = await screen.findByRole('alert')
  expect(alert).toHaveTextContent('Upstream quota exhausted')
  expect(alert).toHaveTextContent('Response time: 80ms')
  expect(post).toHaveBeenCalledTimes(1)

  await user.click(screen.getByRole('button', { name: 'Retry' }))
  expect(await screen.findByText('Retried successfully')).toBeVisible()
})

it('localizes the dialogue and endpoint errors under the chosen interface language', async () => {
  const user = userEvent.setup()
  const language = createInstance()
  await language.init({
    lng: convertDetectedLanguage('zh-CN'),
    resources: { zhCN, zhTW },
    nsSeparator: false,
  })
  vi.spyOn(api, 'get').mockResolvedValue({
    data: {
      success: true,
      data: {
        models: [
          { ...chatModel, chat_endpoint_types: ['openai-response', 'openai'] },
        ],
      },
    },
  })
  vi.spyOn(api, 'post').mockResolvedValue({
    data: {
      success: false,
      message: 'Upstream returned HTTP 429: Supplier diagnostic',
    },
  })
  render(
    <I18nextProvider i18n={language}>
      <QueryClientProvider client={new QueryClient()}>
        <ChannelsProvider>
          <OpenChannelTestDialog />
        </ChannelsProvider>
      </QueryClientProvider>
    </I18nextProvider>
  )

  await user.click(screen.getByRole('button', { name: 'Open channel test' }))
  const dialog = await screen.findByRole('dialog')
  expect(within(dialog).getByRole('tab', { name: '模型实测' })).toBeVisible()
  const prompt = await within(dialog).findByRole('textbox', {
    name: '对话消息',
  })
  expect(prompt).toHaveValue('你好')
  expect(
    within(dialog).getByRole('combobox', { name: '端点类型' })
  ).toHaveValue('OpenAI 响应接口 (/v1/responses)')
  fireEvent.change(prompt, { target: { value: '我的問題' } })

  await act(() => language.changeLanguage(convertDetectedLanguage('zhTW')))
  expect(within(dialog).getByRole('tab', { name: '模型實測' })).toBeVisible()
  expect(within(dialog).getByRole('textbox', { name: '對話訊息' })).toHaveValue(
    '我的問題'
  )
  await user.click(within(dialog).getByRole('button', { name: '開始測試' }))

  const alert = await within(dialog).findByRole('alert')
  expect(alert).toHaveTextContent('上游傳回 HTTP 429 Supplier diagnostic')
})

it('localizes a TabTab test failure while keeping the supplier diagnostic visible', async () => {
  const user = userEvent.setup()
  const language = createInstance()
  await language.init({ lng: 'zhCN', resources: { zhCN } })
  vi.spyOn(api, 'get').mockResolvedValue({
    data: {
      success: true,
      data: { models: [{ ...imageModel, model: 'seedream-5.5' }] },
    },
  })
  vi.spyOn(api, 'post').mockResolvedValue({
    data: {
      success: false,
      error_code: 'channel_test_upstream_image_failed',
      message: 'Supplier diagnostic',
    },
  })
  render(
    <I18nextProvider i18n={language}>
      <QueryClientProvider client={new QueryClient()}>
        <InteractiveChannelTest channelId={43} endpointOptions={[]} />
      </QueryClientProvider>
    </I18nextProvider>
  )

  await user.click(await screen.findByRole('button', { name: '开始测试' }))
  expect(await screen.findByRole('alert')).toHaveTextContent(
    '上游生图任务失败。 Supplier diagnostic'
  )
})
