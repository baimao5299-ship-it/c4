import { useState } from 'react'
import { Link, useNavigate } from 'react-router-dom'
import { motion } from 'framer-motion'
import { ShieldCheck } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { ModeToggle } from '@/components/mode-toggle'
import { ApiError, userApi } from '@/lib/api/client'
import { auth } from '@/lib/auth'
import { setLang, type AppLang } from '@/lib/i18n'
import { cn } from '@/lib/utils'

const LANGS: { code: AppLang; label: string }[] = [
  { code: 'zh-CN', label: '中' },
  { code: 'en', label: 'EN' },
]

// 管理端登录：与用户端共用 POST /user/auth/login；
// 仅 platform_admin 角色允许进入管理台，其余账号不存 token、不跳转。
export default function Login() {
  const { t, i18n } = useTranslation()
  const lang: AppLang = i18n.resolvedLanguage?.startsWith('zh') ? 'zh-CN' : 'en'
  const [email, setEmail] = useState('')
  const [password, setPassword] = useState('')
  const [err, setErr] = useState('')
  const [loading, setLoading] = useState(false)
  const nav = useNavigate()

  const submit = async () => {
    if (!email.trim() || !password) { setErr(t('login.errorGeneric')); return }
    setErr('')
    setLoading(true)
    try {
      const res = await userApi.login({ email: email.trim(), password })
      if (res.user.Role !== 'platform_admin') {
        setErr(t('login.adminOnly'))
        return
      }
      auth.setToken(res.token)
      nav('/app/dashboard')
    } catch (e) {
      // 服务端 error 字段直接展示；网络异常等统一兜底文案
      setErr(e instanceof ApiError ? e.message : t('login.errorGeneric'))
    } finally {
      setLoading(false)
    }
  }

  return (
    <div className="flex min-h-screen items-center justify-center bg-gradient-to-br from-muted via-background to-background">
      <div className="absolute right-4 top-4 flex items-center gap-2">
        <ModeToggle />
        <div className="inline-flex items-center gap-1 rounded-md border bg-background p-0.5">
          {LANGS.map(({ code, label }) => (
            <Button
              key={code}
              size="sm"
              variant="ghost"
              className={cn('h-7 min-w-9 px-2', lang === code && 'bg-secondary text-secondary-foreground')}
              onClick={() => setLang(code)}
            >
              {label}
            </Button>
          ))}
        </div>
      </div>
      <motion.div initial={{ opacity: 0, y: 12 }} animate={{ opacity: 1, y: 0 }} transition={{ duration: 0.3 }}>
        <Card className="w-96">
          <CardHeader><CardTitle className="flex items-center gap-2"><ShieldCheck className="h-5 w-5" /> {t('common.appTitle')}</CardTitle></CardHeader>
          <CardContent className="space-y-3">
            <p className="text-sm text-muted-foreground">{t('login.subtitle')}</p>
            <Input type="email" placeholder={t('login.email')} value={email} onChange={e => { setEmail(e.target.value); setErr('') }} />
            <Input type="password" placeholder={t('login.password')} value={password} onChange={e => { setPassword(e.target.value); setErr('') }} onKeyDown={e => { if (e.key === 'Enter') submit() }} />
            {err && <p className="text-sm text-destructive">{err}</p>}
            <Button className="w-full" disabled={loading} onClick={submit}>{t('login.enter')}</Button>
            <Link to="/user/login" className="block text-center text-sm text-muted-foreground transition-colors hover:text-foreground">
              {t('login.userCenterLink')}
            </Link>
          </CardContent>
        </Card>
      </motion.div>
    </div>
  )
}
