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
import { KeyRound } from 'lucide-react'

import { IconBadge } from '@/components/ui/icon-badge'
import { getTokenModelQuotaData } from '@/features/dashboard/api'
import { ModelBreakdownDialog } from '@/features/dashboard/components/model-breakdown-dialog'

export interface TokenModelDialogToken {
  tokenId: number
  label: string
}

interface TokenModelDialogProps {
  open: boolean
  onOpenChange: (open: boolean) => void
  token: TokenModelDialogToken | null
  timeRange: { start_timestamp: number; end_timestamp: number }
  isAdmin: boolean
}

export function TokenModelDialog(props: TokenModelDialogProps) {
  const tokenId = props.token?.tokenId ?? 0

  return (
    <ModelBreakdownDialog
      open={props.open}
      onOpenChange={props.onOpenChange}
      title={
        <span className='flex items-center gap-2'>
          <IconBadge tone='info' size='sm'>
            <KeyRound />
          </IconBadge>
          {props.token?.label}
        </span>
      }
      timeRange={props.timeRange}
      queryKey={[
        'dashboard',
        'token-model-quota',
        tokenId,
        props.timeRange.start_timestamp,
        props.timeRange.end_timestamp,
        props.isAdmin,
      ]}
      fetchModels={() =>
        getTokenModelQuotaData(
          { token_id: tokenId, ...props.timeRange },
          props.isAdmin
        )
      }
      enabled={tokenId > 0}
    />
  )
}
