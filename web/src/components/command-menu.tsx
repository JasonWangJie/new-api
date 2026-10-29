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
import { useLocation, useNavigate } from '@tanstack/react-router'
import {
  ArrowRight,
  ArrowUpRight,
  ChevronRight,
  Laptop,
  Moon,
  Sun,
} from 'lucide-react'
import React from 'react'
import { useTranslation } from 'react-i18next'

import {
  Command,
  CommandDialog,
  CommandEmpty,
  CommandGroup,
  CommandInput,
  CommandItem,
  CommandList,
  CommandSeparator,
} from '@/components/ui/command'
import { useTheme } from '@/context/theme-provider'
import { useSidebarData } from '@/hooks/use-sidebar-data'
import { cn } from '@/lib/utils'

import { getNavGroupsForPath } from './layout/lib/sidebar-view-registry'
import { ScrollArea } from './ui/scroll-area'

export function CommandMenu(props: {
  open: boolean
  onOpenChange: (open: boolean) => void
}) {
  const { t } = useTranslation()
  const navigate = useNavigate()
  const { setTheme } = useTheme()
  const { pathname } = useLocation()
  const sidebarData = useSidebarData()
  const onOpenChange = props.onOpenChange

  // Use the active nested sidebar view's nav groups when one matches
  // the current URL; otherwise fall back to the root navigation.
  const navGroups = getNavGroupsForPath(pathname, t) ?? sidebarData.navGroups

  const runCommand = React.useCallback(
    (command: () => unknown) => {
      onOpenChange(false)
      command()
    },
    [onOpenChange]
  )

  return (
    <CommandDialog modal open={props.open} onOpenChange={props.onOpenChange}>
      <Command>
        <CommandInput placeholder={t('Type a command or search...')} />
        <CommandList>
          <ScrollArea className='h-72 pe-1'>
            <CommandEmpty>{t('No results found.')}</CommandEmpty>
            {navGroups.map((group) => (
              <CommandGroup key={group.id || group.title} heading={group.title}>
                {group.items.map((navItem) => {
                  if (navItem.url) {
                    return (
                      <CommandItem
                        key={navItem.url}
                        value={navItem.title}
                        onSelect={() => {
                          runCommand(() => {
                            if (navItem.openInNewTab) {
                              window.open(
                                navItem.url,
                                '_blank',
                                'noopener,noreferrer'
                              )
                              return
                            }
                            void navigate({ to: navItem.url })
                          })
                        }}
                      >
                        <div className='flex size-4 items-center justify-center'>
                          <ArrowRight className='text-muted-foreground/80 size-2' />
                        </div>
                        <span
                          className={cn(
                            navItem.url === '/canvas' && 'canvas-entry-label'
                          )}
                        >
                          {navItem.title}
                        </span>
                        {navItem.openInNewTab && (
                          <>
                            <ArrowUpRight
                              className='text-muted-foreground ms-auto size-3'
                              aria-hidden='true'
                            />
                            <span className='sr-only'>
                              {t('Open in new tab')}
                            </span>
                          </>
                        )}
                      </CommandItem>
                    )
                  }

                  return navItem.items?.map((subItem) => (
                    <CommandItem
                      key={`${navItem.title}-${subItem.url}`}
                      value={`${navItem.title}-${subItem.url}`}
                      onSelect={() => {
                        runCommand(() => navigate({ to: subItem.url }))
                      }}
                    >
                      <div className='flex size-4 items-center justify-center'>
                        <ArrowRight className='text-muted-foreground/80 size-2' />
                      </div>
                      {navItem.title} <ChevronRight /> {subItem.title}
                    </CommandItem>
                  ))
                })}
              </CommandGroup>
            ))}
            <CommandSeparator />
            <CommandGroup heading='Theme'>
              <CommandItem onSelect={() => runCommand(() => setTheme('light'))}>
                <Sun /> <span>{t('Light')}</span>
              </CommandItem>
              <CommandItem onSelect={() => runCommand(() => setTheme('dark'))}>
                <Moon className='scale-90' />
                <span>{t('Dark')}</span>
              </CommandItem>
              <CommandItem
                onSelect={() => runCommand(() => setTheme('system'))}
              >
                <Laptop />
                <span>{t('System')}</span>
              </CommandItem>
            </CommandGroup>
          </ScrollArea>
        </CommandList>
      </Command>
    </CommandDialog>
  )
}
