/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.
*/
import { useQuery } from '@tanstack/react-query'
import { FileText, History } from 'lucide-react'
import { useState } from 'react'
import { useTranslation } from 'react-i18next'

import { SectionPageLayout } from '@/components/layout'
import { Alert, AlertDescription, AlertTitle } from '@/components/ui/alert'
import { Tabs, TabsContent, TabsList, TabsTrigger } from '@/components/ui/tabs'

import { getInvoiceConfig } from './api'
import { EligibleOrdersTable } from './components/eligible-orders-table'
import { InvoiceApplicationDialog } from './components/invoice-application-dialog'
import { InvoiceRequestsTable } from './components/invoice-requests-table'
import { formatInvoiceAmount, sumInvoiceOrders } from './lib/format'
import type { InvoiceEligibleOrder } from './types'

export function UserInvoicesPage() {
  const { t, i18n } = useTranslation()
  const config = useQuery({
    queryKey: ['invoice-config'],
    queryFn: getInvoiceConfig,
  })
  const [selectedOrders, setSelectedOrders] = useState<
    Map<number, InvoiceEligibleOrder>
  >(new Map())
  const [applicationOpen, setApplicationOpen] = useState(false)
  const totalAmountCents = sumInvoiceOrders(selectedOrders.values())
  const minAmountCents = config.data?.min_amount_cents ?? 0
  const enabled = config.data?.enabled ?? false
  const belowMinimum = totalAmountCents < minAmountCents

  return (
    <>
      <SectionPageLayout>
        <SectionPageLayout.Title>
          {t('Enterprise Invoices')}
        </SectionPageLayout.Title>
        <SectionPageLayout.Content>
          <div className='space-y-4'>
            <p className='text-muted-foreground text-sm'>
              {t(
                'Apply for enterprise invoices for paid CNY Epay recharge orders and review processing records.'
              )}
            </p>
            <Tabs defaultValue='apply' className='gap-4'>
              <TabsList>
                <TabsTrigger value='apply'>
                  <FileText aria-hidden='true' />
                  {t('Apply for invoice')}
                </TabsTrigger>
                <TabsTrigger value='records'>
                  <History aria-hidden='true' />
                  {t('Invoice records')}
                </TabsTrigger>
              </TabsList>

              <TabsContent value='apply' className='space-y-4'>
                {!config.isLoading && !enabled ? (
                  <Alert>
                    <AlertTitle>
                      {t('Invoice applications are currently disabled')}
                    </AlertTitle>
                    <AlertDescription>
                      {t(
                        'Existing invoice records remain available in the Invoice records tab.'
                      )}
                    </AlertDescription>
                  </Alert>
                ) : null}
                <Alert>
                  <AlertTitle>{t('Invoice selection')}</AlertTitle>
                  <AlertDescription>
                    {t(
                      '{{count}} orders selected, totaling {{amount}}. Minimum application amount: {{minimum}}.',
                      {
                        count: selectedOrders.size,
                        amount: formatInvoiceAmount(
                          totalAmountCents,
                          i18n.resolvedLanguage
                        ),
                        minimum: formatInvoiceAmount(
                          minAmountCents,
                          i18n.resolvedLanguage
                        ),
                      }
                    )}
                  </AlertDescription>
                </Alert>
                <EligibleOrdersTable
                  selectedOrders={selectedOrders}
                  onSelectedOrdersChange={setSelectedOrders}
                  actionLabel={t('Apply for invoice')}
                  actionDisabled={
                    !enabled || selectedOrders.size === 0 || belowMinimum
                  }
                  onAction={() => setApplicationOpen(true)}
                />
              </TabsContent>

              <TabsContent value='records'>
                <InvoiceRequestsTable />
              </TabsContent>
            </Tabs>
          </div>
        </SectionPageLayout.Content>
      </SectionPageLayout>

      <InvoiceApplicationDialog
        open={applicationOpen}
        onOpenChange={setApplicationOpen}
        selectedOrders={selectedOrders}
        totalAmountCents={totalAmountCents}
        minAmountCents={minAmountCents}
        onSelectionReset={() => setSelectedOrders(new Map())}
      />
    </>
  )
}
