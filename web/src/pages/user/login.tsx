// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

import { useState } from 'react'
import { Link, useLocation, useNavigate } from 'react-router-dom'
import { motion } from 'framer-motion'
import { KeyRound, LogIn } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { ModeToggle } from '@/components/mode-toggle'
import { adminApi, ApiError, ApiUnauthorized, userApi } from '@/lib/api/client'
import { userAuth } from '@/lib/auth'
import { setLang, type AppLang } from '@/lib/i18n'
import { cn } from '@/lib/utils'

const LANGS: { code: AppLang; label: string }[] = [
  { code: 'zh-CN', label: '中' },
  { code: 'en', label: 'EN' },
]

// 唯一登录页（原管理端 /login 已并入）：POST /user/auth/login 成功后统一进 /user；
// 角色随 token 存入同一 userAuth 槽——platform_admin 凭侧边栏管理菜单进入 /app。
export default function UserLogin() {
  const { t, i18n } = useTranslation()
  const lang: AppLang = i18n.resolvedLanguage?.startsWith('zh') ? 'zh-CN' : 'en'
  const [email, setEmail] = useState('')
  const [password, setPassword] = useState('')
  const [adminToken, setAdminToken] = useState('')
  const [authMode, setAuthMode] = useState<'user' | 'admin'>('user')
  const location = useLocation()
  const [err, setErr] = useState(() => (location.state as { authMessage?: string } | null)?.authMessage ?? '')
  const [loading, setLoading] = useState(false)
  const nav = useNavigate()

  const submit = async () => {
    if (!email.trim() || !password) { setErr(t('user.auth.errorGeneric')); return }
    setErr('')
    setLoading(true)
    try {
      const res = await userApi.login({ email: email.trim(), password })
      userAuth.setToken(res.token)
      userAuth.setRole(res.user.Role)
      userAuth.setMode('user')
      nav('/user')
    } catch (e) {
      // 服务端 error 字段直接展示；网络异常等统一兜底文案
      setErr(e instanceof ApiUnauthorized || e instanceof ApiError ? e.message : t('user.auth.errorGeneric'))
    } finally {
      setLoading(false)
    }
  }

  const submitAdminToken = async () => {
    const token = adminToken.trim().replace(/^Bearer\s+/i, '')
    if (!token) { setErr(t('user.auth.adminTokenRequired')); return }
    setErr('')
    setLoading(true)
    // Validate before leaving the login page. /api/admin/settings is a cheap,
    // read-only authenticated request and works with both static tokens and
    // platform_admin JWTs; a failed check never leaves a stale token behind.
    userAuth.setToken(token)
    userAuth.setRole('platform_admin')
    userAuth.setMode('admin_token')
    try {
      await adminApi.getSettings()
      nav('/app/dashboard')
    } catch (e) {
      userAuth.clear()
      setErr(e instanceof ApiError ? e.message : t('user.auth.adminTokenInvalid'))
    } finally {
      setLoading(false)
    }
  }

  const switchMode = (mode: 'user' | 'admin') => {
    setAuthMode(mode)
    setErr('')
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
        <Card className="w-[min(calc(100vw-2rem),24rem)]">
          <CardHeader>
            <CardTitle className="flex items-center gap-2"><LogIn className="h-5 w-5" /> {t('user.auth.title')}</CardTitle>
            <div role="tablist" aria-label={t('user.auth.loginMode')} className="grid grid-cols-2 gap-1 rounded-md bg-muted p-1">
              <Button type="button" role="tab" aria-selected={authMode === 'user'} variant={authMode === 'user' ? 'secondary' : 'ghost'} size="sm" onClick={() => switchMode('user')}>
                <LogIn className="mr-1.5 h-4 w-4" />{t('user.auth.userTab')}
              </Button>
              <Button type="button" role="tab" aria-selected={authMode === 'admin'} variant={authMode === 'admin' ? 'secondary' : 'ghost'} size="sm" onClick={() => switchMode('admin')}>
                <KeyRound className="mr-1.5 h-4 w-4" />{t('user.auth.adminTab')}
              </Button>
            </div>
          </CardHeader>
          <CardContent>
            {authMode === 'admin' ? (
              <form className="space-y-3" onSubmit={event => { event.preventDefault(); void submitAdminToken() }}>
                <p className="text-sm text-muted-foreground">{t('user.auth.adminSubtitle')}</p>
                <div className="space-y-1.5"><Label htmlFor="admin-token">{t('user.auth.adminToken')}</Label><Input id="admin-token" type="password" autoComplete="off" autoCapitalize="none" autoCorrect="off" placeholder={t('user.auth.adminTokenPlaceholder')} value={adminToken} onChange={e => { setAdminToken(e.target.value); setErr('') }} aria-invalid={Boolean(err)} aria-describedby={err ? 'login-error' : undefined} /></div>
                {err && <p id="login-error" role="alert" className="text-sm text-destructive">{err}</p>}
                <Button type="submit" className="w-full" disabled={loading}>{t('user.auth.adminLoginButton')}</Button>
              </form>
            ) : (
              <>
                <form className="space-y-3" onSubmit={event => { event.preventDefault(); void submit() }}>
                  <p className="text-sm text-muted-foreground">{t('user.auth.subtitle')}</p>
                  <div className="space-y-1.5"><Label htmlFor="login-email">{t('user.auth.email')}</Label><Input id="login-email" type="email" autoComplete="email" autoCapitalize="none" autoCorrect="off" placeholder={t('user.auth.email')} value={email} onChange={e => { setEmail(e.target.value); setErr('') }} aria-invalid={Boolean(err)} aria-describedby={err ? 'login-error' : undefined} /></div>
                  <div className="space-y-1.5"><Label htmlFor="login-password">{t('user.auth.password')}</Label><Input id="login-password" type="password" autoComplete="current-password" placeholder={t('user.auth.password')} value={password} onChange={e => { setPassword(e.target.value); setErr('') }} aria-invalid={Boolean(err)} aria-describedby={err ? 'login-error' : undefined} /></div>
                  {err && <p id="login-error" role="alert" className="text-sm text-destructive">{err}</p>}
                  <Button type="submit" className="w-full" disabled={loading}>{t('user.auth.loginButton')}</Button>
                </form>
                <Link to="/user/forgot-password" className="block text-center text-sm text-muted-foreground transition-colors hover:text-foreground">{t('user.auth.forgotPasswordLink')}</Link>
                <Link to="/user/register" className="block text-center text-sm text-muted-foreground transition-colors hover:text-foreground">
                  {t('user.auth.registerLink')}
                </Link>
              </>
            )}
          </CardContent>
        </Card>
      </motion.div>
    </div>
  )
}
