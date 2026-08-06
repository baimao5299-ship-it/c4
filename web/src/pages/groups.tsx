import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { motion } from 'framer-motion'
import { Plus, Pencil, Trash2, FolderOpen, Link2, RefreshCw } from 'lucide-react'
import { api } from '@/App'
import { ApiUnauthorized } from '@/lib/api/client'
import { Button } from '@/components/ui/button'
import { Card } from '@/components/ui/card'
import { Checkbox } from '@/components/ui/checkbox'
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Skeleton } from '@/components/ui/skeleton'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { StatusBadge } from '@/components/status-badge'
import { KeyBox } from '@/components/key-box'
import { formatDateTime, truncate } from '@/components/fmt'
import type { components } from '@/lib/api/schema'

type Group = components['schemas']['Group']

export default function Groups() {
  const qc = useQueryClient()
  const { data, isLoading, isError, error } = useQuery({ queryKey: ['groups'], queryFn: api.listGroups })
  const accountsQ = useQuery({ queryKey: ['accounts'], queryFn: api.listAccounts })
  const accounts = accountsQ.data ?? []

  // —— 创建（form → 明文 key 展示）——
  const [createOpen, setCreateOpen] = useState(false)
  const [createName, setCreateName] = useState('')
  const [createdKey, setCreatedKey] = useState<{ name: string; key: string } | null>(null)
  // —— 编辑（重命名）——
  const [editTarget, setEditTarget] = useState<Group | null>(null)
  const [editName, setEditName] = useState('')
  // —— 绑定账号 ——
  const [bindTarget, setBindTarget] = useState<Group | null>(null)
  const [bindChecked, setBindChecked] = useState<number[]>([])
  // —— 轮换 key（确认 → 明文 key 展示）——
  const [rotateTarget, setRotateTarget] = useState<Group | null>(null)
  const [rotateResult, setRotateResult] = useState<{ name: string; key: string } | null>(null)
  // —— 删除 ——
  const [deleting, setDeleting] = useState<Group | null>(null)

  const create = useMutation({
    mutationFn: (name: string) => api.createGroup({ name }),
    onSuccess: (res) => {
      qc.invalidateQueries({ queryKey: ['groups'] })
      setCreatedKey({ name: res.group.Name ?? '', key: res.key })
    },
  })
  const rename = useMutation({
    mutationFn: () => api.updateGroup(editTarget!.ID!, { name: editName.trim() }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['groups'] })
      setEditTarget(null)
    },
  })
  const bind = useMutation({
    mutationFn: () => api.setGroupAccounts(bindTarget!.ID!, bindChecked),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['groups'] })
      setBindTarget(null)
    },
  })
  const rotate = useMutation({
    mutationFn: (id: number) => api.rotateGroupKey(id),
    onSuccess: (res) => {
      qc.invalidateQueries({ queryKey: ['groups'] })
      setRotateResult({ name: rotateTarget?.Name ?? '', key: res.key })
      setRotateTarget(null)
    },
  })
  const remove = useMutation({
    mutationFn: (id: number) => api.deleteGroup(id),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['groups'] })
      setDeleting(null)
    },
  })

  const errMsg = (e: unknown) => (e instanceof ApiUnauthorized ? null : (e as Error)?.message)

  const openBind = (g: Group) => {
    // API 无读取当前绑定的端点，绑定对话框为全量重选（不勾选 = 全解绑）。
    setBindTarget(g)
    setBindChecked([])
  }
  const toggleChecked = (id: number) =>
    setBindChecked(cs => (cs.includes(id) ? cs.filter(c => c !== id) : [...cs, id]))

  return (
    <div className="space-y-4">
      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-lg font-semibold">分组</h1>
          <p className="text-sm text-muted-foreground">客户端 key 的归属单位，绑定一组账号</p>
        </div>
        <Button onClick={() => { setCreateName(''); setCreatedKey(null); setCreateOpen(true) }}><Plus /> 新建分组</Button>
      </div>

      {isError ? (
        <p className="text-sm text-destructive">加载失败：{(error as Error).message}</p>
      ) : isLoading ? (
        <div className="space-y-2">
          {Array.from({ length: 3 }).map((_, i) => <Skeleton key={i} className="h-12" />)}
        </div>
      ) : (data ?? []).length === 0 ? (
        <motion.div initial={{ opacity: 0, y: 12 }} animate={{ opacity: 1, y: 0 }} transition={{ duration: 0.25 }}>
          <Card className="flex flex-col items-center gap-2 py-12 text-muted-foreground">
            <FolderOpen className="size-10" />
            <p className="font-medium">暂无分组</p>
            <p className="text-sm">创建分组以生成客户端访问 key</p>
            <Button className="mt-2" onClick={() => { setCreateName(''); setCreatedKey(null); setCreateOpen(true) }}><Plus /> 新建分组</Button>
          </Card>
        </motion.div>
      ) : (
        <Card className="overflow-hidden">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>ID</TableHead>
                <TableHead>名称</TableHead>
                <TableHead>KeyPrefix</TableHead>
                <TableHead>KeyHash</TableHead>
                <TableHead>创建时间</TableHead>
                <TableHead className="text-right">操作</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {(data ?? []).map(g => (
                <TableRow key={g.ID}>
                  <TableCell className="tabular-nums">{g.ID}</TableCell>
                  <TableCell className="max-w-36 truncate" title={g.Name}>{g.Name}</TableCell>
                  <TableCell className="font-mono text-xs">{g.KeyPrefix ?? '—'}</TableCell>
                  <TableCell className="max-w-32 truncate font-mono text-xs text-muted-foreground" title={g.KeyHash}>
                    {truncate(g.KeyHash, 24)}
                  </TableCell>
                  <TableCell className="text-xs text-muted-foreground">{formatDateTime(g.CreatedAt)}</TableCell>
                  <TableCell className="text-right">
                    <div className="flex justify-end gap-0.5">
                      <Button variant="ghost" size="icon-xs" title="编辑" onClick={() => { setEditTarget(g); setEditName(g.Name ?? '') }}><Pencil /></Button>
                      <Button variant="ghost" size="icon-xs" title="绑定账号" onClick={() => openBind(g)}><Link2 /></Button>
                      <Button variant="ghost" size="icon-xs" title="轮换 key" onClick={() => setRotateTarget(g)}><RefreshCw /></Button>
                      <Button variant="ghost" size="icon-xs" className="text-destructive" title="删除" onClick={() => setDeleting(g)}><Trash2 /></Button>
                    </div>
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </Card>
      )}

      {/* —— 创建分组：表单 → 明文 key 展示 —— */}
      <Dialog open={createOpen} onOpenChange={o => { setCreateOpen(o); if (!o) setCreatedKey(null) }}>
        <DialogContent className="sm:max-w-md">
          {createdKey ? (
            <>
              <DialogHeader>
                <DialogTitle>分组已创建</DialogTitle>
                <DialogDescription>明文 key 仅此一次展示，请立即复制保存</DialogDescription>
              </DialogHeader>
              <KeyBox
                title={createdKey.name ? `分组「${createdKey.name}」访问 key` : '访问 key'}
                value={createdKey.key}
                hint="格式 gk-…；丢失后只能通过「轮换 key」重新生成"
              />
              <DialogFooter>
                <Button onClick={() => { setCreateOpen(false); setCreatedKey(null) }}>完成</Button>
              </DialogFooter>
            </>
          ) : (
            <>
              <DialogHeader>
                <DialogTitle>新建分组</DialogTitle>
                <DialogDescription>创建后返回明文访问 key（仅此一次）</DialogDescription>
              </DialogHeader>
              <div className="space-y-3">
                <div className="space-y-1.5">
                  <Label htmlFor="grp-name">名称</Label>
                  <Input
                    id="grp-name"
                    value={createName}
                    placeholder="如 prod-web"
                    onChange={e => setCreateName(e.target.value)}
                    onKeyDown={e => { if (e.key === 'Enter' && createName.trim() && !create.isPending) create.mutate(createName.trim()) }}
                  />
                </div>
                {create.isError && errMsg(create.error) && (
                  <p className="text-sm text-destructive">{errMsg(create.error)}</p>
                )}
              </div>
              <DialogFooter>
                <Button variant="outline" onClick={() => setCreateOpen(false)} disabled={create.isPending}>取消</Button>
                <Button onClick={() => create.mutate(createName.trim())} disabled={create.isPending || !createName.trim()}>
                  {create.isPending ? '创建中…' : '创建'}
                </Button>
              </DialogFooter>
            </>
          )}
        </DialogContent>
      </Dialog>

      {/* —— 编辑（重命名） —— */}
      <Dialog open={!!editTarget} onOpenChange={o => { if (!o && !rename.isPending) setEditTarget(null) }}>
        <DialogContent className="sm:max-w-sm">
          <DialogHeader>
            <DialogTitle>编辑分组 # {editTarget?.ID}</DialogTitle>
          </DialogHeader>
          <div className="space-y-3">
            <div className="space-y-1.5">
              <Label htmlFor="grp-edit-name">名称</Label>
              <Input id="grp-edit-name" value={editName} onChange={e => setEditName(e.target.value)} />
            </div>
            {rename.isError && errMsg(rename.error) && (
              <p className="text-sm text-destructive">{errMsg(rename.error)}</p>
            )}
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setEditTarget(null)} disabled={rename.isPending}>取消</Button>
            <Button onClick={() => rename.mutate()} disabled={rename.isPending || !editName.trim()}>
              {rename.isPending ? '保存中…' : '保存'}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* —— 绑定账号 —— */}
      <Dialog open={!!bindTarget} onOpenChange={o => { if (!o && !bind.isPending) setBindTarget(null) }}>
        <DialogContent className="sm:max-w-md">
          <DialogHeader>
            <DialogTitle>绑定账号 — {bindTarget?.Name}</DialogTitle>
            <DialogDescription>
              全量重选绑定集合；已选 {bindChecked.length} 个，未勾选即从该分组解绑
            </DialogDescription>
          </DialogHeader>
          <div className="max-h-72 space-y-1 overflow-y-auto rounded-lg border p-2">
            {accounts.length === 0 ? (
              <p className="py-4 text-center text-sm text-muted-foreground">暂无账号，请先到「账号」页创建</p>
            ) : (
              accounts.map(a => (
                <label
                  key={a.ID}
                  className="flex cursor-pointer items-center gap-2.5 rounded-md px-2 py-1.5 hover:bg-muted"
                >
                  <Checkbox checked={bindChecked.includes(a.ID!)} onCheckedChange={() => toggleChecked(a.ID!)} />
                  <span className="flex-1 truncate text-sm">{a.Name}</span>
                  <span className="max-w-32 truncate text-xs text-muted-foreground">{a.Template?.Name ?? `#${a.TemplateID}`}</span>
                  <StatusBadge status={a.Status} />
                </label>
              ))
            )}
          </div>
          {bind.isError && errMsg(bind.error) && (
            <p className="text-sm text-destructive">{errMsg(bind.error)}</p>
          )}
          <DialogFooter>
            <Button variant="outline" onClick={() => setBindTarget(null)} disabled={bind.isPending}>取消</Button>
            <Button onClick={() => bind.mutate()} disabled={bind.isPending}>
              {bind.isPending ? '保存中…' : `保存绑定（${bindChecked.length}）`}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* —— 轮换 key 确认 —— */}
      <Dialog open={!!rotateTarget} onOpenChange={o => { if (!o && !rotate.isPending) setRotateTarget(null) }}>
        <DialogContent className="sm:max-w-sm">
          <DialogHeader>
            <DialogTitle>轮换 key</DialogTitle>
            <DialogDescription>
              确认轮换分组「{rotateTarget?.Name}」的访问 key？旧 key 立即失效，新 key 仅展示一次。
            </DialogDescription>
          </DialogHeader>
          {rotate.isError && errMsg(rotate.error) && (
            <p className="text-sm text-destructive">{errMsg(rotate.error)}</p>
          )}
          <DialogFooter>
            <Button variant="outline" onClick={() => setRotateTarget(null)} disabled={rotate.isPending}>取消</Button>
            <Button onClick={() => rotateTarget && rotate.mutate(rotateTarget.ID!)} disabled={rotate.isPending}>
              {rotate.isPending ? '轮换中…' : '确认轮换'}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* —— 轮换结果：明文 key —— */}
      <Dialog open={!!rotateResult} onOpenChange={o => { if (!o) setRotateResult(null) }}>
        <DialogContent className="sm:max-w-md">
          <DialogHeader>
            <DialogTitle>key 已轮换</DialogTitle>
            <DialogDescription>新明文 key 仅此一次展示，请立即复制保存</DialogDescription>
          </DialogHeader>
          <KeyBox
            title={rotateResult?.name ? `分组「${rotateResult.name}」新 key` : '新访问 key'}
            value={rotateResult?.key ?? ''}
            hint="旧 key 已失效"
          />
          <DialogFooter>
            <Button onClick={() => setRotateResult(null)}>完成</Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* —— 删除确认 —— */}
      <Dialog open={!!deleting} onOpenChange={o => { if (!o && !remove.isPending) setDeleting(null) }}>
        <DialogContent className="sm:max-w-sm">
          <DialogHeader>
            <DialogTitle>删除分组</DialogTitle>
            <DialogDescription>
              确认删除分组「{deleting?.Name}」？其访问 key 将立即失效。
            </DialogDescription>
          </DialogHeader>
          {remove.isError && errMsg(remove.error) && (
            <p className="text-sm text-destructive">{errMsg(remove.error)}</p>
          )}
          <DialogFooter>
            <Button variant="outline" onClick={() => setDeleting(null)} disabled={remove.isPending}>取消</Button>
            <Button variant="destructive" onClick={() => deleting && remove.mutate(deleting.ID!)} disabled={remove.isPending}>
              {remove.isPending ? '删除中…' : '确认删除'}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  )
}
