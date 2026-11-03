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
import { getCoreRowModel, useReactTable } from '@tanstack/react-table'
import { render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { expect, it, vi } from 'vitest'

import { DataTableView, DataTableViewOptions } from '@/components/data-table'

import type { TaskLog } from '../../types'
import { useTaskLogsColumns } from '../columns/task-logs-columns'
import { UsageLogsMobileList } from '../usage-logs-mobile-card'
import { UsageLogsProvider } from '../usage-logs-provider'

const task: TaskLog = {
  id: 1,
  user_id: 7,
  platform: 'internal-video-provider',
  task_id: 'task_public_123',
  action: 'generate',
  channel_id: 3,
  group: 'default',
  quota: 100,
  submit_time: 1,
  status: 'FAILURE',
  admin_info: {
    task_plugin: {
      key: 'video-plugin',
      name: 'Video Plugin',
      version: '1.2.3',
    },
  },
}

function TaskLogsFixture(props: {
  admin: boolean
  root?: boolean
  mobile?: boolean
}) {
  const columns = useTaskLogsColumns(props.admin, props.root ?? false)
  const table = useReactTable({
    data: [task],
    columns,
    getCoreRowModel: getCoreRowModel(),
  })

  return (
    <UsageLogsProvider>
      <DataTableViewOptions table={table} />
      {props.mobile ? (
        <UsageLogsMobileList table={table} logCategory='task' />
      ) : (
        <DataTableView table={table} />
      )}
    </UsageLogsProvider>
  )
}

it.each([
  { view: 'self', admin: false, root: false },
  { view: 'admin', admin: true, root: false },
  { view: 'root', admin: true, root: true },
])(
  'shows the platform in task details only for elevated views ($view)',
  async (props) => {
    const user = userEvent.setup()
    render(<TaskLogsFixture {...props} />)

    await user.click(screen.getByRole('button', { name: 'View details' }))
    const dialog = await screen.findByRole('dialog', { name: /Task Details/ })
    expect(within(dialog).getByText(task.task_id)).toBeVisible()
    expect(within(dialog).getByText('Image to Video')).toBeVisible()
    if (props.admin) {
      expect(within(dialog).getByText('Platform')).toBeVisible()
      expect(within(dialog).getByText(task.platform)).toBeVisible()
    } else {
      expect(within(dialog).queryByText('Platform')).not.toBeInTheDocument()
      expect(within(dialog).queryByText(task.platform)).not.toBeInTheDocument()
      expect(within(dialog).queryByText('Video Plugin')).not.toBeInTheDocument()
    }
  }
)

it.each([
  { view: 'self desktop', admin: false, mobile: false },
  { view: 'admin desktop', admin: true, mobile: false },
  { view: 'self mobile', admin: false, mobile: true },
  { view: 'admin mobile', admin: true, mobile: true },
])(
  'limits plugin visibility and keeps a copyable ID without platform or action ($view)',
  async (props) => {
    const user = userEvent.setup()
    const copy = vi.spyOn(navigator.clipboard, 'writeText').mockResolvedValue()
    render(<TaskLogsFixture {...props} />)

    expect(
      screen.queryByText(task.platform, { exact: false })
    ).not.toBeInTheDocument()
    expect(screen.queryByText(/Image to Video/)).not.toBeInTheDocument()
    if (props.admin) {
      expect(screen.getByText('Video Plugin')).toBeVisible()
      expect(screen.getByText('Plugin')).toBeVisible()
    } else {
      expect(screen.queryByText('Video Plugin')).not.toBeInTheDocument()
      expect(screen.queryByText('Plugin')).not.toBeInTheDocument()
    }
    await user.click(screen.getByText(task.task_id))
    expect(copy).toHaveBeenCalledWith(task.task_id)

    await user.click(screen.getByRole('button', { name: 'View' }))
    const menu = await screen.findByRole('menu')
    if (props.admin) {
      expect(
        within(menu).getByRole('menuitemcheckbox', { name: 'Plugin' })
      ).toBeVisible()
    } else {
      expect(
        within(menu).queryByRole('menuitemcheckbox', { name: 'Plugin' })
      ).not.toBeInTheDocument()
    }
  }
)

it('removes the plugin column option when switching from an admin view to self', async () => {
  const user = userEvent.setup()
  const view = render(<TaskLogsFixture admin />)
  await user.click(screen.getByRole('button', { name: 'View' }))
  expect(
    await screen.findByRole('menuitemcheckbox', { name: 'Plugin' })
  ).toBeVisible()

  view.rerender(<TaskLogsFixture admin={false} />)
  expect(
    screen.queryByRole('menuitemcheckbox', { name: 'Plugin' })
  ).not.toBeInTheDocument()

  view.rerender(<TaskLogsFixture admin />)
  expect(screen.getByRole('menuitemcheckbox', { name: 'Plugin' })).toBeVisible()
})
