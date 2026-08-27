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
import { useQuery } from '@tanstack/react-query'
import { VChart } from '@visactor/react-vchart'
import { PieChart } from 'lucide-react'
import { useEffect, useMemo, useState, type ReactNode } from 'react'
import { useTranslation } from 'react-i18next'

import { Dialog } from '@/components/dialog'
import {
  Empty,
  EmptyDescription,
  EmptyHeader,
  EmptyMedia,
  EmptyTitle,
} from '@/components/ui/empty'
import { Skeleton } from '@/components/ui/skeleton'
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table'
import { useTheme } from '@/context/theme-provider'
import type { ModelQuotaItem } from '@/features/dashboard/types'
import { TOKEN_COLORS } from '@/features/dashboard/lib/charts'
import { formatNumber, formatPercent, formatQuota } from '@/lib/format'
import { VCHART_OPTION } from '@/lib/vchart'

let themeManagerPromise: Promise<
  (typeof import('@visactor/vchart'))['ThemeManager']
> | null = null

interface ModelBreakdownDialogProps {
  open: boolean
  onOpenChange: (open: boolean) => void
  // Dialog title, typically an IconBadge plus the drilled entity name.
  title: ReactNode
  timeRange: { start_timestamp: number; end_timestamp: number }
  queryKey: readonly unknown[]
  fetchModels: () => Promise<{
    success: boolean
    data?: ModelQuotaItem[]
    message?: string
  }>
  // Extra gate for the query beyond `open` (e.g. a selected entity exists).
  enabled?: boolean
  // Overrides the empty-state description (defaults to the key wording).
  emptyDescription?: string
}

function formatRangeDate(timestamp: number): string {
  return new Date(timestamp * 1000).toLocaleDateString()
}

export function ModelBreakdownDialog(props: ModelBreakdownDialogProps) {
  const { t } = useTranslation()
  const { resolvedTheme } = useTheme()
  const [themeReady, setThemeReady] = useState(false)

  useEffect(() => {
    const updateTheme = async () => {
      setThemeReady(false)
      if (!themeManagerPromise) {
        themeManagerPromise = import('@visactor/vchart').then(
          (m) => m.ThemeManager
        )
      }
      const ThemeManager = await themeManagerPromise
      ThemeManager.setCurrentTheme(resolvedTheme === 'dark' ? 'dark' : 'light')
      setThemeReady(true)
    }
    updateTheme()
  }, [resolvedTheme])

  const { data: items, isLoading } = useQuery({
    queryKey: props.queryKey,
    queryFn: props.fetchModels,
    select: (res) => (res.success ? (res.data ?? []) : []),
    enabled: props.open && (props.enabled ?? true),
    staleTime: 60_000,
  })

  const rows = useMemo(() => {
    const list = items ?? []
    const total = list.reduce((sum, item) => sum + (Number(item.quota) || 0), 0)
    return {
      total,
      models: list.map((item) => {
        const quota = Number(item.quota) || 0
        return {
          model: item.model_name || t('Unknown model'),
          quota,
          share: total > 0 ? quota / total : 0,
          count: Number(item.count) || 0,
          tokenUsed: Number(item.token_used) || 0,
        }
      }),
    }
  }, [items, t])

  const pieSpec = useMemo(
    () => ({
      type: 'pie' as const,
      data: [{ id: 'modelBreakdownData', values: rows.models }],
      categoryField: 'model',
      valueField: 'quota',
      outerRadius: 0.8,
      innerRadius: 0.5,
      pie: {
        state: {
          hover: { outerRadius: 0.85, stroke: '#000', lineWidth: 1 },
        },
      },
      label: {
        visible: true,
        position: 'outside' as const,
        formatMethod: (_text: string, datum: Record<string, unknown>) =>
          formatPercent((Number(datum?.share) || 0) * 100),
        style: { fontSize: 11 },
        line: { visible: true },
      },
      legends: { visible: true, orient: 'left' as const },
      tooltip: {
        mark: {
          content: [
            {
              key: (datum: Record<string, unknown>) => datum?.model,
              value: (datum: Record<string, unknown>) =>
                `${formatQuota(Number(datum?.quota) || 0)} (${formatPercent((Number(datum?.share) || 0) * 100)})`,
            },
          ],
        },
      },
      color: { type: 'ordinal', range: TOKEN_COLORS },
      background: { fill: 'transparent' },
      animation: true,
    }),
    [rows.models]
  )

  return (
    <Dialog
      open={props.open}
      onOpenChange={props.onOpenChange}
      title={props.title}
      description={`${formatRangeDate(props.timeRange.start_timestamp)} ~ ${formatRangeDate(props.timeRange.end_timestamp)} · ${t('Total:')} ${formatQuota(rows.total)}`}
      contentClassName='max-sm:h-dvh max-sm:w-screen max-sm:max-w-none max-sm:rounded-none sm:max-w-xl'
      showCloseButton
    >
      {isLoading ? (
        <div className='grid gap-3'>
          <Skeleton className='h-[260px] w-full' />
          <Skeleton className='h-40 w-full' />
        </div>
      ) : rows.models.length === 0 ? (
        <Empty className='h-64 border'>
          <EmptyHeader>
            <EmptyMedia variant='icon'>
              <PieChart />
            </EmptyMedia>
            <EmptyTitle>{t('No data available')}</EmptyTitle>
            <EmptyDescription>
              {props.emptyDescription ??
                t('This key has no consumption records in the selected range.')}
            </EmptyDescription>
          </EmptyHeader>
        </Empty>
      ) : (
        <div className='grid gap-3'>
          <div className='h-[260px]'>
            {themeReady && (
              <VChart
                key={`model-breakdown-${resolvedTheme}`}
                spec={{
                  ...pieSpec,
                  theme: resolvedTheme === 'dark' ? 'dark' : 'light',
                }}
                option={VCHART_OPTION}
              />
            )}
          </div>

          <div className='max-h-64 overflow-y-auto rounded-md border'>
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>{t('Model')}</TableHead>
                  <TableHead className='text-end'>{t('Consumption')}</TableHead>
                  <TableHead className='w-32'>{t('Share')}</TableHead>
                  <TableHead className='text-end'>{t('Requests')}</TableHead>
                  <TableHead className='text-end'>{t('Tokens')}</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {rows.models.map((row, index) => (
                  <TableRow
                    key={row.model}
                    className={index % 2 === 1 ? 'bg-muted/40' : undefined}
                  >
                    <TableCell className='max-w-40 truncate font-medium'>
                      {row.model}
                    </TableCell>
                    <TableCell className='text-end tabular-nums'>
                      {formatQuota(row.quota)}
                    </TableCell>
                    <TableCell>
                      <div className='flex items-center gap-2'>
                        <div className='bg-muted h-1.5 min-w-10 flex-1 rounded-full'>
                          <div
                            className='bg-primary h-full rounded-full'
                            style={{
                              width: `${Math.min(row.share * 100, 100)}%`,
                            }}
                          />
                        </div>
                        <span className='text-muted-foreground text-xs tabular-nums'>
                          {formatPercent(row.share * 100)}
                        </span>
                      </div>
                    </TableCell>
                    <TableCell className='text-end tabular-nums'>
                      {formatNumber(row.count)}
                    </TableCell>
                    <TableCell className='text-end tabular-nums'>
                      {formatNumber(row.tokenUsed)}
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </div>
        </div>
      )}
    </Dialog>
  )
}
