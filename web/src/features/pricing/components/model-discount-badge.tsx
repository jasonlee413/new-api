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
import { useTranslation } from 'react-i18next'

import { cn } from '@/lib/utils'

export interface ModelDiscountBadgeProps {
  /** Discount coefficient (e.g. 0.6 = 6折 / 40% off); hidden when null or 1 */
  discount?: number | null
  className?: string
}

/**
 * Small badge rendered after a model name when the model has a
 * user-configured discount (≠ 1). zh shows "6折", en shows "40% off".
 */
export function ModelDiscountBadge({
  discount,
  className,
}: ModelDiscountBadgeProps) {
  const { t } = useTranslation()

  if (discount == null || discount === 1 || discount <= 0) {
    return null
  }

  // 0.6 -> "6", 0.65 -> "6.5"
  const zhe = (Math.round(discount * 100) / 10).toString()
  const percent = Math.round((1 - discount) * 100)

  return (
    <span
      className={cn(
        'shrink-0 rounded bg-emerald-100 px-1.5 py-0.5 text-[11px] font-medium text-emerald-700 dark:bg-emerald-500/20 dark:text-emerald-300',
        className
      )}
    >
      {t('({{percent}}% off)', { percent, value: zhe })}
    </span>
  )
}
