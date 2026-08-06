import { useTranslation } from 'react-i18next'
import { Placeholder } from '@/components/placeholder'

export default function Logs() {
  const { t } = useTranslation()
  return <Placeholder title={t('nav.logs')} />
}
