import { useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { motion } from 'framer-motion'
import { KeyRound } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { auth } from '@/lib/auth'

export default function Login() {
  const { t } = useTranslation()
  const [token, setToken] = useState('')
  const [err, setErr] = useState('')
  const nav = useNavigate()
  const submit = () => {
    if (!token.trim()) { setErr(t('login.emptyToken')); return }
    auth.setToken(token.trim())
    nav('/dashboard')
  }
  return (
    <div className="relative flex min-h-screen items-center justify-center overflow-hidden">
      <motion.div initial={{ opacity: 0, y: 12 }} animate={{ opacity: 1, y: 0 }} transition={{ duration: 0.3 }}>
        <Card className="w-96 p-2">
          <CardHeader><CardTitle className="flex items-center gap-2"><KeyRound className="h-5 w-5" /> {t('common.appTitle')}</CardTitle></CardHeader>
          <CardContent className="space-y-3">
            <p className="text-sm text-muted-foreground">{t('login.hint')}</p>
            <Input type="password" placeholder="admin token" value={token} onChange={e => { setToken(e.target.value); setErr('') }} />
            {err && <p className="text-sm text-red-500">{err}</p>}
            <Button className="w-full" onClick={submit}>{t('login.enter')}</Button>
          </CardContent>
        </Card>
      </motion.div>
    </div>
  )
}
