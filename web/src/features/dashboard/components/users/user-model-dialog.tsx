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
import { Users } from 'lucide-react'
import { useTranslation } from 'react-i18next'

import { IconBadge } from '@/components/ui/icon-badge'
import { getUserModelQuotaData } from '@/features/dashboard/api'
import { ModelBreakdownDialog } from '@/features/dashboard/components/model-breakdown-dialog'

interface UserModelDialogProps {
  open: boolean
  onOpenChange: (open: boolean) => void
  username: string | null
  timeRange: { start_timestamp: number; end_timestamp: number }
}

export function UserModelDialog(props: UserModelDialogProps) {
  const { t } = useTranslation()
  const username = props.username ?? ''

  return (
    <ModelBreakdownDialog
      open={props.open}
      onOpenChange={props.onOpenChange}
      title={
        <span className='flex items-center gap-2'>
          <IconBadge tone='info' size='sm'>
            <Users />
          </IconBadge>
          {username}
        </span>
      }
      timeRange={props.timeRange}
      queryKey={[
        'dashboard',
        'user-model-quota',
        username,
        props.timeRange.start_timestamp,
        props.timeRange.end_timestamp,
      ]}
      fetchModels={() =>
        getUserModelQuotaData({ username, ...props.timeRange })
      }
      enabled={username !== ''}
      emptyDescription={t(
        'This user has no consumption records in the selected range.'
      )}
    />
  )
}
