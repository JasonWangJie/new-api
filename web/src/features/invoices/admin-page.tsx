/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.
*/
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { FileClock, Search, ScrollText } from 'lucide-react'
import { useState } from 'react'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'

import { ConfirmDialog } from '@/components/confirm-dialog'
import { SectionPageLayout } from '@/components/layout'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { Card, CardContent } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { Tabs, TabsContent, TabsList, TabsTrigger } from '@/components/ui/tabs'
import { Textarea } from '@/components/ui/textarea'
import { searchUsers } from '@/features/users/api'
import type { User } from '@/features/users/types'
import { useDebounce } from '@/hooks'
import { hasPermission } from '@/lib/admin-permissions'
import { handleServerError } from '@/lib/handle-server-error'
import { createServerError } from '@/lib/server-error-message'
import { useAuthStore } from '@/stores/auth-store'

import { createHistoricalInvoice } from './api'
import { EligibleOrdersTable } from './components/eligible-orders-table'
import { InvoiceRequestsTable } from './components/invoice-requests-table'
import { formatInvoiceAmount, sumInvoiceOrders } from './lib/format'
import type { InvoiceEligibleOrder } from './types'

export function AdminInvoicesPage() {
  const { t, i18n } = useTranslation()
  const currentUser = useAuthStore((state) => state.auth.user)
  const canManage = hasPermission(currentUser, 'invoice', 'manage')
  const queryClient = useQueryClient()
  const [userKeyword, setUserKeyword] = useState('')
  const [selectedUser, setSelectedUser] = useState<User | null>(null)
  const [selectedOrders, setSelectedOrders] = useState<
    Map<number, InvoiceEligibleOrder>
  >(new Map())
  const [historyOpen, setHistoryOpen] = useState(false)
  const [note, setNote] = useState('')
  const debouncedUserKeyword = useDebounce(userKeyword.trim(), 300)

  const users = useQuery({
    queryKey: ['invoice-user-search', debouncedUserKeyword],
    queryFn: async () => {
      const result = await searchUsers({
        keyword: debouncedUserKeyword,
        p: 1,
        page_size: 10,
      })
      if (!result.success) throw createServerError(result)
      return result.data?.items ?? []
    },
    enabled: debouncedUserKeyword.length > 0,
    placeholderData: (previous) => previous,
  })
  const totalAmountCents = sumInvoiceOrders(selectedOrders.values())

  const history = useMutation({
    mutationFn: () => {
      if (!selectedUser) throw new Error(t('Select a user first'))
      return createHistoricalInvoice({
        user_id: selectedUser.id,
        order_ids: [...selectedOrders.keys()],
        note: note.trim(),
      })
    },
    onSuccess: async () => {
      toast.success(t('Historical invoice record created'))
      setHistoryOpen(false)
      setNote('')
      setSelectedOrders(new Map())
      await Promise.all([
        queryClient.invalidateQueries({ queryKey: ['invoice-requests'] }),
        queryClient.invalidateQueries({
          queryKey: ['invoice-eligible-orders'],
        }),
      ])
    },
    onError: async (error) => {
      handleServerError(error, t('Failed to create historical invoice record'))
      setHistoryOpen(false)
      setSelectedOrders(new Map())
      await queryClient.invalidateQueries({
        queryKey: ['invoice-eligible-orders'],
      })
    },
  })

  const selectUser = (user: User) => {
    setSelectedUser(user)
    setSelectedOrders(new Map())
    setUserKeyword('')
  }

  return (
    <SectionPageLayout>
      <SectionPageLayout.Title>
        {t('Invoice Management')}
      </SectionPageLayout.Title>
      <SectionPageLayout.Content>
        <div className='space-y-4'>
          <p className='text-muted-foreground text-sm'>
            {t(
              'Process invoice applications or record invoices issued before this feature was enabled.'
            )}
          </p>
          <Tabs defaultValue='requests' className='gap-4'>
            <TabsList>
              <TabsTrigger value='requests'>
                <ScrollText aria-hidden='true' />
                {t('Application records')}
              </TabsTrigger>
              <TabsTrigger value='history'>
                <FileClock aria-hidden='true' />
                {t('Historical supplement')}
              </TabsTrigger>
            </TabsList>

            <TabsContent value='requests'>
              <InvoiceRequestsTable admin canManage={canManage} />
            </TabsContent>

            <TabsContent value='history' className='space-y-4'>
              {!canManage ? (
                <Alert>
                  <AlertDescription>
                    {t('You have read-only access to invoice records.')}
                  </AlertDescription>
                </Alert>
              ) : null}
              <Card size='sm'>
                <CardContent className='space-y-3'>
                  <div className='flex flex-col gap-2 sm:flex-row'>
                    <div className='relative flex-1'>
                      <Search
                        aria-hidden='true'
                        className='text-muted-foreground absolute top-2 left-2.5 size-4'
                      />
                      <Input
                        value={userKeyword}
                        className='pl-8'
                        placeholder={t(
                          'Search by user ID, username, name or email...'
                        )}
                        onChange={(event) =>
                          setUserKeyword(event.currentTarget.value)
                        }
                      />
                    </div>
                  </div>
                  {userKeyword.trim() ? (
                    <div className='divide-y rounded-lg border'>
                      {users.isLoading ? (
                        <p className='text-muted-foreground px-3 py-3 text-sm'>
                          {t('Searching...')}
                        </p>
                      ) : null}
                      {users.data?.map((user) => (
                        <Button
                          key={user.id}
                          type='button'
                          variant='ghost'
                          className='h-auto w-full justify-between gap-3 rounded-none px-3 py-2 text-left'
                          onClick={() => selectUser(user)}
                        >
                          <span className='min-w-0'>
                            <span className='block truncate font-medium'>
                              {user.display_name || user.username}
                            </span>
                            <span className='text-muted-foreground block truncate text-xs'>
                              #{user.id} · {user.username} · {user.email || '-'}
                            </span>
                          </span>
                          <span className='text-primary shrink-0 text-xs'>
                            {t('Select')}
                          </span>
                        </Button>
                      ))}
                      {!users.isLoading && users.data?.length === 0 ? (
                        <p className='text-muted-foreground px-3 py-3 text-sm'>
                          {t('No users found')}
                        </p>
                      ) : null}
                    </div>
                  ) : null}
                  {selectedUser ? (
                    <div className='bg-muted/40 rounded-lg border px-3 py-2 text-sm'>
                      <span className='text-muted-foreground'>
                        {t('Selected user')}:{' '}
                      </span>
                      <span className='font-medium'>
                        {selectedUser.display_name || selectedUser.username}
                      </span>
                      <span className='text-muted-foreground'>
                        {' '}
                        (#{selectedUser.id} · {selectedUser.username})
                      </span>
                    </div>
                  ) : null}
                </CardContent>
              </Card>

              {selectedUser ? (
                <EligibleOrdersTable
                  key={selectedUser.id}
                  adminUserId={selectedUser.id}
                  selectedOrders={selectedOrders}
                  onSelectedOrdersChange={setSelectedOrders}
                  actionLabel={t('Mark as invoiced')}
                  actionDisabled={!canManage || selectedOrders.size === 0}
                  onAction={() => setHistoryOpen(true)}
                />
              ) : (
                <Alert>
                  <AlertDescription>
                    {t(
                      'Search for and select a user to view invoiceable recharge orders.'
                    )}
                  </AlertDescription>
                </Alert>
              )}
            </TabsContent>
          </Tabs>
        </div>
      </SectionPageLayout.Content>

      <ConfirmDialog
        open={historyOpen}
        onOpenChange={(open) => {
          if (!history.isPending) setHistoryOpen(open)
        }}
        title={t('Create historical invoice record?')}
        desc={t(
          'Confirm {{user}}, {{count}} orders and {{amount}}. This action marks the selected orders as invoiced.',
          {
            user: selectedUser?.display_name || selectedUser?.username || '-',
            count: selectedOrders.size,
            amount: formatInvoiceAmount(
              totalAmountCents,
              i18n.resolvedLanguage
            ),
          }
        )}
        confirmText={history.isPending ? t('Saving...') : t('Mark as invoiced')}
        isLoading={history.isPending}
        disabled={!selectedUser || selectedOrders.size === 0}
        handleConfirm={() => history.mutate()}
      >
        <div className='space-y-2'>
          <label className='text-sm font-medium' htmlFor='invoice-history-note'>
            {t('Admin note')}
          </label>
          <Textarea
            id='invoice-history-note'
            value={note}
            maxLength={1000}
            placeholder={t('Optional note for this historical invoice')}
            onChange={(event) => setNote(event.currentTarget.value)}
          />
        </div>
      </ConfirmDialog>
    </SectionPageLayout>
  )
}
