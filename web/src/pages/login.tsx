import { useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { motion } from 'framer-motion'
import { KeyRound } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { auth } from '@/lib/auth'

export default function Login() {
  const [token, setToken] = useState('')
  const [err, setErr] = useState('')
  const nav = useNavigate()
  const submit = () => {
    if (!token.trim()) { setErr('请输入 admin token'); return }
    auth.setToken(token.trim())
    nav('/dashboard')
  }
  return (
    <div className="flex min-h-screen items-center justify-center bg-slate-950">
      <motion.div initial={{ opacity: 0, y: 12 }} animate={{ opacity: 1, y: 0 }} transition={{ duration: 0.3 }}>
        <Card className="w-96">
          <CardHeader><CardTitle className="flex items-center gap-2"><KeyRound className="h-5 w-5" /> 网关管理台</CardTitle></CardHeader>
          <CardContent className="space-y-3">
            <p className="text-sm text-muted-foreground">输入 admin token（config.toml 的 admin.token）</p>
            <Input type="password" placeholder="admin token" value={token} onChange={e => { setToken(e.target.value); setErr('') }} />
            {err && <p className="text-sm text-red-500">{err}</p>}
            <Button className="w-full" onClick={submit}>进入</Button>
          </CardContent>
        </Card>
      </motion.div>
    </div>
  )
}
