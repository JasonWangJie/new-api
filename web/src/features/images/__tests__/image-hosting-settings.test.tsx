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
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, expect, test, vi } from 'vitest'

import { imageRequest } from '../api'
import { ImageHostingForm } from '../components/external-image-hosting-settings'
import type { ImageHostingConfiguration } from '../lib/image-hosting'

vi.mock('../api', () => ({ imageRequest: vi.fn() }))

afterEach(() => {
  vi.resetAllMocks()
})

const initial: ImageHostingConfiguration = {
  revision: 4,
  enabled: true,
  image_enabled: true,
  base_url: 'https://images.example.com',
  file_upload_path: '/api/v1/upload',
  url_upload_path: '/api/v1/upload/url',
  file_upload_method: 'POST',
  url_upload_method: 'POST',
  file_field: 'file',
  url_field: 'url',
  response_url_path: 'data.links.url',
  token_header: 'Authorization',
  token_prefix: 'Bearer ',
  extra_fields: { visibility: 'public' },
  timeout_seconds: 20,
  retries: 1,
  concurrency: 4,
  failure_threshold: 3,
  cooldown_seconds: 60,
  url_fallback: 'local',
  allow_private_network: false,
  has_token: true,
}

function renderForm(config = initial) {
  const client = new QueryClient({
    defaultOptions: { mutations: { retry: false } },
  })
  const onSaved = vi.fn()
  const onReload = vi.fn().mockResolvedValue(undefined)
  render(
    <QueryClientProvider client={client}>
      <ImageHostingForm
        initial={config}
        onSaved={onSaved}
        onReload={onReload}
      />
    </QueryClientProvider>
  )
  return { client, onSaved, onReload }
}

test('saving unchanged credentials preserves the saved token and configuration revision', async () => {
  const saved = { ...initial, revision: 5 }
  vi.mocked(imageRequest).mockResolvedValue(saved)
  const { onSaved } = renderForm()
  expect(screen.getByLabelText('Image host upload token')).toHaveValue('')
  expect(
    screen.getByText('An upload token is saved. Leave blank to keep it.')
  ).toBeVisible()
  expect(
    screen.getByText(
      'Allow cross-origin image reads from this site on the image host or CDN so previews and downloads work.'
    )
  ).toBeVisible()
  await userEvent
    .setup()
    .click(screen.getByRole('button', { name: 'Save image hosting settings' }))
  await waitFor(() => expect(onSaved).toHaveBeenCalledWith(saved))
  const request = vi.mocked(imageRequest).mock.calls[0]
  expect(request.slice(0, 2)).toEqual(['/api/option/images/hosting', 'PUT'])
  expect(request[2]).toMatchObject({
    revision: 4,
    base_url: 'https://images.example.com',
    token_prefix: 'Bearer ',
    extra_fields: { visibility: 'public' },
  })
  expect(request[2]).not.toHaveProperty('token')
  expect(request[2]).not.toHaveProperty('has_token')
  expect(request[2]).not.toHaveProperty('runtime')
})

test('replacement credentials are cleared after saving and never become mutation variables', async () => {
  vi.mocked(imageRequest).mockResolvedValue({ ...initial, revision: 5 })
  const { client, onSaved } = renderForm()
  const user = userEvent.setup()
  const token = screen.getByLabelText('Image host upload token')
  await user.type(token, 'new-upload-token')
  await user.clear(screen.getByLabelText('Upload token header'))
  await user.type(screen.getByLabelText('Upload token header'), 'X-API-Key')
  await user.clear(screen.getByLabelText('Upload token prefix'))
  await user.click(
    screen.getByRole('button', { name: 'Save image hosting settings' })
  )
  await waitFor(() => expect(onSaved).toHaveBeenCalledOnce())
  expect(vi.mocked(imageRequest).mock.calls[0][2]).toMatchObject({
    token: 'new-upload-token',
    token_header: 'X-API-Key',
    token_prefix: '',
  })
  expect(token).toHaveValue('')
  expect(client.getMutationCache().getAll()[0].state.variables).toBeUndefined()
})

test('removing the saved token requires an explicit switch and never submits a replacement', async () => {
  vi.mocked(imageRequest).mockResolvedValue({
    ...initial,
    enabled: false,
    has_token: false,
    revision: 5,
  })
  renderForm({ ...initial, enabled: false })
  const user = userEvent.setup()
  await user.type(
    screen.getByLabelText('Image host upload token'),
    'discarded-draft'
  )
  await user.click(
    screen.getByRole('switch', { name: 'Remove saved upload token' })
  )
  expect(screen.getByLabelText('Image host upload token')).toBeDisabled()
  expect(screen.getByLabelText('Image host upload token')).toHaveValue('')
  await user.click(
    screen.getByRole('button', { name: 'Save image hosting settings' })
  )
  await waitFor(() => expect(imageRequest).toHaveBeenCalledOnce())
  expect(vi.mocked(imageRequest).mock.calls[0][2]).toHaveProperty(
    'clear_token',
    true
  )
  expect(vi.mocked(imageRequest).mock.calls[0][2]).not.toHaveProperty('token')
})

test('upload failure fallback and bounded circuit controls are saved independently from video storage', async () => {
  vi.mocked(imageRequest).mockResolvedValue({
    ...initial,
    revision: 5,
    url_fallback: 'upstream',
    retries: 2,
  })
  renderForm()
  const user = userEvent.setup()
  await user.click(
    screen.getByRole('combobox', { name: 'URL upload failure fallback' })
  )
  await user.click(
    screen.getByRole('option', {
      name: 'Use the original upstream URL when eligible',
    })
  )
  const retries = screen.getByRole('spinbutton', { name: 'Image host retries' })
  expect(retries).toHaveAttribute('max', '3')
  await user.clear(retries)
  await user.type(retries, '2')
  await user.click(
    screen.getByRole('button', { name: 'Save image hosting settings' })
  )
  await waitFor(() => expect(imageRequest).toHaveBeenCalledOnce())
  expect(vi.mocked(imageRequest).mock.calls[0][2]).toMatchObject({
    url_fallback: 'upstream',
    retries: 2,
  })
  expect(vi.mocked(imageRequest).mock.calls[0][2]).not.toHaveProperty(
    'video_enabled'
  )
  expect(
    screen.getByText(/Asynchronous videos keep local storage/)
  ).toBeVisible()
})

test('invalid extra fields block submission and disabled hosting disables the generated-image toggle', async () => {
  renderForm({ ...initial, enabled: false })
  expect(
    screen.getByRole('switch', { name: 'Upload generated image results' })
  ).toHaveAttribute('aria-disabled', 'true')
  const user = userEvent.setup()
  const extra = screen.getByLabelText('Extra upload fields (JSON)')
  await user.clear(extra)
  await user.paste('{"visibility": {"public": true}}')
  await user.click(
    screen.getByRole('button', { name: 'Save image hosting settings' })
  )
  expect(await screen.findByRole('alert')).toHaveTextContent(
    'Extra upload fields must be a JSON object with string, number or boolean values.'
  )
  expect(extra).toHaveAttribute('aria-invalid', 'true')
  expect(imageRequest).not.toHaveBeenCalled()
})

test('configuration conflicts retain edits and credentials until the administrator explicitly reloads', async () => {
  const conflict = Object.assign(new Error('Configuration changed'), {
    isAxiosError: true,
    response: { status: 409 },
    config: { data: { token: 'replacement-token' } },
  })
  vi.mocked(imageRequest).mockRejectedValue(conflict)
  const { client, onSaved, onReload } = renderForm()
  const user = userEvent.setup()
  const token = screen.getByLabelText('Image host upload token')
  await user.type(token, 'replacement-token')
  await user.click(
    screen.getByRole('button', { name: 'Save image hosting settings' })
  )
  expect(
    await screen.findByRole('button', { name: 'Reload saved settings' })
  ).toBeVisible()
  expect(token).toHaveValue('replacement-token')
  expect(onSaved).not.toHaveBeenCalled()
  expect(onReload).not.toHaveBeenCalled()
  expect(
    JSON.stringify(client.getMutationCache().getAll()[0].state.error)
  ).not.toContain('replacement-token')
  await user.click(
    screen.getByRole('button', { name: 'Reload saved settings' })
  )
  await waitFor(() => expect(onReload).toHaveBeenCalledOnce())
})

test('extra upload fields preserve numeric and boolean JSON values for URL ingestion', async () => {
  vi.mocked(imageRequest).mockResolvedValue({ ...initial, revision: 5 })
  const { onSaved } = renderForm()
  const user = userEvent.setup()
  const extra = screen.getByLabelText('Extra upload fields (JSON)')
  await user.clear(extra)
  await user.paste(
    '{"visibility":"public","policy_id":3,"expires_in":3600,"enabled":true}'
  )
  await user.click(
    screen.getByRole('button', { name: 'Save image hosting settings' })
  )
  await waitFor(() => expect(onSaved).toHaveBeenCalledOnce())
  expect(vi.mocked(imageRequest).mock.calls[0][2]).toMatchObject({
    extra_fields: {
      visibility: 'public',
      policy_id: 3,
      expires_in: 3600,
      enabled: true,
    },
  })
})

test('a rejected save displays the server reason and keeps the unsaved token out of cached errors', async () => {
  vi.mocked(imageRequest).mockRejectedValue({
    isAxiosError: true,
    response: {
      status: 400,
      data: { success: false, message: 'Image host requires HTTPS' },
    },
    config: { data: { token: 'unsaved-upload-token' } },
  })
  const { client, onSaved } = renderForm()
  const user = userEvent.setup()
  const token = screen.getByLabelText('Image host upload token')
  await user.type(token, 'unsaved-upload-token')
  await user.click(
    screen.getByRole('button', { name: 'Save image hosting settings' })
  )
  expect(await screen.findByRole('alert')).toHaveTextContent(
    'Image host requires HTTPS'
  )
  expect(token).toHaveValue('unsaved-upload-token')
  expect(onSaved).not.toHaveBeenCalled()
  expect(
    JSON.stringify(client.getMutationCache().getAll()[0].state.error)
  ).not.toContain('unsaved-upload-token')
})
