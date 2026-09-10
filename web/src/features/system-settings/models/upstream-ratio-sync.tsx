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
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { CheckSquare, Copy, RefreshCcw, Upload, X } from 'lucide-react'
import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'

import {
  Alert,
  AlertAction,
  AlertDescription,
  AlertTitle,
} from '@/components/ui/alert'
import { Button } from '@/components/ui/button'

import {
  fetchUpstreamRatios,
  getSystemOptions,
  getUpstreamChannels,
  updateSystemOption,
  uploadPricingCSV,
} from '../api'
import type {
  DifferencesMap,
  DisplayPriceLine,
  ModelParseIssue,
  RatioType,
  UpstreamChannel,
  UpstreamConfig,
} from '../types'
import { ChannelSelectorDialog } from './channel-selector-dialog'
import {
  ConflictConfirmDialog,
  type ConflictItem,
} from './conflict-confirm-dialog'
import {
  DEFAULT_ENDPOINT,
  MODELS_DEV_PRESET_ENDPOINT,
  MODELS_DEV_PRESET_ID,
  OFFICIAL_CHANNEL_ENDPOINT,
  OFFICIAL_CHANNEL_ID,
  OPENROUTER_CHANNEL_TYPE,
  OPENROUTER_ENDPOINT,
} from './constants'
import {
  NUMERIC_SYNC_FIELDS,
  RATIO_SYNC_FIELDS,
  applyResolutionRemovalPlan,
  applyResolutionSelection,
  applyResolutionSelections,
  deleteResolutionField,
  type ResolutionRemovalPlan,
  type ResolutionSelection,
  type ResolutionsMap,
} from './upstream-ratio-sync-helpers'
import { UpstreamRatioSyncTable } from './upstream-ratio-sync-table'

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

type UpstreamRatioSyncProps = {
  modelRatios: {
    ModelPrice: string
    ModelRatio: string
    CompletionRatio: string
    CacheRatio: string
    CreateCacheRatio: string
    ImageRatio: string
    AudioRatio: string
    AudioCompletionRatio: string
    'billing_setting.billing_mode': string
    'billing_setting.billing_expr': string
  }
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// The two synthesized presets always carry stable negative IDs assigned by
// `controller/ratio_sync.go`; matching by ID alone is sufficient and avoids
// fragile name/base_url comparisons.
function getDefaultEndpointForChannel(channel: UpstreamChannel): string {
  if (channel.id === MODELS_DEV_PRESET_ID) return MODELS_DEV_PRESET_ENDPOINT
  if (channel.id === OFFICIAL_CHANNEL_ID) return OFFICIAL_CHANNEL_ENDPOINT
  if (channel.type === OPENROUTER_CHANNEL_TYPE) return OPENROUTER_ENDPOINT
  return DEFAULT_ENDPOINT
}

function optionKeyBySyncField(ratioType: string): string {
  const explicit: Record<string, string> = {
    billing_mode: 'billing_setting.billing_mode',
    billing_expr: 'billing_setting.billing_expr',
  }
  if (explicit[ratioType]) return explicit[ratioType]
  return ratioType
    .split('_')
    .map((word) => word.charAt(0).toUpperCase() + word.slice(1))
    .join('')
}

function parseJsonRecord<T>(raw: string | undefined | null): Record<string, T> {
  try {
    return JSON.parse(raw || '{}') as Record<string, T>
  } catch {
    return {}
  }
}

// CSV 导入解析缺口原因码 → i18n 标签（后端 reason 格式为 "code: 详情"）
const PARSE_REASON_LABELS: Record<string, string> = {
  highest_tier_fallback: 'Charged at highest tier price',
  unsupported_condition: 'Unsupported combined condition',
  unknown_desc_type: 'Unrecognized description type',
  invalid_time_period: 'Invalid time period',
  time_label_mismatch: 'Inconsistent time period label',
  expr_compile_failed: 'Expression compile failed',
  no_pricing_produced: 'No pricing produced',
}

function parseReasonText(reason: string, t: (key: string) => string): string {
  const sep = reason.indexOf(': ')
  const code = sep >= 0 ? reason.slice(0, sep) : reason
  const detail = sep >= 0 ? reason.slice(sep + 2) : ''
  const label = PARSE_REASON_LABELS[code]
  const head = label ? t(label) : code
  return detail ? `${head}: ${detail}` : head
}

// ---------------------------------------------------------------------------
// Component
// ---------------------------------------------------------------------------

export function UpstreamRatioSync({ modelRatios }: UpstreamRatioSyncProps) {
  const { t } = useTranslation()
  const queryClient = useQueryClient()

  const [channelDialogOpen, setChannelDialogOpen] = useState(false)
  const [conflictDialogOpen, setConflictDialogOpen] = useState(false)
  const [selectedChannelIds, setSelectedChannelIds] = useState<number[]>([])
  const [channelEndpoints, setChannelEndpoints] = useState<
    Record<number, string>
  >({})
  const [differences, setDifferences] = useState<DifferencesMap>({})
  const [resolutions, setResolutions] = useState<ResolutionsMap>({})
  const [displayPrices, setDisplayPrices] = useState<
    Record<string, Record<string, string | DisplayPriceLine[]>>
  >({})
  const [conflictItems, setConflictItems] = useState<ConflictItem[]>([])
  const [confirmLoading, setConfirmLoading] = useState(false)
  const [skippedModels, setSkippedModels] = useState<string[]>([])
  const [parseIssues, setParseIssues] = useState<ModelParseIssue[]>([])

  const { data: channelsData } = useQuery({
    queryKey: ['upstream-channels'],
    queryFn: getUpstreamChannels,
    enabled: channelDialogOpen,
  })

  // Memoize the channels list so the effect below only re-runs when the query
  // data actually changes, instead of on every render (the `|| []` fallback
  // would otherwise produce a new array reference each render).
  const channels = useMemo(() => channelsData?.data ?? [], [channelsData?.data])

  useEffect(() => {
    if (channels.length === 0) return
    setChannelEndpoints((prev) => {
      let mutated = false
      const next = { ...prev }
      for (const channel of channels) {
        if (!next[channel.id]) {
          next[channel.id] = getDefaultEndpointForChannel(channel)
          mutated = true
        }
      }
      return mutated ? next : prev
    })
  }, [channels])

  const fetchMutation = useMutation({
    mutationFn: fetchUpstreamRatios,
    onSuccess: (data) => {
      if (!data.success) {
        toast.error(data.message || t('Failed to fetch upstream prices'))
        return
      }

      const { differences: diffs, test_results } = data.data

      const errorResults = test_results.filter((r) => r.status === 'error')
      if (errorResults.length > 0) {
        const errorMsg = errorResults
          .map((r) => `${r.name}: ${r.error}`)
          .join(', ')
        toast.warning(t('Some channels failed: {{errorMsg}}', { errorMsg }))
      }

      setDifferences(diffs)
      setResolutions({})
      setDisplayPrices({})
      setSkippedModels([])
      setParseIssues([])

      if (Object.keys(diffs).length === 0) {
        toast.success(t('No price differences found'))
      } else {
        toast.success(t('Upstream prices fetched successfully'))
      }
    },
    onError: (error: Error) => {
      toast.error(error.message || t('Failed to fetch upstream prices'))
    },
  })

  const csvFileInputRef = useRef<HTMLInputElement>(null)

  const csvUploadMutation = useMutation({
    mutationFn: uploadPricingCSV,
    onSuccess: (data) => {
      if (!data.success) {
        toast.error(data.message || t('Failed to parse pricing file'))
        return
      }

      const {
        differences: diffs,
        skipped_models: skippedModels,
        display_prices: csvDisplayPrices,
        parse_issues: parseIssues,
      } = data.data

      setDifferences(diffs)
      setResolutions({})
      setDisplayPrices(csvDisplayPrices ?? {})
      setSkippedModels(skippedModels ?? [])
      setParseIssues(parseIssues ?? [])

      if (skippedModels && skippedModels.length > 0) {
        toast.warning(
          t('Skipped {{count}} models that are not available in any enabled channel', {
            count: skippedModels.length,
          })
        )
      }

      if (parseIssues && parseIssues.length > 0) {
        toast.warning(
          t('{{count}} models have price rows that need attention', {
            count: parseIssues.length,
          })
        )
      }

      if (Object.keys(diffs).length === 0) {
        toast.success(t('No price differences found'))
      } else {
        toast.success(t('Pricing file parsed successfully'))
      }
    },
    onError: (error: Error) => {
      toast.error(error.message || t('Failed to parse pricing file'))
    },
  })

  const handleCSVUploadClick = () => {
    csvFileInputRef.current?.click()
  }

  const handleCopySkippedModels = () => {
    navigator.clipboard.writeText(skippedModels.join('\n'))
    toast.success(t('Copied'))
  }

  const handleCSVFileChange = (e: React.ChangeEvent<HTMLInputElement>) => {
    const file = e.target.files?.[0]
    if (!file) return
    const name = file.name.toLowerCase()
    if (!name.endsWith('.csv') && !name.endsWith('.xlsx')) {
      toast.error(t('Only .csv and .xlsx files are supported'))
      e.target.value = ''
      return
    }
    csvUploadMutation.mutate(file)
    e.target.value = ''
  }

  const { mutate: syncMutate, isPending: isSyncPending } = useMutation({
    mutationFn: async (updates: Array<{ key: string; value: string }>) => {
      for (const update of updates) {
        try {
          await updateSystemOption(update)
        } catch (err) {
          const msg = err instanceof Error ? err.message : String(err)
          throw new Error(`${update.key}: ${msg}`)
        }
      }
    },
    onSuccess: () => {
      toast.success(t('Prices synced successfully'))
      queryClient.invalidateQueries({ queryKey: ['system-options'] })

      setDifferences((prevDiffs) => {
        const newDiffs = { ...prevDiffs }
        Object.entries(resolutions).forEach(([model, ratios]) => {
          Object.keys(ratios).forEach((ratioType) => {
            if (newDiffs[model]?.[ratioType as RatioType]) {
              delete newDiffs[model][ratioType as RatioType]
              if (Object.keys(newDiffs[model]).length === 0) {
                delete newDiffs[model]
              }
            }
          })
        })
        return newDiffs
      })

      setResolutions({})
    },
    onError: (error: Error) => {
      toast.error(error.message || t('Failed to sync prices'))
    },
  })

  const handleOpenChannelDialog = () => {
    setChannelDialogOpen(true)
  }

  const handleConfirmChannelSelection = (selectedIds: number[]) => {
    const selectedChannels = channels.filter((ch) =>
      selectedIds.includes(ch.id)
    )

    if (selectedChannels.length === 0) {
      toast.warning(t('Please select at least one channel'))
      return
    }

    const upstreams: UpstreamConfig[] = selectedChannels.map((ch) => ({
      id: ch.id,
      name: ch.name,
      base_url: ch.base_url,
      endpoint: channelEndpoints[ch.id] || DEFAULT_ENDPOINT,
    }))

    fetchMutation.mutate({ upstreams, timeout: 10 })
  }

  const handleSelectValue = useCallback(
    (
      model: string,
      ratioType: RatioType,
      value: number | string,
      sourceName: string
    ) => {
      setResolutions((prev) =>
        applyResolutionSelection(prev, differences, {
          model,
          ratioType,
          value,
          sourceName,
        })
      )
    },
    [differences]
  )

  const handleSelectValues = useCallback(
    (selections: ResolutionSelection[]) => {
      if (selections.length === 0) return
      setResolutions((prev) =>
        applyResolutionSelections(prev, differences, selections)
      )
    },
    [differences]
  )

  const handleUnselectValue = useCallback(
    (model: string, ratioType: RatioType) => {
      setResolutions((prev) => deleteResolutionField(prev, model, ratioType))
    },
    []
  )

  const handleUnselectValues = useCallback((plan: ResolutionRemovalPlan) => {
    if (plan.size === 0) return
    setResolutions((prev) => applyResolutionRemovalPlan(prev, plan))
  }, [])

  const parseRatios = useCallback(
    (get: (key: string) => string | undefined) => ({
      ModelRatio: parseJsonRecord<number>(get('ModelRatio')),
      CompletionRatio: parseJsonRecord<number>(get('CompletionRatio')),
      CacheRatio: parseJsonRecord<number>(get('CacheRatio')),
      CreateCacheRatio: parseJsonRecord<number>(get('CreateCacheRatio')),
      ImageRatio: parseJsonRecord<number>(get('ImageRatio')),
      AudioRatio: parseJsonRecord<number>(get('AudioRatio')),
      AudioCompletionRatio: parseJsonRecord<number>(
        get('AudioCompletionRatio')
      ),
      ModelPrice: parseJsonRecord<number>(get('ModelPrice')),
      'billing_setting.billing_mode': parseJsonRecord<string>(
        get('billing_setting.billing_mode')
      ),
      'billing_setting.billing_expr': parseJsonRecord<string>(
        get('billing_setting.billing_expr')
      ),
    }),
    []
  )

  const parsedRatios = useMemo(() => {
    return parseRatios(
      (key) => modelRatios[key as keyof typeof modelRatios] as string
    )
  }, [modelRatios, parseRatios])

  type ParsedRatios = ReturnType<typeof parseRatios>

  const getLocalBillingCategory = (
    model: string,
    currentRatios: ParsedRatios
  ): 'price' | 'ratio' | 'tiered' | null => {
    if (currentRatios.ModelPrice[model] !== undefined) return 'price'
    if (
      currentRatios['billing_setting.billing_mode']?.[model] === 'tiered_expr'
    ) {
      return 'tiered'
    }
    if (
      currentRatios.ModelRatio[model] !== undefined ||
      currentRatios.CompletionRatio[model] !== undefined ||
      currentRatios.CacheRatio[model] !== undefined ||
      currentRatios.CreateCacheRatio[model] !== undefined ||
      currentRatios.ImageRatio[model] !== undefined ||
      currentRatios.AudioRatio[model] !== undefined ||
      currentRatios.AudioCompletionRatio[model] !== undefined
    ) {
      return 'ratio'
    }
    return null
  }

  const performSync = useCallback(
    async (): Promise<boolean> => {
      // 同步写入是全量覆盖整个倍率 map，必须以服务器最新配置为底组装，
      // 否则页面快照陈旧会把其他模型刚保存的折扣/倍率冲掉（乒乓重置）
      let currentRatios: ParsedRatios = parsedRatios
      try {
        const fresh = await queryClient.fetchQuery({
          queryKey: ['system-options'],
          queryFn: getSystemOptions,
          staleTime: 0,
        })
        const rawMap = new Map(
          (fresh?.data ?? []).map((o) => [o.key, o.value])
        )
        currentRatios = parseRatios((key) => rawMap.get(key))
      } catch {
        // 拉取失败时退回页面快照
      }

      const finalRatios: Record<string, Record<string, number | string>> = {
        ModelRatio: { ...currentRatios.ModelRatio },
        CompletionRatio: { ...currentRatios.CompletionRatio },
        CacheRatio: { ...currentRatios.CacheRatio },
        CreateCacheRatio: { ...currentRatios.CreateCacheRatio },
        ImageRatio: { ...currentRatios.ImageRatio },
        AudioRatio: { ...currentRatios.AudioRatio },
        AudioCompletionRatio: { ...currentRatios.AudioCompletionRatio },
        ModelPrice: { ...currentRatios.ModelPrice },
        'billing_setting.billing_mode': {
          ...currentRatios['billing_setting.billing_mode'],
        },
        'billing_setting.billing_expr': {
          ...currentRatios['billing_setting.billing_expr'],
        },
      }

      Object.entries(resolutions).forEach(([model, ratios]) => {
        const selectedTypes = Object.keys(ratios)
        const hasPrice = selectedTypes.includes('model_price')
        const hasRatio = selectedTypes.some((rt) =>
          RATIO_SYNC_FIELDS.includes(rt as RatioType)
        )

        if (hasPrice) {
          delete finalRatios.ModelRatio[model]
          delete finalRatios.CompletionRatio[model]
          delete finalRatios.CacheRatio[model]
          delete finalRatios.CreateCacheRatio[model]
          delete finalRatios.ImageRatio[model]
          delete finalRatios.AudioRatio[model]
          delete finalRatios.AudioCompletionRatio[model]
        }
        if (hasRatio) {
          delete finalRatios.ModelPrice[model]
        }

        // When applying expression billing (tiered), clean up all legacy ratio/model_price entries.
        const hasBillingExpr = selectedTypes.some(
          (rt) => rt === 'billing_mode' || rt === 'billing_expr'
        )
        if (hasBillingExpr) {
          // 注意：不动 ModelRatio——表达式计费下它是折扣位。
          // 同选折扣时由后续循环写入新值；未同选时保留服务器现值，
          // 避免"只同步表达式却把已配折扣冲成 1"。旧基础倍率残留由后端
          // GetTieredModelRatioDiscount 的默认倍率表排除逻辑兜底（视为无折扣）。
          delete finalRatios.CompletionRatio[model]
          delete finalRatios.CacheRatio[model]
          delete finalRatios.CreateCacheRatio[model]
          delete finalRatios.ImageRatio[model]
          delete finalRatios.AudioRatio[model]
          delete finalRatios.AudioCompletionRatio[model]
          delete finalRatios.ModelPrice[model]
        }

        Object.entries(ratios).forEach(([ratioType, value]) => {
          const optionKey = optionKeyBySyncField(ratioType)
          finalRatios[optionKey][model] = NUMERIC_SYNC_FIELDS.has(ratioType)
            ? Number(value)
            : value
        })
      })

      const updates = Object.entries(finalRatios).map(([key, value]) => ({
        key,
        value: JSON.stringify(value, null, 2),
      }))

      return new Promise<boolean>((resolve) => {
        syncMutate(updates, {
          onSuccess: () => resolve(true),
          onError: () => resolve(false),
        })
      })
    },
    [resolutions, syncMutate, queryClient, parseRatios, parsedRatios]
  )

  const findSourceChannel = (
    model: string,
    ratioType: RatioType,
    value: number | string
  ): string => {
    const upMap = differences[model]?.[ratioType]?.upstreams
    if (!upMap) return 'Unknown'
    const entry = Object.entries(upMap).find(([, v]) => v === value)
    return entry ? entry[0] : 'Unknown'
  }

  const handleApplySync = () => {
    const currentRatios = parsedRatios
    const conflicts: ConflictItem[] = []

    const fixedPriceLabel = t('Fixed price')
    const modelRatioLabel = t('Model ratio')
    const completionRatioLabel = t('Completion ratio')

    Object.entries(resolutions).forEach(([model, ratios]) => {
      const localCat = getLocalBillingCategory(model, currentRatios)
      const selectedTypes = Object.keys(ratios)
      let newCat: 'price' | 'ratio' | 'tiered'
      if ('model_price' in ratios) {
        newCat = 'price'
      } else if (
        selectedTypes.includes('billing_mode') ||
        selectedTypes.includes('billing_expr')
      ) {
        // 表达式计费优先判定：model_ratio 作为折扣伴随字段与 billing_expr 同选时
        // 仍属 tiered，避免误报"表达式 → 倍率"的计费模式冲突
        newCat = 'tiered'
      } else if (RATIO_SYNC_FIELDS.some((rt) => selectedTypes.includes(rt))) {
        newCat = 'ratio'
      } else {
        newCat = 'tiered'
      }

      // 本地已是表达式计费、本次仅选择折扣（model_ratio）时，只是设置折扣系数，
      // 不改变计费模式，不属于"固定价格 vs 比例计费"冲突
      const isDiscountOnlyOnTiered =
        localCat === 'tiered' &&
        newCat === 'ratio' &&
        selectedTypes.every((rt) => rt === 'model_ratio')

      if (
        localCat &&
        newCat !== 'tiered' &&
        localCat !== newCat &&
        !isDiscountOnlyOnTiered
      ) {
        let currentDesc: string
        if (localCat === 'price') {
          currentDesc = `${fixedPriceLabel}: ${currentRatios.ModelPrice[model]}`
        } else if (localCat === 'tiered') {
          currentDesc = 'Expression billing'
        } else {
          currentDesc = `${modelRatioLabel}: ${currentRatios.ModelRatio[model] ?? '-'}\n${completionRatioLabel}: ${currentRatios.CompletionRatio[model] ?? '-'}`
        }

        // 此分支内 newCat 已排除 'tiered'（见上方守卫），仅可能是 price/ratio
        const newDesc =
          newCat === 'price'
            ? `${fixedPriceLabel}: ${ratios.model_price}`
            : `${modelRatioLabel}: ${ratios.model_ratio ?? '-'}\n${completionRatioLabel}: ${ratios.completion_ratio ?? '-'}`

        const channelNames = selectedTypes
          .map((rt) => findSourceChannel(model, rt as RatioType, ratios[rt]))
          .filter((v, idx, arr) => arr.indexOf(v) === idx)
          .join(', ')

        conflicts.push({
          channel: channelNames,
          model,
          current: currentDesc,
          newVal: newDesc,
        })
      }
    })

    if (conflicts.length > 0) {
      setConflictItems(conflicts)
      setConflictDialogOpen(true)
      return
    }

    toast.info(t('Syncing prices, please wait...'))
    performSync()
  }

  const handleConfirmConflict = async () => {
    setConfirmLoading(true)
    try {
      const success = await performSync()
      if (success) {
        setConflictDialogOpen(false)
      }
    } finally {
      setConfirmLoading(false)
    }
  }

  const hasSelections = Object.keys(resolutions).length > 0
  const isLoading =
    fetchMutation.isPending ||
    csvUploadMutation.isPending ||
    isSyncPending ||
    confirmLoading

  return (
    <div className='flex h-full min-h-0 flex-col gap-4'>
      <div className='flex shrink-0 flex-col gap-2 sm:flex-row sm:items-center sm:justify-between'>
        <div className='flex flex-col gap-2 sm:flex-row'>
          <Button onClick={handleOpenChannelDialog} disabled={isLoading}>
            <RefreshCcw className='mr-2 h-4 w-4' />
            {t('Select Sync Channels')}
          </Button>
          <Button
            variant='outline'
            onClick={handleCSVUploadClick}
            disabled={isLoading}
          >
            {csvUploadMutation.isPending && (
              <span className='mr-2 h-4 w-4 animate-spin rounded-full border-2 border-current border-t-transparent' />
            )}
            <Upload className='mr-2 h-4 w-4' />
            {t('Upload CSV/Excel')}
          </Button>
          <Button
            variant='secondary'
            onClick={handleApplySync}
            disabled={!hasSelections || isLoading}
          >
            {(isSyncPending || confirmLoading) && (
              <span className='mr-2 h-4 w-4 animate-spin rounded-full border-2 border-current border-t-transparent' />
            )}
            <CheckSquare className='mr-2 h-4 w-4' />
            {t('Apply Sync')}
          </Button>
        </div>
      </div>

      <input
        ref={csvFileInputRef}
        type='file'
        accept='.csv,.xlsx'
        className='hidden'
        onChange={handleCSVFileChange}
      />

      {skippedModels.length > 0 && (
        <Alert className='shrink-0'>
          <AlertTitle>
            {t('Skipped {{count}} models that are not available in any enabled channel', {
              count: skippedModels.length,
            })}
          </AlertTitle>
          <AlertDescription>
            {t(
              'Only models available in enabled channels or already configured locally can be imported'
            )}
          </AlertDescription>
          <div className='col-span-full max-h-32 overflow-y-auto'>
            <div className='flex flex-wrap gap-1 pt-1'>
              {skippedModels.map((model) => (
                <span
                  key={model}
                  className='bg-muted rounded px-1.5 py-0.5 font-mono text-xs'
                >
                  {model}
                </span>
              ))}
            </div>
          </div>
          <AlertAction className='flex gap-1'>
            <Button
              variant='ghost'
              size='icon-xs'
              onClick={handleCopySkippedModels}
              title={t('Copy')}
            >
              <Copy />
            </Button>
            <Button
              variant='ghost'
              size='icon-xs'
              onClick={() => setSkippedModels([])}
              title={t('Close')}
            >
              <X />
            </Button>
          </AlertAction>
        </Alert>
      )}

      {parseIssues.length > 0 && (
        <Alert className='shrink-0'>
          <AlertTitle>
            {t('{{count}} models have price rows that need attention', {
              count: parseIssues.length,
            })}
          </AlertTitle>
          <AlertDescription>
            {t(
              'Price rows with billing conditions that cannot be tiered are charged at the highest tier price; rows with unrecognized descriptions are skipped, so affected models may be priced higher or incomplete'
            )}
          </AlertDescription>
          <div className='col-span-full max-h-40 overflow-y-auto'>
            <div className='flex flex-col gap-1.5 pt-1'>
              {parseIssues.map((issue) => (
                <div key={issue.model} className='flex min-w-0 flex-col gap-0.5'>
                  <span className='bg-muted w-fit rounded px-1.5 py-0.5 font-mono text-xs'>
                    {issue.model}
                  </span>
                  <span className='text-muted-foreground text-xs break-all'>
                    {issue.reasons.map((r) => parseReasonText(r, t)).join('；')}
                  </span>
                </div>
              ))}
            </div>
          </div>
          <AlertAction>
            <Button
              variant='ghost'
              size='icon-xs'
              onClick={() => setParseIssues([])}
              title={t('Close')}
            >
              <X />
            </Button>
          </AlertAction>
        </Alert>
      )}

      <div className='min-h-0 flex-1'>
        <UpstreamRatioSyncTable
          differences={differences}
          resolutions={resolutions}
          displayPrices={displayPrices}
          isDisabled={isLoading}
          isSyncing={fetchMutation.isPending}
          onSelectValue={handleSelectValue}
          onSelectValues={handleSelectValues}
          onUnselectValue={handleUnselectValue}
          onUnselectValues={handleUnselectValues}
        />
      </div>

      <ChannelSelectorDialog
        open={channelDialogOpen}
        onOpenChange={setChannelDialogOpen}
        channels={channels}
        selectedChannelIds={selectedChannelIds}
        onSelectedChannelIdsChange={setSelectedChannelIds}
        channelEndpoints={channelEndpoints}
        onChannelEndpointsChange={setChannelEndpoints}
        onConfirm={handleConfirmChannelSelection}
      />

      <ConflictConfirmDialog
        open={conflictDialogOpen}
        onOpenChange={setConflictDialogOpen}
        conflicts={conflictItems}
        onConfirm={handleConfirmConflict}
        isLoading={confirmLoading}
      />
    </div>
  )
}
