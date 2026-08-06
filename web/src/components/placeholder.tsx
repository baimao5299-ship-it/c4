import { Construction } from 'lucide-react'

// Task 2 占位：Task 3/4 实现各页面时整体替换。
export function Placeholder({ title }: { title: string }) {
  return (
    <div className="flex min-h-[60vh] items-center justify-center">
      <div className="space-y-2 text-center text-muted-foreground">
        <Construction className="mx-auto h-10 w-10" />
        <p className="text-lg font-medium">{title}</p>
        <p className="text-sm">建设中</p>
      </div>
    </div>
  )
}
