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
import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { expect, test } from 'vitest'

import { ImagePolicyExampleDialog } from '../components/image-policy-example-dialog'

test('shows the model capability template for the selected platform', async () => {
  const user = userEvent.setup()
  const view = render(<ImagePolicyExampleDialog platform='openai' />)

  await user.click(
    screen.getByRole('button', {
      name: /OpenAI.*Configuration example/,
    })
  )
  const openAIDialog = screen.getByRole('dialog')
  expect(openAIDialog).toHaveTextContent('gpt-image-2')

  await user.click(within(openAIDialog).getByRole('button', { name: 'Close' }))
  await waitFor(() =>
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
  )
  view.rerender(<ImagePolicyExampleDialog platform='gemini' />)
  await user.click(
    screen.getByRole('button', {
      name: /Gemini.*Configuration example/,
    })
  )

  const dialog = screen.getByRole('dialog')
  expect(dialog).toHaveTextContent('gemini-3-pro-image-preview')
  expect(dialog).toHaveTextContent('gemini-2.5-flash-image')
  expect(dialog).not.toHaveTextContent('imagen-4.0-generate-001')
  expect(dialog).not.toHaveTextContent('gpt-image-2')
})

test('copies the complete example shown for the selected platform', async () => {
  const user = userEvent.setup()
  render(<ImagePolicyExampleDialog platform='gemini' />)

  await user.click(
    screen.getByRole('button', {
      name: /Gemini.*Configuration example/,
    })
  )
  const dialog = screen.getByRole('dialog')
  const example = within(dialog).getByRole('region', {
    name: 'Model capability example for Gemini',
  }).textContent

  await user.click(within(dialog).getByRole('button', { name: 'Copy example' }))

  expect(await navigator.clipboard.readText()).toBe(example)
})

test('uses a copy-ready placeholder for dynamically registered platforms', async () => {
  const user = userEvent.setup()
  render(<ImagePolicyExampleDialog platform='custom_media' />)

  await user.click(
    screen.getByRole('button', {
      name: /custom_media.*Configuration example/,
    })
  )

  expect(screen.getByRole('dialog')).toHaveTextContent(
    'custom_media-image-model'
  )
})
