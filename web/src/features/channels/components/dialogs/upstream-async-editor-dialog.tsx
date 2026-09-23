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
import { zodResolver } from '@hookform/resolvers/zod'
import { Braces, Check, CircleHelp, Plus, Trash2 } from 'lucide-react'
import { useEffect, useState, type ReactNode } from 'react'
import {
  Controller,
  useFieldArray,
  useForm,
  type UseFormRegisterReturn,
  type UseFormReturn,
} from 'react-hook-form'
import { useTranslation } from 'react-i18next'

import { CopyButton } from '@/components/copy-button'
import { Dialog } from '@/components/dialog'
import { JsonCodeEditor } from '@/components/json-code-editor'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import {
  Card,
  CardAction,
  CardContent,
  CardHeader,
  CardTitle,
} from '@/components/ui/card'
import { Checkbox } from '@/components/ui/checkbox'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import {
  Select,
  SelectContent,
  SelectGroup,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'
import { Tabs, TabsContent, TabsList, TabsTrigger } from '@/components/ui/tabs'
import { Textarea } from '@/components/ui/textarea'

import {
  createUpstreamAsyncProfileForm,
  upstreamAsyncConfigSchema,
  upstreamAsyncConfigToForm,
  upstreamAsyncEditorFormSchema,
  upstreamAsyncFormToConfig,
  UPSTREAM_ASYNC_IMAGE_EXAMPLE,
  UPSTREAM_ASYNC_MAI_IMAGE_EXAMPLE,
  UPSTREAM_ASYNC_VIDEO_EXAMPLE,
  type UpstreamAsyncEditorForm,
} from '../../lib/upstream-async'
import type { UpstreamAsyncConfig, UpstreamAsyncOperation } from '../../types'

type EditorTab = 'structured' | 'json' | 'examples'

export type UpstreamAsyncEditorDialogProps = {
  open: boolean
  value: UpstreamAsyncConfig | null
  onOpenChange: (open: boolean) => void
  onSave: (value: UpstreamAsyncConfig | null) => void
}

const mediaTypeOptions = [
  { value: 'video', label: 'Video' },
  { value: 'image', label: 'Image' },
] as const
const methodOptions = ['GET', 'POST'] as const
const operationOptions: UpstreamAsyncOperation[] = [
  'generate',
  'edit',
  'extend',
]

function profileOperationLabel(operation: UpstreamAsyncOperation): string {
  switch (operation) {
    case 'generate':
      return 'Generate'
    case 'edit':
      return 'Edit'
    case 'extend':
      return 'Extend'
  }
}

function FieldShell(props: {
  id: string
  label: string
  description?: string
  error?: string
  children: ReactNode
  className?: string
}) {
  const { t } = useTranslation()
  return (
    <div className={props.className || 'grid gap-2'}>
      <Label htmlFor={props.id}>{props.label}</Label>
      {props.children}
      {props.description ? (
        <p className='text-muted-foreground text-xs'>{props.description}</p>
      ) : null}
      {props.error ? (
        <p className='text-destructive text-xs'>{t(props.error)}</p>
      ) : null}
    </div>
  )
}

function ExampleCard(props: {
  title: string
  description: string
  value: UpstreamAsyncConfig
}) {
  const { t } = useTranslation()
  const json = JSON.stringify(props.value, null, 2)
  return (
    <Card size='sm'>
      <CardHeader>
        <CardTitle>{props.title}</CardTitle>
        <p className='text-muted-foreground text-xs'>{props.description}</p>
        <CardAction>
          <CopyButton
            value={json}
            variant='outline'
            size='sm'
            tooltip={t('Copy example')}
            aria-label={t('Copy {{type}} upstream async example', {
              type: props.title,
            })}
          >
            {t('Copy')}
          </CopyButton>
        </CardAction>
      </CardHeader>
      <CardContent>
        <pre className='bg-muted max-h-64 overflow-auto rounded-lg p-3 text-xs whitespace-pre-wrap'>
          {json}
        </pre>
      </CardContent>
    </Card>
  )
}

export function UpstreamAsyncEditorDialog(
  props: UpstreamAsyncEditorDialogProps
) {
  const { t } = useTranslation()
  const [activeTab, setActiveTab] = useState<EditorTab>('structured')
  const [jsonText, setJsonText] = useState(() =>
    JSON.stringify(
      props.value || { profiles: [UPSTREAM_ASYNC_VIDEO_EXAMPLE.profiles[0]] },
      null,
      2
    )
  )
  const [jsonError, setJsonError] = useState('')
  const form = useForm<UpstreamAsyncEditorForm>({
    resolver: zodResolver(upstreamAsyncEditorFormSchema),
    defaultValues: upstreamAsyncConfigToForm(props.value),
  })
  const profiles = useFieldArray({ control: form.control, name: 'profiles' })

  useEffect(() => {
    if (!props.open) return
    const values = upstreamAsyncConfigToForm(props.value)
    form.reset(values)
    setJsonText(
      JSON.stringify(props.value || upstreamAsyncFormToConfig(values), null, 2)
    )
    setJsonError('')
    setActiveTab('structured')
  }, [form, props.open, props.value])

  const parseJSONEditor = (): UpstreamAsyncConfig | null => {
    try {
      const decoded: unknown = JSON.parse(jsonText)
      const parsed = upstreamAsyncConfigSchema.safeParse(decoded)
      if (!parsed.success) {
        setJsonError(parsed.error.issues[0]?.message || t('Invalid JSON'))
        return null
      }
      setJsonError('')
      return parsed.data as UpstreamAsyncConfig
    } catch {
      setJsonError(t('Invalid JSON'))
      return null
    }
  }

  const switchTab = (nextTab: EditorTab) => {
    if (nextTab === activeTab) return
    if (activeTab === 'json' && nextTab === 'structured') {
      const config = parseJSONEditor()
      if (!config) return
      form.reset(upstreamAsyncConfigToForm(config))
    }
    if (activeTab === 'structured' && nextTab === 'json') {
      const parsedForm = upstreamAsyncEditorFormSchema.safeParse(
        form.getValues()
      )
      if (!parsedForm.success) {
        setJsonError(t('Fix structured form errors before editing JSON'))
        return
      }
      try {
        setJsonText(
          JSON.stringify(upstreamAsyncFormToConfig(parsedForm.data), null, 2)
        )
        setJsonError('')
      } catch {
        setJsonError(t('Fix structured form errors before editing JSON'))
        return
      }
    }
    setActiveTab(nextTab)
  }

  const saveStructured = (values: UpstreamAsyncEditorForm) => {
    try {
      props.onSave(upstreamAsyncFormToConfig(values))
      props.onOpenChange(false)
    } catch (error) {
      setJsonError(error instanceof Error ? error.message : t('Invalid value'))
    }
  }

  const save = () => {
    if (activeTab === 'json') {
      const config = parseJSONEditor()
      if (!config) return
      props.onSave(config)
      props.onOpenChange(false)
      return
    }
    void form.handleSubmit(saveStructured)()
  }

  const clear = () => {
    props.onSave(null)
    props.onOpenChange(false)
  }

  return (
    <Dialog
      open={props.open}
      onOpenChange={props.onOpenChange}
      title={t('Upstream asynchronous tasks')}
      description={t(
        'Override task ID, polling status, result URL, and usage parsing for matching image or video channels.'
      )}
      contentClassName='flex max-h-[92vh] flex-col gap-0 p-0 sm:max-w-6xl'
      headerClassName='border-b px-6 py-4'
      footerClassName='border-t px-6 py-4'
      contentHeight='72vh'
      footer={
        <>
          <Button type='button' variant='ghost' onClick={clear}>
            {t('Use built-in parsing')}
          </Button>
          <Button
            type='button'
            variant='outline'
            onClick={() => props.onOpenChange(false)}
          >
            {t('Cancel')}
          </Button>
          <Button type='button' onClick={save}>
            <Check aria-hidden='true' />
            {t('Save changes')}
          </Button>
        </>
      }
    >
      <Tabs
        value={activeTab}
        onValueChange={(value) => switchTab(value as EditorTab)}
        className='min-w-0 gap-0'
      >
        <div className='border-b px-4 py-3'>
          <TabsList className='grid h-auto w-full grid-cols-3 gap-1'>
            <TabsTrigger value='structured'>
              {t('Structured editor')}
              <Badge variant='outline'>{profiles.fields.length}</Badge>
            </TabsTrigger>
            <TabsTrigger value='json'>
              <Braces aria-hidden='true' />
              {t('Advanced JSON')}
            </TabsTrigger>
            <TabsTrigger value='examples'>
              <CircleHelp aria-hidden='true' />
              {t('Examples')}
            </TabsTrigger>
          </TabsList>
        </div>

        <TabsContent value='structured' className='space-y-4 p-4'>
          <div className='flex flex-wrap items-center justify-between gap-3'>
            <p className='text-muted-foreground text-xs'>
              {t(
                'Models match the mapped upstream model. Leave models empty to match every model on this channel.'
              )}
            </p>
            <Button
              type='button'
              variant='outline'
              size='sm'
              disabled={profiles.fields.length >= 32}
              onClick={() => profiles.append(createUpstreamAsyncProfileForm())}
            >
              <Plus aria-hidden='true' />
              {t('Add profile')}
            </Button>
          </div>

          {profiles.fields.map((profileField, index) => {
            const errors = form.formState.errors.profiles?.[index]
            const mediaType = form.watch(`profiles.${index}.media_type`)
            const submitMode = form.watch(`profiles.${index}.submit_mode`)
            return (
              <Card key={profileField.id} size='sm'>
                <CardHeader>
                  <CardTitle>
                    {t('Profile {{index}}', { index: index + 1 })}
                  </CardTitle>
                  <CardAction>
                    <Button
                      type='button'
                      variant='ghost'
                      size='icon'
                      disabled={profiles.fields.length === 1}
                      onClick={() => profiles.remove(index)}
                      aria-label={t('Remove profile {{index}}', {
                        index: index + 1,
                      })}
                    >
                      <Trash2 aria-hidden='true' />
                    </Button>
                  </CardAction>
                </CardHeader>
                <CardContent className='space-y-5'>
                  <div className='grid gap-4 md:grid-cols-3'>
                    <FieldShell
                      id={`upstream-async-${index}-id`}
                      label={t('Profile ID')}
                      error={errors?.id?.message}
                    >
                      <Input
                        id={`upstream-async-${index}-id`}
                        aria-invalid={Boolean(errors?.id)}
                        {...form.register(`profiles.${index}.id`)}
                      />
                    </FieldShell>
                    <FieldShell
                      id={`upstream-async-${index}-media`}
                      label={t('Media type')}
                      error={errors?.media_type?.message}
                    >
                      <Controller
                        control={form.control}
                        name={`profiles.${index}.media_type`}
                        render={({ field }) => (
                          <Select
                            items={mediaTypeOptions.map((option) => ({
                              value: option.value,
                              label: t(option.label),
                            }))}
                            value={field.value}
                            onValueChange={(value) => {
                              field.onChange(value)
                              if (value !== 'image') return
                              const operationPath =
                                `profiles.${index}.operations` as const
                              const operations = form.getValues(operationPath)
                              if (!operations.includes('extend')) return
                              form.setValue(
                                operationPath,
                                operations.filter(
                                  (operation) => operation !== 'extend'
                                ),
                                { shouldDirty: true, shouldValidate: true }
                              )
                            }}
                          >
                            <SelectTrigger id={`upstream-async-${index}-media`}>
                              <SelectValue />
                            </SelectTrigger>
                            <SelectContent alignItemWithTrigger={false}>
                              <SelectGroup>
                                {mediaTypeOptions.map((option) => (
                                  <SelectItem
                                    key={option.value}
                                    value={option.value}
                                  >
                                    {t(option.label)}
                                  </SelectItem>
                                ))}
                              </SelectGroup>
                            </SelectContent>
                          </Select>
                        )}
                      />
                    </FieldShell>
                    <FieldShell
                      id={`upstream-async-${index}-models`}
                      label={t('Mapped upstream models')}
                      description={t(
                        'Comma-separated; empty matches all models'
                      )}
                      error={errors?.models?.message}
                    >
                      <Input
                        id={`upstream-async-${index}-models`}
                        placeholder='vendor-video-model'
                        aria-invalid={Boolean(errors?.models)}
                        {...form.register(`profiles.${index}.models`)}
                      />
                    </FieldShell>
                  </div>

                  <div className='grid gap-2'>
                    <Label>{t('Operations')}</Label>
                    <Controller
                      control={form.control}
                      name={`profiles.${index}.operations`}
                      render={({ field }) => (
                        <div className='flex flex-wrap gap-4'>
                          {operationOptions.map((operation) => {
                            const checkboxID = `upstream-async-${index}-${operation}`
                            const unavailable =
                              mediaType === 'image' && operation === 'extend'
                            return (
                              <div
                                key={operation}
                                className='flex items-center gap-2'
                              >
                                <Checkbox
                                  id={checkboxID}
                                  disabled={unavailable}
                                  checked={field.value.includes(operation)}
                                  onCheckedChange={(checked) => {
                                    const next = checked
                                      ? [...field.value, operation]
                                      : field.value.filter(
                                          (item) => item !== operation
                                        )
                                    field.onChange(next)
                                  }}
                                />
                                <Label htmlFor={checkboxID}>
                                  {t(profileOperationLabel(operation))}
                                </Label>
                              </div>
                            )
                          })}
                        </div>
                      )}
                    />
                    {errors?.operations?.message ? (
                      <p className='text-destructive text-xs'>
                        {t(errors.operations.message)}
                      </p>
                    ) : null}
                  </div>

                  <div className='border-t pt-4'>
                    <p className='mb-3 font-medium'>{t('Submit parsing')}</p>
                    <FieldShell
                      id={`upstream-async-${index}-task-id-path`}
                      label={t('Task ID path')}
                      description={t(
                        'Restricted GJSON path in the submit response'
                      )}
                      error={errors?.task_id_path?.message}
                    >
                      <Input
                        id={`upstream-async-${index}-task-id-path`}
                        placeholder='data.task_id'
                        aria-invalid={Boolean(errors?.task_id_path)}
                        {...form.register(`profiles.${index}.task_id_path`)}
                      />
                    </FieldShell>
                  </div>

                  <div className='space-y-4 border-t pt-4'>
                    <p className='font-medium'>{t('Submit request')}</p>
                    <FieldShell
                      id={`upstream-async-${index}-submit-mode`}
                      label={t('Submit request mode')}
                      description={t(
                        'Default uses the channel adapter or task plugin request. Custom sends a POST to the configured path.'
                      )}
                    >
                      <Controller
                        control={form.control}
                        name={`profiles.${index}.submit_mode`}
                        render={({ field }) => (
                          <Select
                            items={[
                              { value: 'default', label: t('Default') },
                              { value: 'custom', label: t('Custom') },
                            ]}
                            value={field.value}
                            onValueChange={field.onChange}
                          >
                            <SelectTrigger
                              id={`upstream-async-${index}-submit-mode`}
                            >
                              <SelectValue />
                            </SelectTrigger>
                            <SelectContent alignItemWithTrigger={false}>
                              <SelectGroup>
                                <SelectItem value='default'>
                                  {t('Default')}
                                </SelectItem>
                                <SelectItem value='custom'>
                                  {t('Custom')}
                                </SelectItem>
                              </SelectGroup>
                            </SelectContent>
                          </Select>
                        )}
                      />
                    </FieldShell>
                    {submitMode === 'custom' ? (
                      <>
                        <FieldShell
                          id={`upstream-async-${index}-submit-path`}
                          label={t('Submit path')}
                          description={t(
                            'Relative to the channel Base URL; custom submit requests stay on the same origin.'
                          )}
                          error={errors?.submit_path?.message}
                        >
                          <Input
                            id={`upstream-async-${index}-submit-path`}
                            placeholder='/v1/videos'
                            aria-invalid={Boolean(errors?.submit_path)}
                            {...form.register(`profiles.${index}.submit_path`)}
                          />
                        </FieldShell>
                        <JSONTextField
                          id={`upstream-async-${index}-submit-query`}
                          label={t('Submit query templates')}
                          register={form.register(
                            `profiles.${index}.submit_query_json`
                          )}
                          error={errors?.submit_query_json?.message}
                        />
                        <div className='grid gap-4 lg:grid-cols-2'>
                          <JSONTextField
                            id={`upstream-async-${index}-submit-headers`}
                            label={t('Submit header templates')}
                            register={form.register(
                              `profiles.${index}.submit_headers_json`
                            )}
                            error={errors?.submit_headers_json?.message}
                          />
                          <JSONTextField
                            id={`upstream-async-${index}-submit-body`}
                            label={t('Submit JSON body template')}
                            description={t(
                              'Use {request.field} to copy a client field with its JSON type; leave empty to use the default request body.'
                            )}
                            register={form.register(
                              `profiles.${index}.submit_body_json`
                            )}
                            error={errors?.submit_body_json?.message}
                          />
                        </div>
                      </>
                    ) : null}
                  </div>

                  <div className='space-y-4 border-t pt-4'>
                    <p className='font-medium'>{t('Polling request')}</p>
                    <div className='grid gap-4 md:grid-cols-[10rem_1fr]'>
                      <FieldShell
                        id={`upstream-async-${index}-method`}
                        label={t('Method')}
                        error={errors?.method?.message}
                      >
                        <Controller
                          control={form.control}
                          name={`profiles.${index}.method`}
                          render={({ field }) => (
                            <Select
                              items={methodOptions.map((method) => ({
                                value: method,
                                label: method,
                              }))}
                              value={field.value}
                              onValueChange={field.onChange}
                            >
                              <SelectTrigger
                                id={`upstream-async-${index}-method`}
                              >
                                <SelectValue />
                              </SelectTrigger>
                              <SelectContent alignItemWithTrigger={false}>
                                <SelectGroup>
                                  {methodOptions.map((method) => (
                                    <SelectItem key={method} value={method}>
                                      {method}
                                    </SelectItem>
                                  ))}
                                </SelectGroup>
                              </SelectContent>
                            </Select>
                          )}
                        />
                      </FieldShell>
                      <FieldShell
                        id={`upstream-async-${index}-poll-path`}
                        label={t('Polling path')}
                        description={t(
                          'Relative to the frozen channel Base URL; {task_id} is URL-escaped.'
                        )}
                        error={errors?.poll_path?.message}
                      >
                        <Input
                          id={`upstream-async-${index}-poll-path`}
                          placeholder='/v1/tasks/{task_id}'
                          aria-invalid={Boolean(errors?.poll_path)}
                          {...form.register(`profiles.${index}.poll_path`)}
                        />
                      </FieldShell>
                    </div>
                    <div className='grid gap-4 lg:grid-cols-2'>
                      <JSONTextField
                        id={`upstream-async-${index}-query`}
                        label={t('Query templates')}
                        register={form.register(`profiles.${index}.query_json`)}
                        error={errors?.query_json?.message}
                      />
                      <JSONTextField
                        id={`upstream-async-${index}-headers`}
                        label={t('Request header templates')}
                        register={form.register(
                          `profiles.${index}.headers_json`
                        )}
                        error={errors?.headers_json?.message}
                      />
                      <JSONTextField
                        id={`upstream-async-${index}-body`}
                        label={t('JSON request body')}
                        description={t('Leave empty for no body')}
                        register={form.register(`profiles.${index}.body_json`)}
                        error={errors?.body_json?.message}
                        className='lg:col-span-2'
                      />
                    </div>
                  </div>

                  <div className='space-y-4 border-t pt-4'>
                    <p className='font-medium'>{t('Polling response')}</p>
                    <div className='grid gap-4 md:grid-cols-2'>
                      <PathField
                        form={form}
                        index={index}
                        name='status_path'
                        label={t('Status path')}
                        placeholder='data.status'
                        error={errors?.status_path?.message}
                      />
                      <PathField
                        form={form}
                        index={index}
                        name='result_path'
                        label={t('Result URL path')}
                        placeholder='data.output.url'
                        error={errors?.result_path?.message}
                      />
                      <PathField
                        form={form}
                        index={index}
                        name='progress_path'
                        label={t('Progress path')}
                        placeholder='data.progress'
                        error={errors?.progress_path?.message}
                      />
                      <PathField
                        form={form}
                        index={index}
                        name='failure_reason_path'
                        label={t('Failure reason path')}
                        placeholder='data.error.message'
                        error={errors?.failure_reason_path?.message}
                      />
                    </div>
                    <div className='grid gap-4 md:grid-cols-2'>
                      <JSONTextField
                        id={`upstream-async-${index}-queued`}
                        label={t('Queued values')}
                        register={form.register(
                          `profiles.${index}.queued_json`
                        )}
                        error={errors?.queued_json?.message}
                      />
                      <JSONTextField
                        id={`upstream-async-${index}-in-progress`}
                        label={t('In-progress values')}
                        register={form.register(
                          `profiles.${index}.in_progress_json`
                        )}
                        error={errors?.in_progress_json?.message}
                      />
                      <JSONTextField
                        id={`upstream-async-${index}-succeeded`}
                        label={t('Succeeded values')}
                        register={form.register(
                          `profiles.${index}.succeeded_json`
                        )}
                        error={errors?.succeeded_json?.message}
                      />
                      <JSONTextField
                        id={`upstream-async-${index}-failed`}
                        label={t('Failed values')}
                        register={form.register(
                          `profiles.${index}.failed_json`
                        )}
                        error={errors?.failed_json?.message}
                      />
                    </div>
                    <div className='grid gap-4 lg:grid-cols-2'>
                      <JSONTextField
                        id={`upstream-async-${index}-usage`}
                        label={t('Usage paths')}
                        register={form.register(
                          `profiles.${index}.usage_paths_json`
                        )}
                        error={errors?.usage_paths_json?.message}
                      />
                      <JSONTextField
                        id={`upstream-async-${index}-download-headers`}
                        label={t('Result download header templates')}
                        description={t(
                          'Credentialed downloads must stay on the channel origin.'
                        )}
                        register={form.register(
                          `profiles.${index}.download_headers_json`
                        )}
                        error={errors?.download_headers_json?.message}
                      />
                    </div>
                  </div>
                </CardContent>
              </Card>
            )
          })}
          {jsonError ? (
            <p className='text-destructive text-sm'>{t(jsonError)}</p>
          ) : null}
        </TabsContent>

        <TabsContent value='json' className='p-4'>
          <JsonCodeEditor
            value={jsonText}
            onChange={(value) => {
              setJsonText(value)
              setJsonError('')
            }}
            heightClassName='h-[520px] min-h-[520px] max-h-[520px]'
            ariaLabel={t('Upstream asynchronous task JSON')}
            aria-invalid={Boolean(jsonError)}
          />
          <p className='text-muted-foreground mt-2 text-xs'>
            {t(
              'Submit bodies also accept {request.field} placeholders for typed client fields.'
            )}
          </p>
          {jsonError ? (
            <p className='text-destructive mt-1 text-xs'>{t(jsonError)}</p>
          ) : null}
        </TabsContent>

        <TabsContent value='examples' className='grid gap-4 p-4 lg:grid-cols-2'>
          <ExampleCard
            title={t('Video single-result example')}
            description={t(
              'A video profile extracts one result URL after polling succeeds.'
            )}
            value={UPSTREAM_ASYNC_VIDEO_EXAMPLE}
          />
          <ExampleCard
            title={t('Image multi-result example')}
            description={t(
              'An image profile extracts a string array and maps final token usage.'
            )}
            value={UPSTREAM_ASYNC_IMAGE_EXAMPLE}
          />
          <ExampleCard
            title={t('Mai Token image example')}
            description={t(
              'An image task submits to the provider video path and downloads the completed image URL.'
            )}
            value={UPSTREAM_ASYNC_MAI_IMAGE_EXAMPLE}
          />
        </TabsContent>
      </Tabs>
    </Dialog>
  )
}

function JSONTextField(props: {
  id: string
  label: string
  description?: string
  error?: string
  register: UseFormRegisterReturn
  className?: string
}) {
  return (
    <FieldShell
      id={props.id}
      label={props.label}
      description={props.description}
      error={props.error}
      className={props.className ? `grid gap-2 ${props.className}` : undefined}
    >
      <Textarea
        id={props.id}
        className='min-h-20 font-mono text-xs'
        spellCheck={false}
        aria-invalid={Boolean(props.error)}
        {...props.register}
      />
    </FieldShell>
  )
}

type PathFieldName =
  | 'status_path'
  | 'result_path'
  | 'progress_path'
  | 'failure_reason_path'

function PathField(props: {
  form: UseFormReturn<UpstreamAsyncEditorForm>
  index: number
  name: PathFieldName
  label: string
  placeholder: string
  error?: string
}) {
  const id = `upstream-async-${props.index}-${props.name}`
  return (
    <FieldShell id={id} label={props.label} error={props.error}>
      <Input
        id={id}
        placeholder={props.placeholder}
        aria-invalid={Boolean(props.error)}
        {...props.form.register(`profiles.${props.index}.${props.name}`)}
      />
    </FieldShell>
  )
}
