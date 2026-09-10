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
import type { ColumnDef } from '@tanstack/react-table'
import { AlertTriangle } from 'lucide-react'
import { useMemo } from 'react'
import { useTranslation } from 'react-i18next'

import { DataTableColumnHeader } from '@/components/data-table'
import { StatusBadge } from '@/components/status-badge'
import { Checkbox } from '@/components/ui/checkbox'
import {
  Tooltip,
  TooltipContent,
  TooltipProvider,
  TooltipTrigger,
} from '@/components/ui/tooltip'

import type { DisplayPriceLine, RatioType } from '../types'
import {
  getAlignedRatioTypes,
  getPreferredSyncField,
  getSyncFieldLabel,
  isSelectedResolutionValue,
  type ModelRow,
  type ResolutionsMap,
} from './upstream-ratio-sync-helpers'
import type { UpstreamBulkSelectState } from './upstream-ratio-sync-table'

const syncFieldListClassName = 'flex max-w-full min-w-0 flex-col gap-1.5'
const syncFieldRowClassName =
  'bg-muted/30 flex h-8 w-fit max-w-full min-w-0 items-center gap-2 rounded-md px-2'
// 多行可读价格（如 CSV 阶梯定价逐行条目）使用的行样式：取消固定高度
const syncFieldRowMultilineClassName =
  'bg-muted/30 flex min-h-8 w-fit max-w-full min-w-0 items-center gap-2 rounded-md px-2 py-1'
const syncFieldLabelClassName = 'min-w-[4.5rem] shrink-0'

// CSV 导入时各同步字段的可读价格标签（替换默认的倍率/表达式字段名）
// model_ratio 现为折扣位（CSV 折扣列写入），标签展示为 Discount coefficient；
// 不复用 Discount 键（已被充值优惠编辑器占用）
const CSV_DISPLAY_FIELD_LABELS: Record<string, string> = {
  model_ratio: 'Discount coefficient',
  completion_ratio: 'Output price',
  cache_ratio: 'Cache read price',
  create_cache_ratio: 'Cache write price',
  model_price: 'Fixed price',
  billing_expr: 'Billing Price',
  billing_mode: 'Billing Mode',
}

// 可读价格行标签 → 徽章配色所用的 ratioType（与左侧当前价格列的字段颜色保持一致）
const DISPLAY_LINE_COLORS: Record<string, string> = {
  'Input price': 'model_ratio',
  'Output price': 'completion_ratio',
  'Cache read price': 'cache_ratio',
  'Cache write price': 'create_cache_ratio',
  'Cache Write (1h)': 'create_cache_ratio',
}

export function useUpstreamRatioSyncColumns(
  upstreamNames: string[],
  bulkSelectStateByUpstream: Record<string, UpstreamBulkSelectState>,
  resolutions: ResolutionsMap,
  ratioTypeFilter: string,
  isDisabled: boolean,
  onSelectValue: (
    model: string,
    ratioType: RatioType,
    value: number | string,
    sourceName: string
  ) => void,
  onUnselectValue: (model: string, ratioType: RatioType) => void,
  onBulkSelect: (upstreamName: string) => void,
  onBulkUnselect: (upstreamName: string) => void,
  displayPrices?: Record<string, Record<string, string | DisplayPriceLine[]>>
): ColumnDef<ModelRow>[] {
  const { t } = useTranslation()

  return useMemo<ColumnDef<ModelRow>[]>(() => {
    const baseColumns: ColumnDef<ModelRow>[] = [
      {
        accessorKey: 'model',
        header: ({ column }) => (
          <DataTableColumnHeader column={column} title={t('Model')} />
        ),
        size: 220,
        minSize: 180,
        cell: ({ row }) => {
          const model = row.original.model
          return (
            <div className='flex max-w-full min-w-0 items-center gap-2'>
              <span className='truncate font-medium'>{model}</span>
              {row.original.billingConflict && (
                <TooltipProvider>
                  <Tooltip>
                    <TooltipTrigger>
                      <AlertTriangle className='h-3.5 w-3.5 shrink-0 text-amber-500' />
                    </TooltipTrigger>
                    <TooltipContent>
                      <p>
                        {t(
                          'This model has both fixed price and ratio billing conflicts'
                        )}
                      </p>
                    </TooltipContent>
                  </Tooltip>
                </TooltipProvider>
              )}
            </div>
          )
        },
      },
      {
        id: 'current',
        header: ({ column }) => (
          <DataTableColumnHeader column={column} title={t('Current Price')} />
        ),
        size: 260,
        minSize: 220,
        cell: ({ row }) => {
          const fields = getAlignedRatioTypes(
            row.original.ratioTypes,
            upstreamNames,
            ratioTypeFilter
          )
          return (
            <div className={syncFieldListClassName}>
              {fields.map((ratioType) => {
                const current = row.original.ratioTypes[ratioType]?.current
                return (
                  <div key={ratioType} className={syncFieldRowClassName}>
                    <StatusBadge
                      label={getSyncFieldLabel(ratioType, t)}
                      autoColor={ratioType}
                      size='sm'
                      copyable={false}
                      className={syncFieldLabelClassName}
                    />
                    {current === null || current === undefined ? (
                      <StatusBadge
                        label={t('Not Set')}
                        variant='neutral'
                        size='sm'
                        copyable={false}
                      />
                    ) : (
                      <TooltipProvider>
                        <Tooltip>
                          <TooltipTrigger
                            render={
                              <StatusBadge
                                label={String(current)}
                                variant='info'
                                size='sm'
                                className='max-w-[160px] truncate font-mono'
                              />
                            }
                          />
                          <TooltipContent>
                            <p className='max-w-xs text-xs break-all'>
                              {String(current)}
                            </p>
                          </TooltipContent>
                        </Tooltip>
                      </TooltipProvider>
                    )}
                  </div>
                )
              })}
            </div>
          )
        },
      },
    ]

    const upstreamColumns: ColumnDef<ModelRow>[] = upstreamNames.map(
      (upstreamName) => ({
        id: `upstream_${upstreamName}`,
        size: 280,
        minSize: 240,
        header: () => {
          const bulkSelectState = bulkSelectStateByUpstream[upstreamName]
          const displayName = bulkSelectState?.displayName ?? upstreamName
          const selectableCount = bulkSelectState?.selectableCount ?? 0
          const selectedCount = bulkSelectState?.selectedCount ?? 0
          const allSelected =
            selectableCount > 0 && selectedCount === selectableCount
          const someSelected =
            selectedCount > 0 && selectedCount < selectableCount
          return (
            <div className='flex h-9 min-w-0 items-center gap-1.5'>
              {selectableCount > 0 && (
                <Checkbox
                  checked={allSelected}
                  indeterminate={someSelected}
                  disabled={isDisabled}
                  onCheckedChange={(checked) => {
                    if (checked) {
                      onBulkSelect(upstreamName)
                    } else {
                      onBulkUnselect(upstreamName)
                    }
                  }}
                  aria-label={t('Select all (filtered)')}
                  className='shrink-0'
                />
              )}
              <div className='flex min-w-0 flex-1 items-center gap-1.5'>
                <span className='min-w-0 truncate font-medium'>
                  {displayName}
                </span>
                {selectableCount > 0 && (
                  <span className='bg-muted text-muted-foreground shrink-0 rounded px-1.5 py-0.5 text-[11px] leading-none font-normal tabular-nums'>
                    {selectedCount}/{selectableCount}
                  </span>
                )}
              </div>
            </div>
          )
        },
        cell: ({ row }) => {
          const fields = getAlignedRatioTypes(
            row.original.ratioTypes,
            upstreamNames,
            ratioTypeFilter
          )

          return (
            <div className={syncFieldListClassName}>
              {fields.map((ratioType) => {
                const diff = row.original.ratioTypes[ratioType]
                const upstreamVal = diff?.upstreams?.[upstreamName]
                const isConfident = diff?.confidence?.[upstreamName] !== false
                const isVisibleForSource =
                  getPreferredSyncField(
                    row.original.ratioTypes,
                    ratioType,
                    upstreamName
                  ) === ratioType
                const displayContent =
                  displayPrices?.[row.original.model]?.[ratioType]
                const displayLines = Array.isArray(displayContent)
                  ? displayContent
                  : undefined
                const displayText =
                  typeof displayContent === 'string' ? displayContent : undefined
                const isSelected = isSelectedResolutionValue(
                  resolutions,
                  row.original.model,
                  ratioType,
                  upstreamVal
                )
                const handleSelect = () =>
                  onSelectValue(
                    row.original.model,
                    ratioType,
                    upstreamVal as number | string,
                    upstreamName
                  )
                const handleUnselect = () =>
                  onUnselectValue(row.original.model, ratioType)

                // 表达式模型的可读价格是逐变量价格行数组：每行一个彩色徽章标签 + 价格，
                // 与简单模型的字段行样式保持一致；选择仍按 billing_expr 字段整体进行
                if (
                  displayLines &&
                  isVisibleForSource &&
                  upstreamVal !== undefined &&
                  upstreamVal !== null &&
                  upstreamVal !== 'same'
                ) {
                  return (
                    <div
                      key={ratioType}
                      className={syncFieldRowMultilineClassName}
                    >
                      <div className='flex min-w-0 flex-col gap-1.5'>
                        {displayLines.map((line) => (
                          <div
                            key={line.label}
                            className='flex h-8 items-center gap-2'
                          >
                            <StatusBadge
                              label={t(line.label)}
                              autoColor={
                                DISPLAY_LINE_COLORS[line.label] ?? ratioType
                              }
                              size='sm'
                              copyable={false}
                              className={syncFieldLabelClassName}
                            />
                            {/* 每行都有选中框，状态联动：表达式字段整体选择 */}
                            <Checkbox
                              checked={isSelected}
                              disabled={isDisabled}
                              onCheckedChange={(checked) => {
                                if (checked) {
                                  handleSelect()
                                } else {
                                  handleUnselect()
                                }
                              }}
                              className='size-4 shrink-0'
                            />
                            <span className='font-mono text-sm whitespace-nowrap'>
                              {line.value}
                            </span>
                          </div>
                        ))}
                      </div>
                    </div>
                  )
                }

                return (
                  <div key={ratioType} className={syncFieldRowClassName}>
                    <StatusBadge
                      label={
                        displayText
                          ? t(CSV_DISPLAY_FIELD_LABELS[ratioType] ?? ratioType)
                          : getSyncFieldLabel(ratioType, t)
                      }
                      autoColor={ratioType}
                      size='sm'
                      copyable={false}
                      className={syncFieldLabelClassName}
                    />
                    <div className='min-w-0 flex-1'>
                      {renderUpstreamValue({
                        upstreamVal,
                        displayText,
                        isAvailable: isVisibleForSource,
                        isConfident,
                        isSelected,
                        isDisabled,
                        t,
                        onSelect: handleSelect,
                        onUnselect: handleUnselect,
                      })}
                    </div>
                  </div>
                )
              })}
            </div>
          )
        },
      })
    )

    return [...baseColumns, ...upstreamColumns]
  }, [
    upstreamNames,
    bulkSelectStateByUpstream,
    resolutions,
    ratioTypeFilter,
    isDisabled,
    onSelectValue,
    onUnselectValue,
    onBulkSelect,
    onBulkUnselect,
    displayPrices,
    t,
  ])
}

type RenderUpstreamValueArgs = {
  upstreamVal: number | string | 'same' | null | undefined
  displayText?: string
  isAvailable: boolean
  isConfident: boolean
  isSelected: boolean
  isDisabled: boolean
  t: (key: string) => string
  onSelect: () => void
  onUnselect: () => void
}

function renderUpstreamValue(args: RenderUpstreamValueArgs) {
  const {
    upstreamVal,
    displayText,
    isAvailable,
    isConfident,
    isSelected,
    isDisabled,
    t,
  } = args

  if (!isAvailable) {
    return (
      <StatusBadge label='—' variant='neutral' size='sm' copyable={false} />
    )
  }

  if (upstreamVal === null || upstreamVal === undefined) {
    return (
      <StatusBadge
        label={t('Not Set')}
        variant='neutral'
        size='sm'
        copyable={false}
      />
    )
  }

  if (upstreamVal === 'same') {
    return (
      <StatusBadge
        label={t('Same as Local')}
        variant='info'
        size='sm'
        copyable={false}
      />
    )
  }

  // CSV 导入时优先展示可读价格文本（如 "4.2 元/M"），选择逻辑仍基于原始值；
  // 经 t() 处理使 "Expression billing" 等 i18n key 可翻译，价格文本不在词表中则原样返回
  const text = displayText ? t(displayText) : String(upstreamVal)

  return (
    <div className='flex h-full min-w-0 items-center gap-2'>
      <Checkbox
        checked={isSelected}
        disabled={isDisabled}
        onCheckedChange={(checked) => {
          if (checked) {
            args.onSelect()
          } else {
            args.onUnselect()
          }
        }}
        className='size-4'
      />
      <TooltipProvider>
        <Tooltip>
          <TooltipTrigger
            render={
              <span className='inline-block max-w-[240px] cursor-default truncate font-mono text-sm' />
            }
          >
            {text}
          </TooltipTrigger>
          <TooltipContent>
            <p className='max-w-xs text-xs break-all'>{text}</p>
          </TooltipContent>
        </Tooltip>
      </TooltipProvider>
      {!isConfident && (
        <TooltipProvider>
          <Tooltip>
            <TooltipTrigger>
              <AlertTriangle className='h-3.5 w-3.5 shrink-0 text-amber-500' />
            </TooltipTrigger>
            <TooltipContent>
              <p>{t('This data may be unreliable, use with caution')}</p>
            </TooltipContent>
          </Tooltip>
        </TooltipProvider>
      )}
    </div>
  )
}
