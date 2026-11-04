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
import {
  act,
  fireEvent,
  render,
  screen,
  waitFor,
  within,
} from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, beforeEach, expect, test, vi } from 'vitest'

import { getGroups } from '@/features/users/api'
import { useAuthStore } from '@/stores/auth-store'

import { imageRequest } from '../../api'
import type { ImageAdminConfiguration } from '../../types'
import { VideoRoutingForm, VideoRoutingSettings } from '../index'
import { VIDEO_ROUTING_URL } from '../policy'
import { VideoRoutingRuleEditor } from '../rule-editor'
import type { VideoRoutingConfig, VideoRoutingRule } from '../types'

vi.mock('../../api', () => ({ imageRequest: vi.fn() }))
vi.mock('@/features/users/api', () => ({ getGroups: vi.fn() }))

let client: QueryClient
const config: ImageAdminConfiguration = {
  runtime: {},
  policies: [],
  pools: [],
  storage_profiles: [],
  storage_providers: [],
  media_providers: ['drama'],
  routing_platforms: ['mixed', 'drama'],
}
const rule: VideoRoutingRule = {
  id: 'route-a',
  name: 'Long A to E',
  enabled: true,
  match: {
    groups: ['default'],
    models: ['seedance2.5-A'],
    resolutions: ['480p'],
    modes: ['text_to_video'],
    video_inputs: [],
    seconds: { min: 15, max: 30, min_exclusive: true, max_exclusive: false },
  },
  targets: [
    { channel_id: 20, model: 'seedance2.5-E', resolution: '720p' },
    { channel_id: 21, model: 'seedance2.5-E', resolution: '720p' },
  ],
  on_exhausted: 'fail',
}
const initial: VideoRoutingConfig = {
  version: 1,
  revision: 7,
  enabled: true,
  rules: [rule],
}

beforeEach(() => {
  client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  })
  vi.mocked(getGroups).mockResolvedValue({ success: true, data: ['default'] })
  vi.mocked(imageRequest).mockImplementation(async (url, method, data) => {
    if (url.includes('/options?')) {
      return {
        channels: [20, 21].map((id) => ({
          id,
          name: `Channel ${id}`,
          models: [{ model: 'seedance2.5-E', upstream_model: 'seedance2.5-E' }],
        })),
      } as never
    }
    if (method === 'PUT') {
      return { ...(data as VideoRoutingConfig), revision: 8 } as never
    }
    return initial as never
  })
})
afterEach(() => {
  client.clear()
  useAuthStore.getState().auth.reset()
})

function form(data = initial, canManage = true) {
  return (
    <QueryClientProvider client={client}>
      <VideoRoutingForm initial={data} config={config} canManage={canManage} />
    </QueryClientProvider>
  )
}

test('reopening settings after saving keeps the saved revision and rules', async () => {
  useAuthStore.getState().auth.setUser({ id: 1, username: 'admin', role: 100 })
  let persisted = structuredClone(initial)
  vi.mocked(imageRequest).mockImplementation(async (_url, method, data) => {
    if (method === 'PUT') {
      persisted = { ...(data as VideoRoutingConfig), revision: 8 }
    }
    return persisted as never
  })
  const user = userEvent.setup()
  const settings = (
    <QueryClientProvider client={client}>
      <VideoRoutingSettings config={config} />
    </QueryClientProvider>
  )
  const mounted = render(settings)
  await user.click(
    screen.getByRole('button', { name: 'Configure video routing rules' })
  )
  await user.click(
    await screen.findByRole('checkbox', { name: 'Enable rule Long A to E' })
  )
  await user.click(screen.getByRole('button', { name: 'Save routing rules' }))
  await waitFor(() =>
    expect(
      screen.getByRole('button', { name: 'Save routing rules' })
    ).not.toBeDisabled()
  )
  mounted.unmount()
  render(settings)
  await user.click(
    screen.getByRole('button', { name: 'Configure video routing rules' })
  )
  await waitFor(() =>
    expect(
      screen.getByRole('checkbox', { name: 'Enable rule Long A to E' })
    ).not.toBeChecked()
  )
})

test.each([200, 409, 500])(
  'returning during a save waits for its response and retains the submitted draft (status=%s)',
  async (status) => {
    useAuthStore
      .getState()
      .auth.setUser({ id: 1, username: 'admin', role: 100 })
    let complete!: () => void
    let persisted = structuredClone(initial)
    vi.mocked(imageRequest).mockImplementation(async (_url, method, data) => {
      if (method !== 'PUT') return persisted as never
      return new Promise((resolve, reject) => {
        complete = () => {
          if (status !== 200) {
            reject(
              Object.assign(new Error('Save unavailable'), {
                isAxiosError: true,
                response: { status },
              })
            )
            return
          }
          persisted = { ...(data as VideoRoutingConfig), revision: 8 }
          resolve(persisted as never)
        }
      })
    })
    const user = userEvent.setup()
    const settings = (
      <QueryClientProvider client={client}>
        <VideoRoutingSettings config={config} />
      </QueryClientProvider>
    )
    const mounted = render(settings)
    await user.click(
      screen.getByRole('button', { name: 'Configure video routing rules' })
    )
    await user.click(
      await screen.findByRole('checkbox', { name: 'Enable rule Long A to E' })
    )
    await user.click(screen.getByRole('button', { name: 'Save routing rules' }))
    mounted.unmount()
    render(settings)
    await user.click(
      screen.getByRole('button', { name: 'Configure video routing rules' })
    )
    const reenteredSave = screen.queryByRole('button', {
      name: 'Save routing rules',
    })
    const preventedConcurrentSave =
      !reenteredSave || reenteredSave.hasAttribute('disabled')
    await act(async () => complete())
    await waitFor(() =>
      expect(
        screen.getByRole('checkbox', { name: 'Enable rule Long A to E' })
      ).not.toBeChecked()
    )
    expect(preventedConcurrentSave).toBe(true)
    if (status === 409) {
      expect(
        screen.getByRole('button', { name: 'Reload saved settings' })
      ).toBeVisible()
    }
    if (status === 500) {
      expect(screen.getByRole('alert')).toHaveTextContent('Save unavailable')
    }
  }
)

test('editing a target rewrite preserves the original matching parameters and saves a versioned draft', async () => {
  const user = userEvent.setup()
  render(form())
  await user.click(screen.getByRole('button', { name: 'Edit' }))
  const dialog = screen.getByRole('dialog')
  expect(
    within(dialog).getByRole('combobox', { name: 'Requested models' })
  ).toBeVisible()
  expect(
    within(dialog).getByRole('combobox', { name: 'Input resolutions' })
  ).toBeVisible()
  expect(within(dialog).getByText('Request conditions')).toBeVisible()
  expect(
    within(dialog).getAllByRole('combobox', { name: 'Output resolution' })[0]
  ).toHaveTextContent('720p')
  fireEvent.change(
    within(dialog).getByRole('spinbutton', {
      name: 'Minimum duration (seconds)',
    }),
    { target: { value: '16' } }
  )
  await user.click(
    within(dialog).getByRole('button', { name: 'Move target 2 up' })
  )
  expect(
    within(dialog).getAllByRole('combobox', { name: 'Channel' })[0]
  ).toHaveFocus()
  await user.click(
    within(dialog).getByRole('button', { name: 'Apply to draft' })
  )
  expect(
    vi.mocked(imageRequest).mock.calls.some((call) => call[1] === 'PUT')
  ).toBe(false)
  await user.click(screen.getByRole('button', { name: 'Save routing rules' }))
  await waitFor(() =>
    expect(imageRequest).toHaveBeenCalledWith(
      VIDEO_ROUTING_URL,
      'PUT',
      expect.objectContaining({
        revision: 7,
        rules: [
          expect.objectContaining({
            match: expect.objectContaining({
              resolutions: ['480p'],
              seconds: expect.objectContaining({
                min: 16,
                min_exclusive: true,
              }),
            }),
            targets: [rule.targets[1], rule.targets[0]],
          }),
        ],
      })
    )
  )
})

test('duplicate, reorder, disable and delete operate on the draft until save', async () => {
  const user = userEvent.setup()
  render(form())
  await user.click(screen.getByRole('button', { name: 'Duplicate' }))
  await user.click(screen.getByRole('button', { name: 'Apply to draft' }))
  await user.click(
    screen.getByRole('button', { name: 'Move rule Long A to E (copy) up' })
  )
  expect(
    screen.getByRole('checkbox', { name: 'Enable rule Long A to E (copy)' })
  ).toHaveFocus()
  await user.click(
    screen.getByRole('checkbox', { name: 'Enable rule Long A to E (copy)' })
  )
  const original = screen.getByRole('row', {
    name: /^Long A to E seedance2.5-A /,
  })
  await user.click(within(original).getByRole('button', { name: 'Delete' }))
  await user.click(
    within(screen.getByRole('alertdialog')).getByRole('button', {
      name: 'Continue',
    })
  )
  await user.click(screen.getByRole('button', { name: 'Save routing rules' }))
  await waitFor(() =>
    expect(imageRequest).toHaveBeenCalledWith(
      VIDEO_ROUTING_URL,
      'PUT',
      expect.objectContaining({
        rules: [
          expect.objectContaining({
            id: expect.any(String),
            enabled: false,
            name: 'Long A to E (copy)',
            on_exhausted: 'fail',
          }),
        ],
      })
    )
  )
})

test.each(['Cancel', 'Continue'])(
  'closing a rule deletion dialog restores keyboard focus after %s',
  async (action) => {
    const user = userEvent.setup()
    render(form())
    screen.getByRole('button', { name: 'Delete' }).focus()
    await user.keyboard('{Enter}')
    const dialog = screen.getByRole('alertdialog')
    within(dialog).getByRole('button', { name: action }).focus()
    await user.keyboard('{Enter}')
    const expected = action === 'Cancel' ? 'Delete' : 'Add routing rule'
    await waitFor(() =>
      expect(screen.getByRole('button', { name: expected })).toHaveFocus()
    )
  }
)

test('a stale save keeps the local draft and revision until explicit reload', async () => {
  const user = userEvent.setup()
  const rendered = render(form())
  await user.click(
    screen.getByRole('checkbox', { name: 'Enable rule Long A to E' })
  )
  vi.mocked(imageRequest).mockRejectedValueOnce(
    Object.assign(new Error('stale configuration'), {
      isAxiosError: true,
      response: { status: 409 },
    })
  )
  await user.click(screen.getByRole('button', { name: 'Save routing rules' }))
  expect(
    await screen.findByRole('button', { name: 'Reload saved settings' })
  ).toBeVisible()
  rendered.rerender(form({ ...initial, revision: 9 }))
  expect(
    screen.getByRole('checkbox', { name: 'Enable rule Long A to E' })
  ).not.toBeChecked()
  expect(
    screen.getByRole('button', { name: 'Save routing rules' })
  ).toBeDisabled()
  vi.mocked(imageRequest).mockResolvedValueOnce({ ...initial, revision: 9 })
  await user.click(
    screen.getByRole('button', { name: 'Reload saved settings' })
  )
  expect(
    screen.getByRole('checkbox', { name: 'Enable rule Long A to E' })
  ).toBeChecked()
  await user.click(screen.getByRole('button', { name: 'Save routing rules' }))
  await waitFor(() =>
    expect(imageRequest).toHaveBeenLastCalledWith(
      VIDEO_ROUTING_URL,
      'PUT',
      expect.objectContaining({ revision: 9 })
    )
  )
})

test('invalid duration ranges stay in the editor and clearing a bound clears its exclusive flag', async () => {
  const user = userEvent.setup()
  const apply = vi.fn()
  render(
    <QueryClientProvider client={client}>
      <VideoRoutingRuleEditor
        rule={rule}
        config={config}
        onApply={apply}
        onClose={vi.fn()}
      />
    </QueryClientProvider>
  )
  fireEvent.change(
    screen.getByRole('spinbutton', { name: 'Maximum duration (seconds)' }),
    { target: { value: '15' } }
  )
  await user.click(screen.getByRole('button', { name: 'Apply to draft' }))
  expect(
    await screen.findByText(
      'Check the rule name, requested models, duration range and targets.'
    )
  ).toBeVisible()
  expect(apply).not.toHaveBeenCalled()
  fireEvent.change(
    screen.getByRole('spinbutton', { name: 'Minimum duration (seconds)' }),
    { target: { value: '' } }
  )
  expect(
    screen.getByRole('checkbox', { name: 'Exclude minimum' })
  ).toHaveAttribute('aria-disabled', 'true')
  expect(
    screen.getByRole('checkbox', { name: 'Exclude minimum' })
  ).not.toBeChecked()
  await user.click(screen.getByRole('button', { name: 'Apply to draft' }))
  await waitFor(() =>
    expect(apply).toHaveBeenCalledWith(
      expect.objectContaining({
        match: expect.objectContaining({
          seconds: expect.objectContaining({
            min: undefined,
            min_exclusive: false,
          }),
        }),
      })
    )
  )
})

test('draft preview shows original billing parameters and the rewritten execution target without saving', async () => {
  const user = userEvent.setup()
  render(form())
  await user.type(
    screen.getByRole('textbox', { name: 'Model' }),
    'seedance2.5-A'
  )
  await user.type(screen.getByRole('textbox', { name: 'Resolution' }), '480p')
  await user.type(
    screen.getByRole('spinbutton', { name: 'Duration (seconds)' }),
    '20'
  )
  vi.mocked(imageRequest).mockResolvedValueOnce({
    eligible: [],
    excluded: [],
    routing_groups: ['default'],
    conditional_routing: {
      rule_id: rule.id,
      rule_name: rule.name,
      matched_rules: [{ id: rule.id, name: rule.name }],
      source: { model: 'seedance2.5-A', resolution: '480p', seconds: 20 },
      targets: [
        {
          channel_id: 20,
          model: 'seedance2.5-E',
          upstream_model: 'seedance2.5-E',
          resolution: '720p',
          eligible: true,
          reasons: [],
        },
      ],
      on_exhausted: 'fail',
      is_fallback: false,
    },
  })
  await user.click(screen.getByRole('button', { name: 'Preview routing' }))
  expect(
    await screen.findByText(
      'Original request and billing: seedance2.5-A · 480p · 20 s'
    )
  ).toBeVisible()
  expect(screen.getByRole('cell', { name: '720p' })).toBeVisible()
  expect(imageRequest).toHaveBeenLastCalledWith(
    '/api/option/images/routing/preview',
    'POST',
    expect.objectContaining({
      model: 'seedance2.5-A',
      resolution: '480p',
      seconds: 20,
      video_routing_config: initial,
    })
  )
  expect(
    vi.mocked(imageRequest).mock.calls.some((call) => call[1] === 'PUT')
  ).toBe(false)
  await user.click(
    screen.getByRole('checkbox', { name: 'Enable conditional video routing' })
  )
  expect(
    screen.queryByText(
      'Original request and billing: seedance2.5-A · 480p · 20 s'
    )
  ).not.toBeInTheDocument()
  expect(screen.getByRole('textbox', { name: 'Model' })).toHaveValue(
    'seedance2.5-A'
  )
})

test('read-only access cannot edit rules or save changes', () => {
  render(form(initial, false))
  expect(screen.getByRole('button', { name: 'Edit' })).toBeDisabled()
  expect(
    screen.getByRole('button', { name: 'Add routing rule' })
  ).toBeDisabled()
  expect(
    screen.getByRole('button', { name: 'Save routing rules' })
  ).toBeDisabled()
  expect(
    screen.getByRole('checkbox', { name: 'Enable conditional video routing' })
  ).toHaveAttribute('aria-disabled', 'true')
})

test('a narrow viewport shows rule cards with working actions instead of a squeezed table', async () => {
  const matchMedia = window.matchMedia.bind(window)
  vi.spyOn(window, 'matchMedia').mockImplementation((query) => ({
    ...matchMedia(query),
    matches: query === '(max-width: 640px)' || matchMedia(query).matches,
  }))
  const user = userEvent.setup()
  render(form())
  expect(
    screen.queryByRole('columnheader', { name: 'Rule name' })
  ).not.toBeInTheDocument()
  expect(screen.getByText('Long A to E')).toBeVisible()
  expect(screen.getAllByRole('listitem')[0]).toHaveTextContent(
    '#20 · seedance2.5-E → 720p'
  )
  expect(
    screen.getByRole('checkbox', { name: 'Enable rule Long A to E' })
  ).toBeChecked()
  const enabled = screen.getByRole('checkbox', {
    name: 'Enable rule Long A to E',
  })
  enabled.focus()
  await user.keyboard(' ')
  expect(
    screen.getByRole('checkbox', { name: 'Enable rule Long A to E' })
  ).toHaveFocus()
  await user.click(screen.getByRole('button', { name: 'Edit' }))
  expect(screen.getByRole('dialog')).toBeVisible()
})

test.each([
  { resolutions: ['1080p'], warning: true },
  { resolutions: [], warning: true },
  { resolutions: ['480p', '1080p'], warning: true },
  { resolutions: ['480p', '720p'], warning: false },
])(
  'a 720p execution target warns about quality loss for $resolutions (warning=$warning)',
  ({ resolutions, warning }) => {
    render(
      <QueryClientProvider client={client}>
        <VideoRoutingRuleEditor
          rule={{
            ...rule,
            match: { ...rule.match, resolutions },
            targets: [rule.targets[0]],
          }}
          config={config}
          onApply={vi.fn()}
          onClose={vi.fn()}
        />
      </QueryClientProvider>
    )
    const message = screen.queryByText(
      'Some matching requests may be downgraded to this resolution. Billing still uses the original request resolution.'
    )
    if (warning) expect(message).toBeVisible()
    else expect(message).not.toBeInTheDocument()
  }
)

test('targets sharing a channel are rejected even when their model names differ', async () => {
  const user = userEvent.setup()
  const apply = vi.fn()
  const duplicateChannelRule = {
    ...rule,
    targets: [
      rule.targets[0],
      { ...rule.targets[0], model: 'another-video-model' },
    ],
  }
  render(
    <QueryClientProvider client={client}>
      <VideoRoutingRuleEditor
        rule={duplicateChannelRule}
        config={config}
        onApply={apply}
        onClose={vi.fn()}
      />
    </QueryClientProvider>
  )
  await user.click(screen.getByRole('button', { name: 'Apply to draft' }))
  expect(
    await screen.findByText('Targets in a rule must use different channels.')
  ).toBeVisible()
  expect(apply).not.toHaveBeenCalled()
})

test('a new rule can select a channel and model and is saved only after applying the draft', async () => {
  const user = userEvent.setup()
  render(form({ ...initial, rules: [] }))
  await user.click(screen.getByRole('button', { name: 'Add routing rule' }))
  const dialog = screen.getByRole('dialog')
  expect(
    within(dialog).getByRole('combobox', { name: 'When targets are exhausted' })
  ).toHaveTextContent('Return to original routing')
  await user.type(
    within(dialog).getByRole('textbox', { name: 'Rule name' }),
    'New route'
  )
  await user.click(within(dialog).getByRole('combobox', { name: 'Channel' }))
  await user.click(
    await screen.findByRole('option', { name: '#20 · Channel 20' })
  )
  await user.click(
    within(dialog).getByRole('combobox', { name: 'Target model' })
  )
  await user.click(await screen.findByRole('option', { name: 'seedance2.5-E' }))
  await user.click(
    within(dialog).getByRole('button', { name: 'Apply to draft' })
  )
  expect(screen.getByRole('cell', { name: 'New route All' })).toBeVisible()
  expect(
    vi.mocked(imageRequest).mock.calls.some((call) => call[1] === 'PUT')
  ).toBe(false)
  await user.click(screen.getByRole('button', { name: 'Save routing rules' }))
  await waitFor(() =>
    expect(imageRequest).toHaveBeenCalledWith(
      VIDEO_ROUTING_URL,
      'PUT',
      expect.objectContaining({
        rules: [
          expect.objectContaining({
            name: 'New route',
            on_exhausted: 'original',
            targets: [
              { channel_id: 20, model: 'seedance2.5-E', resolution: '' },
            ],
          }),
        ],
      })
    )
  )
})
