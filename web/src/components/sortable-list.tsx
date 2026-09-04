import type { CSSProperties, ReactNode } from 'react'
import {
  closestCenter,
  DndContext,
  KeyboardSensor,
  PointerSensor,
  useSensor,
  useSensors,
  type DragEndEvent,
  type DraggableAttributes,
  type DraggableSyntheticListeners,
} from '@dnd-kit/core'
import {
  arrayMove,
  sortableKeyboardCoordinates,
  SortableContext,
  useSortable,
  verticalListSortingStrategy,
} from '@dnd-kit/sortable'
import { CSS } from '@dnd-kit/utilities'
import { GripVertical } from 'lucide-react'
import { cn } from '@/lib/utils'

type SortableListProps = {
  ids: number[]
  disabled?: boolean
  onReorder: (ids: number[]) => void
  children: ReactNode
}

export function SortableList({ ids, disabled = false, onReorder, children }: SortableListProps) {
  const sensors = useSensors(
    useSensor(PointerSensor, { activationConstraint: { distance: 6 } }),
    useSensor(KeyboardSensor, { coordinateGetter: sortableKeyboardCoordinates }),
  )
  const onDragEnd = ({ active, over }: DragEndEvent) => {
    if (disabled || over == null || active.id === over.id) return
    const from = ids.indexOf(Number(active.id))
    const to = ids.indexOf(Number(over.id))
    if (from < 0 || to < 0) return
    onReorder(arrayMove(ids, from, to))
  }
  return (
    <DndContext sensors={sensors} collisionDetection={closestCenter} onDragEnd={onDragEnd}>
      <SortableContext items={ids} strategy={verticalListSortingStrategy} disabled={disabled}>
        {children}
      </SortableContext>
    </DndContext>
  )
}

type SortableItemRenderProps = {
  setNodeRef: (node: HTMLElement | null) => void
  style: CSSProperties
  isDragging: boolean
  handleAttributes: DraggableAttributes
  handleListeners: DraggableSyntheticListeners
}

export function SortableItem({ id, disabled = false, children }: {
  id: number
  disabled?: boolean
  children: (props: SortableItemRenderProps) => ReactNode
}) {
  const { attributes, listeners, setNodeRef, transform, transition, isDragging } = useSortable({ id, disabled })
  const style: CSSProperties = {
    transform: CSS.Transform.toString(transform),
    transition,
    position: 'relative',
    zIndex: isDragging ? 20 : undefined,
    opacity: isDragging ? 0.82 : undefined,
  }
  return children({ setNodeRef, style, isDragging, handleAttributes: attributes, handleListeners: listeners })
}

export function SortableHandle({ attributes, listeners, disabled, label, className }: {
  attributes: DraggableAttributes
  listeners: DraggableSyntheticListeners
  disabled?: boolean
  label: string
  className?: string
}) {
  return (
    <button
      type="button"
      className={cn('inline-flex size-11 shrink-0 touch-none items-center justify-center rounded-md text-muted-foreground transition-colors hover:bg-muted hover:text-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring disabled:cursor-not-allowed disabled:opacity-35 md:size-9', className)}
      aria-label={label}
      title={label}
      disabled={disabled}
      {...attributes}
      {...listeners}
    >
      <GripVertical className="size-5" />
    </button>
  )
}

export function SortableOrderPanel({ items, disabled, saving, label, hint, dragLabel, onReorder }: {
  items: Array<{ id: number; label: string; detail?: string }>
  disabled?: boolean
  saving?: boolean
  label: string
  hint: string
  dragLabel: (name: string) => string
  onReorder: (ids: number[]) => void
}) {
  const ids = items.map(item => item.id)
  return (
    <div className="overflow-hidden rounded-lg border bg-card" aria-label={label}>
      <div className="border-b bg-muted/35 px-3 py-2.5">
        <div className="flex items-center justify-between gap-3">
          <p className="text-sm font-medium">{label}</p>
          {saving && <span className="text-xs text-muted-foreground" aria-live="polite">{hint}</span>}
        </div>
        {!saving && <p className="mt-0.5 text-xs text-muted-foreground">{hint}</p>}
      </div>
      <SortableList ids={ids} disabled={disabled || saving} onReorder={onReorder}>
        <div className="divide-y">
          {items.map(item => (
            <SortableItem key={item.id} id={item.id} disabled={disabled || saving}>
              {({ setNodeRef, style, isDragging, handleAttributes, handleListeners }) => (
                <div ref={setNodeRef} style={style} className={cn('flex min-h-14 items-center gap-2 bg-card px-2 py-1.5', isDragging && 'shadow-lg ring-1 ring-primary/30')}>
                  <SortableHandle attributes={handleAttributes} listeners={handleListeners} disabled={disabled || saving} label={dragLabel(item.label)} />
                  <div className="min-w-0">
                    <p className="truncate text-sm font-medium" title={item.label}>{item.label}</p>
                    {item.detail && <p className="truncate text-xs text-muted-foreground" title={item.detail}>{item.detail}</p>}
                  </div>
                </div>
              )}
            </SortableItem>
          ))}
        </div>
      </SortableList>
    </div>
  )
}
