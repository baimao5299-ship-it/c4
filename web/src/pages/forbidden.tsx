// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

import { Link } from 'react-router-dom'
import { motion } from 'framer-motion'
import { ShieldX } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import { buttonVariants } from '@/components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { cn } from '@/lib/utils'

// 403 无权限界面：已登录但角色不足访问管理端时由 RequireAdmin 守卫渲染。
export default function Forbidden() {
  const { t } = useTranslation()
  return (
    <div className="flex min-h-screen items-center justify-center bg-gradient-to-br from-muted via-background to-background">
      <motion.div initial={{ opacity: 0, y: 12 }} animate={{ opacity: 1, y: 0 }} transition={{ duration: 0.3 }}>
        <Card className="w-96">
          <CardHeader><CardTitle className="flex items-center gap-2"><ShieldX className="h-5 w-5" /> {t('error403.title')}</CardTitle></CardHeader>
          <CardContent className="space-y-4">
            <p className="text-sm text-muted-foreground">{t('error403.desc')}</p>
            <Link to="/user" className={cn(buttonVariants({ variant: 'outline' }), 'w-full')}>
              {t('error403.backUser')}
            </Link>
          </CardContent>
        </Card>
      </motion.div>
    </div>
  )
}
